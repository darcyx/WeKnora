package sandboxalloc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/sandbox/allocation"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type UserMachineOps interface {
	DeleteSessionWorkspaces(ctx context.Context, machine allocation.UserMachine, sessionIDs []string) ([]string, error)
	ReclaimUserMachine(ctx context.Context, machine allocation.UserMachine,
		check func(context.Context) (bool, error)) (bool, error)
}

func cleanupBackoff(releasedAt *time.Time, now time.Time) time.Duration {
	d := time.Minute
	if releasedAt != nil {
		d = now.Sub(*releasedAt)
	}
	return min(max(d, time.Minute), 4*time.Hour)
}

type WorkspaceCleaner struct {
	svc      *Service
	resolver sandbox.TenantSandboxResolver
	now      func() time.Time
}

func NewWorkspaceCleaner(svc *Service, resolver sandbox.TenantSandboxResolver) *WorkspaceCleaner {
	return &WorkspaceCleaner{svc: svc, resolver: resolver, now: svc.Now}
}

type sessionDirectoryDeleter interface {
	DeleteSessionWorkspaces(ctx context.Context, machine allocation.UserMachine, sessionIDs []string) ([]string, error)
}

func (c *WorkspaceCleaner) machineOps(ctx context.Context, m allocation.UserMachine) (sessionDirectoryDeleter, error) {
	if c.resolver == nil {
		return nil, errors.New("sandboxalloc: no sandbox resolver")
	}
	mgr, err := c.resolver.Resolve(types.WithSandboxTenantID(ctx, m.OwnerTenantID), m.ConfigTenantID, m.ConfigID)
	if err != nil {
		return nil, err
	}
	ops, ok := mgr.(sessionDirectoryDeleter)
	if !ok {
		return nil, fmt.Errorf("sandboxalloc: config %d/%s does not serve user machines", m.ConfigTenantID, m.ConfigID)
	}
	return ops, nil
}

type userMachineReclaimer interface {
	ReclaimUserMachine(
		ctx context.Context, machine allocation.UserMachine, check func(context.Context) (bool, error),
	) (bool, error)
}

func (c *WorkspaceCleaner) reclaimOps(ctx context.Context, m allocation.UserMachine) (userMachineReclaimer, error) {
	ops, err := c.machineOps(ctx, m)
	if err != nil {
		return nil, err
	}
	reclaimer, ok := ops.(userMachineReclaimer)
	if !ok {
		return nil, fmt.Errorf("sandboxalloc: config %d/%s does not reclaim user machines",
			m.ConfigTenantID, m.ConfigID)
	}
	return reclaimer, nil
}

func (c *WorkspaceCleaner) ReleaseSessions(ctx context.Context, sessionIDs []string) (map[string]bool, error) {
	out, err := c.svc.Retire(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	handled := make(map[string]bool, len(out))
	for _, r := range out {
		if r.Assignment != nil && r.Assignment.MachineDigest != "" {
			handled[r.SessionID] = true
		}
	}
	return handled, nil
}

func (c *WorkspaceCleaner) UserSessions(ctx context.Context, ids []string) (map[string]bool, error) {
	handled := make(map[string]bool)
	for _, id := range ids {
		row, err := c.svc.repo.GetAssignment(ctx, id)
		if err != nil {
			return nil, err
		}
		if row != nil && row.MachineDigest != "" {
			handled[id] = true
		}
	}
	return handled, nil
}

func (c *WorkspaceCleaner) DeleteSessions(
	ctx context.Context, tenantID uint64, idsByOwner map[string][]string,
) (map[string]bool, int, error) {
	retired, err := c.svc.repo.DeleteSessionsRetiring(ctx, tenantID, idsByOwner, c.now())
	if err != nil {
		return nil, 0, err
	}
	handled := make(map[string]bool)
	for _, r := range retired {
		if r.Assignment != nil && r.Assignment.MachineDigest != "" {
			handled[r.SessionID] = true
		}
	}
	return handled, len(retired), nil
}

func (c *WorkspaceCleaner) RunCleanups(ctx context.Context, sessionIDs []string) {
	rows := make([]*types.SessionSandboxAssignment, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		row, err := c.svc.repo.GetAssignment(ctx, id)
		if err != nil {
			logger.Warnf(ctx, "[sandbox-alloc] load cleanup of %s: %v", id, err)
			continue
		}
		if row != nil && row.CleanupDueAt != nil && row.MachineDigest != "" {
			rows = append(rows, row)
		}
	}
	c.execute(ctx, rows)
}

func (c *WorkspaceCleaner) RetryDue(ctx context.Context, limit int) (int, int, error) {
	rows, err := c.svc.repo.ListDueWorkspaceCleanups(ctx, c.now(), limit)
	if err != nil {
		return 0, 0, err
	}
	done, failed := c.execute(ctx, rows)
	return done, failed, nil
}

func (c *WorkspaceCleaner) execute(ctx context.Context, rows []*types.SessionSandboxAssignment) (int, int) {
	batches := map[string][]*types.SessionSandboxAssignment{}
	for _, row := range rows {
		batches[row.MachineDigest] = append(batches[row.MachineDigest], row)
	}
	done, failed := 0, 0
	retry := func(row *types.SessionSandboxAssignment, reason string) {
		failed++
		logger.Warnf(ctx, "[sandbox-alloc] clean workspace of %s: %s", row.SessionID, reason)
		_ = c.svc.repo.RetryWorkspaceCleanup(ctx, row.SessionID, c.now().Add(cleanupBackoff(row.ReleasedAt, c.now())))
	}
	for digest, rows := range batches {
		reg, err := c.svc.repo.GetUserMachine(ctx, digest)
		if err != nil {
			for _, row := range rows {
				retry(row, err.Error())
			}
			continue
		}
		if reg == nil {
			for _, row := range rows {
				_ = c.svc.repo.CompleteWorkspaceCleanup(ctx, row.SessionID)
				done++
			}
			continue
		}
		b := struct {
			machine allocation.UserMachine
			rows    []*types.SessionSandboxAssignment
		}{machineOf(reg), rows}
		ids := make([]string, 0, len(b.rows))
		for _, row := range b.rows {
			ids = append(ids, row.SessionID)
		}
		var deleted []string
		ops, err := c.machineOps(ctx, b.machine)
		if err == nil {
			deleted, err = ops.DeleteSessionWorkspaces(ctx, b.machine, ids)
		}
		gone := make(map[string]bool, len(deleted))
		for _, id := range deleted {
			gone[id] = true
		}
		reason := "session operations still registered"
		if err != nil {
			reason = err.Error()
		}
		for _, row := range b.rows {
			if gone[row.SessionID] {
				_ = c.svc.repo.CompleteWorkspaceCleanup(ctx, row.SessionID)
				done++
				continue
			}
			retry(row, reason)
		}
	}
	return done, failed
}

func (c *WorkspaceCleaner) ReleaseOwnerAssignments(ctx context.Context, ownerID string) error {
	_, err := c.svc.repo.RetireOwnerSessions(ctx, ownerID, c.now())
	return err
}

func (c *WorkspaceCleaner) ReclaimOwner(ctx context.Context, ownerID string) {
	machines, err := c.OwnerMachines(ctx, 0, ownerID)
	if err != nil {
		logger.Warnf(ctx, "[sandbox-alloc] list machines of %s: %v", ownerID, err)
		return
	}
	c.ReclaimOwnerMachines(ctx, machines)
}

func (c *WorkspaceCleaner) OwnerMachines(
	ctx context.Context, ownerTenantID uint64, ownerID string,
) ([]allocation.UserMachine, error) {
	rows, err := c.svc.repo.ListUserMachinesByOwner(ctx, ownerTenantID, ownerID)
	if err != nil {
		return nil, err
	}
	out := make([]allocation.UserMachine, 0, len(rows))
	for _, r := range rows {
		out = append(out, machineOf(r))
	}
	return out, nil
}

func (c *WorkspaceCleaner) ReleaseMachine(ctx context.Context, m allocation.UserMachine) (bool, error) {
	return c.destroyIfEmpty(ctx, m)
}

func (c *WorkspaceCleaner) ReclaimOwnerMachines(ctx context.Context, machines []allocation.UserMachine) {
	for _, m := range machines {
		if _, err := c.destroyIfEmpty(ctx, m); err != nil {
			logger.Warnf(ctx, "[sandbox-alloc] release machine %s: %v", digestPrefix(c.svc.Digest(m)), err)
		}
	}
}

func (c *WorkspaceCleaner) destroyIfEmpty(ctx context.Context, m allocation.UserMachine) (bool, error) {
	ops, err := c.reclaimOps(ctx, m)
	if err != nil {
		return false, err
	}
	digest := c.svc.Digest(m)
	var lock interfaces.MachineReclaimLock
	ok, err := ops.ReclaimUserMachine(ctx, m, func(_ context.Context) (bool, error) {
		var lockErr error
		lock, lockErr = c.svc.repo.LockEmptyMachine(ctx, digest)
		if lockErr != nil || lock == nil {
			return false, lockErr
		}
		return true, nil
	})
	if lock != nil {
		defer lock.Abort()
	}
	if err != nil || !ok {
		return false, err
	}
	if lock == nil {
		return false, fmt.Errorf(
			"sandboxalloc: manager of config %d/%s skipped the reclaim check",
			m.ConfigTenantID, m.ConfigID)
	}
	deleted, delErr := lock.CommitDelete()
	if delErr != nil {
		logger.Warnf(ctx, "[sandbox-alloc] drop registry row %s: %v", digestPrefix(digest), delErr)
		return false, delErr
	}
	return deleted, nil
}

func machineOf(reg *types.SandboxUserMachine) allocation.UserMachine {
	return allocation.UserMachine{
		OwnerTenantID: reg.OwnerTenantID, OwnerID: reg.OwnerID,
		ConfigTenantID: reg.ConfigTenantID, ConfigID: reg.ConfigID,
	}
}
