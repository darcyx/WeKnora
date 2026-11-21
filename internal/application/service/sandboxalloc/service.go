package sandboxalloc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/sandbox/allocation"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

type ConfigReader interface {
	GetByID(ctx context.Context, tenantID uint64, id string) (*types.TenantSandboxConfigEntity, error)
}

type SessionReader interface {
	GetByID(ctx context.Context, tenantID uint64, id string) (*types.Session, error)
}

type Service struct {
	repo      interfaces.SandboxAllocationRepository
	configs   ConfigReader
	sessions  SessionReader
	namespace string
	now       func() time.Time
}

var ErrMachineUnregistered = errors.New("sandboxalloc: shared machine is not registered")

func New(
	repo interfaces.SandboxAllocationRepository, configs ConfigReader,
	sessions SessionReader, namespace string,
) *Service {
	return &Service{
		repo: repo, configs: configs, sessions: sessions, namespace: namespace,
		now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
	}
}

func (s *Service) Namespace() string { return s.namespace }

func (s *Service) Digest(m allocation.UserMachine) string { return m.Digest(s.namespace) }

func (s *Service) Repo() interfaces.SandboxAllocationRepository { return s.repo }

func (s *Service) Now() time.Time { return s.now() }

type PlaceRequest struct {
	SessionTenantID uint64
	SessionID       string
	ConfigTenantID  uint64
	ConfigID        string
	Purpose         allocation.Purpose
}

const placeAttempts = 3

func (s *Service) Place(ctx context.Context, req PlaceRequest) (allocation.Assignment, error) {
	a, _, err := s.place(ctx, req)
	return a, err
}

func (s *Service) place(
	ctx context.Context, req PlaceRequest,
) (allocation.Assignment, allocation.FallbackReason, error) {
	for attempt := 0; attempt < placeAttempts; attempt++ {
		existing, ok, err := s.Lookup(ctx, req.SessionID)
		if err != nil {
			return allocation.Assignment{}, "", err
		}
		if ok && existing.Released() {
			return allocation.Assignment{}, "", fmt.Errorf("sandboxalloc: session %s is being deleted: %w",
				req.SessionID, sandbox.ErrSandboxSessionDeleted)
		}
		if ok {
			return existing, allocation.ReasonNone, nil
		}
		sess, err := s.sessions.GetByID(ctx, req.SessionTenantID, req.SessionID)
		if err != nil {
			return allocation.Assignment{}, "", fmt.Errorf("sandboxalloc: load session %s: %w", req.SessionID, err)
		}
		if sess == nil {
			return allocation.Assignment{}, "", fmt.Errorf("sandboxalloc: session %s not found", req.SessionID)
		}
		pin := interfaces.PlacementPin{ConfigID: sess.SandboxConfigID, TenantID: sess.SandboxConfigTenantID}
		a, reason, err := s.decide(ctx, req, sess, pin)
		if err != nil {
			return allocation.Assignment{}, "", err
		}
		row, machine := ToEntity(a, s.digestOf(a))
		winner, err := s.repo.ClaimPlacement(ctx, row, machine, pin)
		if errors.Is(err, interfaces.ErrPlacementConflict) {
			continue
		}
		if errors.Is(err, interfaces.ErrSessionRetired) {
			return allocation.Assignment{}, "", fmt.Errorf("sandboxalloc: session %s is being deleted: %w",
				req.SessionID, sandbox.ErrSandboxSessionDeleted)
		}
		if err != nil {
			return allocation.Assignment{}, "", fmt.Errorf("sandboxalloc: record placement: %w", err)
		}
		if winner.ConfigTenantID != row.ConfigTenantID || winner.ConfigID != row.ConfigID ||
			winner.MachineDigest != row.MachineDigest || winner.ReleasedAt != nil {
			got, _, err := s.Lookup(ctx, req.SessionID)
			return got, allocation.ReasonNone, err
		}
		logger.Infof(ctx, "[sandbox-alloc] session=%s mode=%s reason=%s config=%d/%s",
			a.SessionID, a.Mode(), reason, a.ConfigTenantID, a.ConfigID)
		return a, reason, nil
	}
	return allocation.Assignment{}, "", fmt.Errorf("sandboxalloc: placement of %s kept conflicting", req.SessionID)
}

func (s *Service) digestOf(a allocation.Assignment) string {
	m, ok := a.UserMachine()
	if !ok {
		return ""
	}
	return s.Digest(m)
}

func (s *Service) decide(
	ctx context.Context, req PlaceRequest, sess *types.Session, pin interfaces.PlacementPin,
) (allocation.Assignment, allocation.FallbackReason, error) {
	purpose := req.Purpose
	if purpose == "" {
		purpose = allocation.PurposeChat
	}
	if types.IsSkillMaintenanceDescription(sess.Description) {
		purpose = allocation.PurposeMaintenance
	}
	configID, configTenant := req.ConfigID, req.ConfigTenantID
	if pin.ConfigID != "" {
		configID = pin.ConfigID
		if pin.TenantID != 0 {
			configTenant = pin.TenantID
		} else if req.ConfigID != pin.ConfigID {
			configTenant = 0
		}
	}
	if configTenant == 0 {
		configTenant = sess.TenantID
	}
	configID = sandbox.NormalizeConfigID(configID)
	owner := ownerFromSession(sess)

	legacy := pin.ConfigID != "" || (sess.ForkBootstrap != nil && !sess.ForkBootstrap.Consumed())
	if !legacy {
		has, err := s.repo.HasSandboxCheckpoint(ctx, sess.ID)
		if err != nil {
			return allocation.Assignment{}, "", fmt.Errorf("sandboxalloc: check history of %s: %w", sess.ID, err)
		}
		legacy = has
	}
	decision := allocation.Decision{Mode: allocation.ModeSession, Reason: allocation.ReasonLegacySessionSandbox}
	if !legacy {
		mode, err := s.configMode(ctx, configTenant, configID)
		if err != nil {
			return allocation.Assignment{}, "", err
		}
		decision = allocation.Decide(allocation.DecideInput{
			ConfigMode:        mode,
			Purpose:           purpose,
			Owner:             owner,
			SessionID:         sess.ID,
			UserModeAvailable: allocation.UserModeAvailable(),
		})
	}
	a := allocation.Assignment{SessionID: sess.ID, ConfigTenantID: configTenant, ConfigID: configID}
	if decision.Mode != allocation.ModeUser {
		return a, decision.Reason, nil
	}
	a.Machine = allocation.UserMachine{
		OwnerTenantID: sess.TenantID, OwnerID: sess.UserID,
		ConfigTenantID: configTenant, ConfigID: configID,
	}
	if err := a.Machine.Validate(); err != nil {
		return allocation.Assignment{}, "", fmt.Errorf("sandboxalloc: %w", err)
	}
	return a, decision.Reason, nil
}

func (s *Service) configMode(ctx context.Context, tenantID uint64, configID string) (allocation.Mode, error) {
	if s.configs == nil || configID == "" || configID == types.SandboxConfigIDGlobalDefault {
		return allocation.ModeSession, nil
	}
	entity, err := s.configs.GetByID(ctx, tenantID, configID)
	if err != nil {
		return "", fmt.Errorf("sandboxalloc: read config %d/%s: %w", tenantID, configID, err)
	}
	if entity == nil || entity.Config == nil {
		return allocation.ModeSession, nil
	}
	mode, err := allocation.ParseMode(entity.Config.AllocationMode)
	if err != nil {
		return allocation.ModeSession, nil
	}
	return mode, nil
}

func (s *Service) Lookup(ctx context.Context, sessionID string) (allocation.Assignment, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return allocation.Assignment{}, false, nil
	}
	row, err := s.repo.GetAssignment(ctx, sessionID)
	if err != nil {
		return allocation.Assignment{}, false, fmt.Errorf("sandboxalloc: read assignment %s: %w", sessionID, err)
	}
	if row == nil || row.IsTombstone() {
		return allocation.Assignment{}, false, nil
	}
	var machine *types.SandboxUserMachine
	if row.MachineDigest != "" {
		machine, err = s.repo.GetUserMachine(ctx, row.MachineDigest)
		if err != nil {
			return allocation.Assignment{}, false, fmt.Errorf("sandboxalloc: read machine of %s: %w", sessionID, err)
		}
		if machine == nil {
			return allocation.Assignment{}, false, fmt.Errorf("%w: session %s", ErrMachineUnregistered, sessionID)
		}
	}
	return FromEntity(row, machine), true, nil
}

func (s *Service) Retire(ctx context.Context, sessionIDs []string) ([]interfaces.RetiredSession, error) {
	out, err := s.repo.RetireSessions(ctx, sessionIDs, s.now())
	if err != nil {
		return nil, fmt.Errorf("sandboxalloc: retire sessions: %w", err)
	}
	return out, nil
}

func (s *Service) Release(ctx context.Context, sessionID string) (interfaces.RetiredSession, error) {
	if strings.TrimSpace(sessionID) == "" {
		return interfaces.RetiredSession{}, errors.New("sandboxalloc: release needs a session id")
	}
	out, err := s.Retire(ctx, []string{sessionID})
	if err != nil {
		return interfaces.RetiredSession{}, err
	}
	if len(out) != 1 {
		return interfaces.RetiredSession{}, fmt.Errorf(
			"sandboxalloc: retiring %s returned %d results", sessionID, len(out),
		)
	}
	return out[0], nil
}

func (s *Service) SessionAllocation(ctx context.Context, sessionID string) (allocation.Assignment, bool, error) {
	return s.Lookup(ctx, sessionID)
}

func (s *Service) VerifySessionAllocation(
	ctx context.Context, sessionTenantID uint64, sessionID string, expected allocation.Assignment,
) error {
	row, err := s.repo.GetAssignment(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("sandboxalloc: read assignment %s: %w", sessionID, err)
	}
	if row == nil || row.ReleasedAt != nil {
		return sandbox.ErrSandboxSessionDeleted
	}
	if row.ConfigTenantID != expected.ConfigTenantID ||
		sandbox.NormalizeConfigID(row.ConfigID) != sandbox.NormalizeConfigID(expected.ConfigID) ||
		row.MachineDigest != s.digestOf(expected) {
		return fmt.Errorf("%w: session %s was re-addressed", sandbox.ErrUserAllocationRejected, sessionID)
	}
	sess, err := s.sessions.GetByID(ctx, sessionTenantID, sessionID)
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, apperrors.ErrSessionNotFound) ||
		(err == nil && sess == nil) {
		return sandbox.ErrSandboxSessionDeleted
	}
	if err != nil {
		return fmt.Errorf("sandboxalloc: read session %s: %w", sessionID, err)
	}
	if row.MachineDigest == "" {
		return nil
	}
	current := allocation.UserMachine{
		OwnerTenantID: sess.TenantID, OwnerID: sess.UserID,
		ConfigTenantID: row.ConfigTenantID, ConfigID: row.ConfigID,
	}
	if s.Digest(current) != row.MachineDigest {
		return fmt.Errorf("%w: session %s no longer belongs to the machine's owner",
			sandbox.ErrUserAllocationRejected, sessionID)
	}
	owner := ownerFromSession(sess)
	if !allocation.EligibleForUser(owner.Kind, owner.Source) {
		return sandbox.ErrUserAllocationRejected
	}
	return nil
}

func (s *Service) UserMachinesByConfig(
	ctx context.Context, configTenantID uint64, configID string,
) ([]allocation.UserMachine, error) {
	rows, err := s.repo.ListUserMachinesByConfig(ctx, configTenantID, configID)
	if err != nil {
		return nil, err
	}
	out := make([]allocation.UserMachine, 0, len(rows))
	for _, r := range rows {
		out = append(out, allocation.UserMachine{
			OwnerTenantID: r.OwnerTenantID, OwnerID: r.OwnerID,
			ConfigTenantID: r.ConfigTenantID, ConfigID: r.ConfigID,
		})
	}
	return out, nil
}

var _ sandbox.AllocationDirectory = (*Service)(nil)

func (s *Service) ForkAssignment(source allocation.Assignment, targetSessionID string) allocation.Assignment {
	child := source
	child.SessionID = targetSessionID
	child.ReleasedAt = nil
	return child
}
