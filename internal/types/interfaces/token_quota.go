package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// TokenQuotaSettingsResolver is the narrow part of SystemSettingService used
// by token accounting. Keeping it small makes the quota service testable and
// prevents it from depending on management CRUD methods.
type TokenQuotaSettingsResolver interface {
	GetInt(ctx context.Context, key string, envName string, def int64) int64
}

type TokenQuotaRepository interface {
	GetOverride(ctx context.Context, subjectID string) (*types.TokenQuotaOverride, error)
	UpsertOverride(ctx context.Context, override *types.TokenQuotaOverride) error
	DeleteOverride(ctx context.Context, subjectID string) (bool, error)
	GetPeriodUsage(ctx context.Context, subjectID, period string, periodStart time.Time) (*types.TokenQuotaPeriodUsage, error)
	ListUserQuotaSnapshots(
		ctx context.Context,
		subjectPrefix string,
		dayStart, monthStart time.Time,
		limit, offset int,
	) ([]*types.TokenQuotaUsageSnapshot, int64, error)
	Reserve(ctx context.Context, reservation *types.TokenQuotaReservation, limits types.TokenQuotaLimits) error
	Settle(ctx context.Context, reservationID string, usage types.TokenUsage) error
	Release(ctx context.Context, reservationID string) error
	ReleaseExpired(ctx context.Context, subjectID string, before time.Time) error
}

// TokenQuotaService is the application boundary used by model-call wrappers
// and SystemAdmin management endpoints.
type TokenQuotaService interface {
	Reserve(ctx context.Context, subjectID string, promptTokens, completionTokens int64) (*types.TokenQuotaReservation, error)
	Settle(ctx context.Context, reservationID string, usage *types.TokenUsage) error
	Release(ctx context.Context, reservationID string) error
	GetUserQuota(ctx context.Context, subjectID string) (*types.TokenQuotaUsageSnapshot, error)
	ListTenantUsers(ctx context.Context, tenantID uint64, page, pageSize int) (*types.TokenQuotaUserPage, error)
	UpsertUserOverride(ctx context.Context, override *types.TokenQuotaOverride) error
	DeleteUserOverride(ctx context.Context, subjectID string) error
}
