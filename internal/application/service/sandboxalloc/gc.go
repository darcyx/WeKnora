package sandboxalloc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Tencent/WeKnora/internal/common/redislock"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/sandbox/allocation"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	UserMachineGCLockKey   = "weknora:sandbox:user-gc:lock"
	UserMachineGCInterval  = time.Hour
	UserMachineGCCursorKey = "weknora:sandbox:user-gc:cursor"
	gcLockLease            = 5 * time.Minute
	gcBatch                = 200
	gcMaxPerRun            = 2000
)

type GCReport struct {
	Scanned, Reclaimed, Busy, Failed int
	CleanupsDone, CleanupsFailed     int
	Skipped                          bool
}

type UserMachineGC struct {
	svc     *Service
	cleaner *WorkspaceCleaner
	configs ConfigReader
	redis   redis.UniversalClient
	now     func() time.Time
	batch   int
	maxRun  int
	cursor  interfaces.EmptyMachineCursor
}

func NewUserMachineGC(
	svc *Service, cleaner *WorkspaceCleaner, configs ConfigReader, client redis.UniversalClient,
) *UserMachineGC {
	return &UserMachineGC{
		svc: svc, cleaner: cleaner, configs: configs, redis: client,
		now: func() time.Time { return time.Now().UTC() }, batch: gcBatch, maxRun: gcMaxPerRun,
	}
}

func (g *UserMachineGC) RunOnce(ctx context.Context) (GCReport, error) {
	if g.redis == nil {
		return g.scan(ctx)
	}
	token, err := redislock.NewToken()
	if err != nil {
		return GCReport{}, err
	}
	ok, err := redislock.TryAcquire(ctx, g.redis, UserMachineGCLockKey, token, gcLockLease)
	if err != nil {
		return GCReport{}, err
	}
	if !ok {
		return GCReport{Skipped: true}, nil
	}
	scanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(gcLockLease / 3)
		defer t.Stop()
		for {
			select {
			case <-scanCtx.Done():
				return
			case <-t.C:
				renewed, err := redislock.Renew(scanCtx, g.redis, UserMachineGCLockKey, token, gcLockLease)
				if err != nil || !renewed {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer relCancel()
		_, _ = redislock.Release(relCtx, g.redis, UserMachineGCLockKey, token)
	}()
	return g.scan(scanCtx)
}

func (g *UserMachineGC) scan(ctx context.Context) (GCReport, error) {
	var report GCReport
	report.CleanupsDone, report.CleanupsFailed, _ = g.cleaner.RetryDue(ctx, gcBatch)

	cursor := g.loadCursor(ctx)
	emptyBefore := g.now().Add(-24 * time.Hour)
	for report.Scanned < g.maxRun {
		rows, err := g.svc.repo.ListEmptyUserMachines(ctx, emptyBefore, cursor, g.batch)
		if err != nil {
			return report, err
		}
		if len(rows) == 0 {
			cursor = interfaces.EmptyMachineCursor{}
			break
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				g.saveCursor(ctx, cursor)
				return report, ctx.Err()
			}
			if report.Scanned >= g.maxRun {
				break
			}
			cursor = interfaces.EmptyMachineCursor{EmptySince: *row.EmptySince, Digest: row.MachineDigest}
			g.reclaimCandidate(ctx, row, &report)
		}
	}
	g.saveCursor(ctx, cursor)
	logger.Infof(ctx, "[sandbox-gc] scanned=%d reclaimed=%d busy=%d failed=%d cleanups_done=%d cleanups_failed=%d",
		report.Scanned, report.Reclaimed, report.Busy, report.Failed, report.CleanupsDone, report.CleanupsFailed)
	return report, nil
}

func (g *UserMachineGC) reclaimCandidate(ctx context.Context, row *types.SandboxUserMachine, report *GCReport) {
	report.Scanned++
	m := machineOf(row)
	digest := row.MachineDigest
	ops, err := g.cleaner.reclaimOps(ctx, m)
	if err != nil {
		report.Failed++
		logger.Warnf(ctx, "[sandbox-gc] resolve config %d/%s for machine %s: %v",
			row.ConfigTenantID, row.ConfigID, digestPrefix(digest), err)
		return
	}
	days, err := g.retentionDays(ctx, row.ConfigTenantID, row.ConfigID)
	if err != nil {
		report.Failed++
		logger.Warnf(ctx, "[sandbox-gc] retention for machine %s: %v", digestPrefix(digest), err)
		return
	}
	var lock interfaces.MachineReclaimLock
	ok, err := ops.ReclaimUserMachine(ctx, m, func(_ context.Context) (bool, error) {
		var lockErr error
		lock, lockErr = g.svc.repo.LockEmptyMachine(ctx, digest)
		if lockErr != nil || lock == nil {
			return false, lockErr
		}
		due := !g.now().Before(lock.Claim().EmptySince.Add(time.Duration(days) * 24 * time.Hour))
		if !due {
			lock.Abort()
			lock = nil
			return false, nil
		}
		return true, nil
	})
	if lock != nil {
		defer lock.Abort()
	}
	switch {
	case err != nil:
		report.Failed++
		logger.Warnf(ctx, "[sandbox-gc] reclaim machine %s: %v", digestPrefix(digest), err)
	case !ok:
		report.Busy++
	case lock == nil:
		report.Failed++
		logger.Warnf(ctx, "[sandbox-gc] reclaim machine %s: manager skipped the reclaim check",
			digestPrefix(digest))
	default:
		deleted, delErr := lock.CommitDelete()
		if delErr != nil {
			report.Failed++
			logger.Warnf(ctx, "[sandbox-gc] drop registry row %s: %v", digestPrefix(digest), delErr)
			return
		}
		if !deleted {
			report.Busy++
			return
		}
		report.Reclaimed++
	}
}

type gcCursorJSON struct {
	EmptySince time.Time `json:"empty_since"`
	Digest     string    `json:"digest"`
}

func (g *UserMachineGC) loadCursor(ctx context.Context) interfaces.EmptyMachineCursor {
	if g.redis == nil {
		return g.cursor
	}
	raw, err := g.redis.Get(ctx, UserMachineGCCursorKey).Bytes()
	if err != nil {
		return interfaces.EmptyMachineCursor{}
	}
	var c gcCursorJSON
	if json.Unmarshal(raw, &c) != nil {
		return interfaces.EmptyMachineCursor{}
	}
	return interfaces.EmptyMachineCursor{EmptySince: c.EmptySince, Digest: c.Digest}
}

func (g *UserMachineGC) saveCursor(ctx context.Context, c interfaces.EmptyMachineCursor) {
	if g.redis == nil {
		g.cursor = c
		return
	}
	if c.EmptySince.IsZero() {
		_ = g.redis.Del(context.WithoutCancel(ctx), UserMachineGCCursorKey).Err()
		return
	}
	raw, _ := json.Marshal(gcCursorJSON{EmptySince: c.EmptySince, Digest: c.Digest})
	_ = g.redis.Set(context.WithoutCancel(ctx), UserMachineGCCursorKey, raw, 2*UserMachineGCInterval).Err()
}

func (g *UserMachineGC) retentionDays(ctx context.Context, tenantID uint64, configID string) (int, error) {
	days := allocation.DefaultEmptyRetentionDays
	if g.configs == nil {
		return days, nil
	}
	entity, err := g.configs.GetByID(ctx, tenantID, configID)
	if err != nil {
		return 0, err
	}
	if entity == nil || entity.Config == nil {
		return 0, fmt.Errorf("sandboxalloc: sandbox config %d/%s not found", tenantID, configID)
	}
	return allocation.EffectiveRetentionDays(entity.Config.UserSandboxEmptyRetentionDays), nil
}

func digestPrefix(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
