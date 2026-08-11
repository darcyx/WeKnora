package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

const (
	TokenQuotaDefaultDailyLimitSetting   = "token_quota.default_daily_limit"
	TokenQuotaDefaultMonthlyLimitSetting = "token_quota.default_monthly_limit"
	TokenQuotaMaxCompletionSetting       = "token_quota.max_completion_tokens"

	defaultTokenQuotaDailyLimit          int64 = 200_000_000
	defaultTokenQuotaMonthlyLimit        int64 = 200_000_000
	defaultTokenQuotaMaxCompletionTokens int64 = 4096
	tokenQuotaReservationTTL                   = 30 * time.Minute
)

var ErrTokenQuotaExceeded = types.ErrTokenQuotaExceeded

type tokenQuotaService struct {
	repo     interfaces.TokenQuotaRepository
	settings interfaces.TokenQuotaSettingsResolver
}

func NewTokenQuotaService(
	repo interfaces.TokenQuotaRepository,
	settings interfaces.TokenQuotaSettingsResolver,
) interfaces.TokenQuotaService {
	return &tokenQuotaService{repo: repo, settings: settings}
}

func (s *tokenQuotaService) Reserve(
	ctx context.Context,
	subjectID string,
	promptTokens, completionTokens int64,
) (*types.TokenQuotaReservation, error) {
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return nil, errors.New("token quota subject is required")
	}
	if promptTokens < 0 || completionTokens < 0 {
		return nil, errors.New("token quota tokens cannot be negative")
	}
	limits, err := s.resolveLimits(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	if limits.DailyTokenLimit == 0 && limits.MonthlyTokenLimit == 0 {
		return nil, nil
	}
	if completionTokens == 0 {
		completionTokens = s.maxCompletionTokens(ctx)
	}
	reservedTokens := promptTokens + completionTokens
	if reservedTokens <= 0 {
		return nil, fmt.Errorf("%w: reservation must be positive", ErrTokenQuotaExceeded)
	}

	now := time.Now().UTC()
	reservation := &types.TokenQuotaReservation{
		ID:             uuid.NewString(),
		SubjectID:      subjectID,
		DayStart:       quotaDayStart(now),
		MonthStart:     quotaMonthStart(now),
		PromptTokens:   promptTokens,
		ReservedTokens: reservedTokens,
		Status:         types.TokenQuotaReservationPending,
		ExpiresAt:      now.Add(tokenQuotaReservationTTL),
	}
	if err := s.repo.Reserve(ctx, reservation, limits); err != nil {
		return nil, err
	}
	return reservation, nil
}

func (s *tokenQuotaService) Settle(ctx context.Context, reservationID string, usage *types.TokenUsage) error {
	if strings.TrimSpace(reservationID) == "" || usage == nil {
		return nil
	}
	settled := *usage
	if settled.PromptTokens < 0 {
		settled.PromptTokens = 0
	}
	if settled.CompletionTokens < 0 {
		settled.CompletionTokens = 0
	}
	if settled.TotalTokens < settled.PromptTokens+settled.CompletionTokens {
		settled.TotalTokens = settled.PromptTokens + settled.CompletionTokens
	}
	return s.repo.Settle(ctx, reservationID, settled)
}

func (s *tokenQuotaService) Release(ctx context.Context, reservationID string) error {
	if strings.TrimSpace(reservationID) == "" {
		return nil
	}
	return s.repo.Release(ctx, reservationID)
}

func (s *tokenQuotaService) GetUserQuota(ctx context.Context, subjectID string) (*types.TokenQuotaUsageSnapshot, error) {
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return nil, errors.New("token quota subject is required")
	}
	limits, err := s.resolveLimits(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	override, err := s.repo.GetOverride(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	daily, err := s.repo.GetPeriodUsage(ctx, subjectID, types.TokenQuotaPeriodDay, quotaDayStart(now))
	if err != nil {
		return nil, err
	}
	monthly, err := s.repo.GetPeriodUsage(ctx, subjectID, types.TokenQuotaPeriodMonth, quotaMonthStart(now))
	if err != nil {
		return nil, err
	}
	return &types.TokenQuotaUsageSnapshot{
		SubjectID: subjectID,
		Limits:    limits,
		Override:  override,
		Daily:     daily,
		Monthly:   monthly,
	}, nil
}

func (s *tokenQuotaService) ListTenantUsers(
	ctx context.Context,
	tenantID uint64,
	page, pageSize int,
) (*types.TokenQuotaUserPage, error) {
	if page < 1 {
		return nil, errors.New("token quota page must be at least 1")
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, errors.New("token quota page size must be between 1 and 100")
	}
	prefix := strconv.FormatUint(tenantID, 10) + ":"
	now := time.Now().UTC()
	snapshots, total, err := s.repo.ListUserQuotaSnapshots(
		ctx,
		prefix,
		quotaDayStart(now),
		quotaMonthStart(now),
		pageSize,
		(page-1)*pageSize,
	)
	if err != nil {
		return nil, err
	}
	defaultLimits := s.defaultLimits(ctx)
	items := make([]types.TokenQuotaUser, 0, len(snapshots))
	for _, snapshot := range snapshots {
		snapshot.Limits = mergeTokenQuotaLimits(defaultLimits, snapshot.Override)
		items = append(items, types.TokenQuotaUser{
			ExternalUserID: strings.TrimPrefix(snapshot.SubjectID, prefix),
			Quota:          snapshot,
		})
	}
	return &types.TokenQuotaUserPage{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}

func (s *tokenQuotaService) UpsertUserOverride(ctx context.Context, override *types.TokenQuotaOverride) error {
	if override == nil {
		return errors.New("token quota override is required")
	}
	override.SubjectID = strings.TrimSpace(override.SubjectID)
	if override.SubjectID == "" {
		return errors.New("token quota subject is required")
	}
	for _, value := range []*int64{override.DailyTokenLimit, override.MonthlyTokenLimit} {
		if value != nil && *value < 0 {
			return errors.New("token quota limit cannot be negative")
		}
	}
	if override.DailyTokenLimit == nil && override.MonthlyTokenLimit == nil {
		return errors.New("at least one token quota limit is required")
	}
	return s.repo.UpsertOverride(ctx, override)
}

func (s *tokenQuotaService) DeleteUserOverride(ctx context.Context, subjectID string) error {
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return errors.New("token quota subject is required")
	}
	_, err := s.repo.DeleteOverride(ctx, subjectID)
	return err
}

func (s *tokenQuotaService) resolveLimits(ctx context.Context, subjectID string) (types.TokenQuotaLimits, error) {
	limits := s.defaultLimits(ctx)
	override, err := s.repo.GetOverride(ctx, subjectID)
	if err != nil {
		return limits, err
	}
	return mergeTokenQuotaLimits(limits, override), nil
}

func (s *tokenQuotaService) defaultLimits(ctx context.Context) types.TokenQuotaLimits {
	limits := types.TokenQuotaLimits{
		DailyTokenLimit: s.settings.GetInt(ctx, TokenQuotaDefaultDailyLimitSetting,
			"WEKNORA_TOKEN_QUOTA_DEFAULT_DAILY_LIMIT", defaultTokenQuotaDailyLimit),
		MonthlyTokenLimit: s.settings.GetInt(ctx, TokenQuotaDefaultMonthlyLimitSetting,
			"WEKNORA_TOKEN_QUOTA_DEFAULT_MONTHLY_LIMIT", defaultTokenQuotaMonthlyLimit),
	}
	if limits.DailyTokenLimit < 0 {
		limits.DailyTokenLimit = 0
	}
	if limits.MonthlyTokenLimit < 0 {
		limits.MonthlyTokenLimit = 0
	}
	return limits
}

func mergeTokenQuotaLimits(limits types.TokenQuotaLimits, override *types.TokenQuotaOverride) types.TokenQuotaLimits {
	if override == nil {
		return limits
	}
	if override.DailyTokenLimit != nil {
		limits.DailyTokenLimit = *override.DailyTokenLimit
	}
	if override.MonthlyTokenLimit != nil {
		limits.MonthlyTokenLimit = *override.MonthlyTokenLimit
	}
	return limits
}

func (s *tokenQuotaService) maxCompletionTokens(ctx context.Context) int64 {
	value := s.settings.GetInt(ctx, TokenQuotaMaxCompletionSetting,
		"WEKNORA_TOKEN_QUOTA_MAX_COMPLETION_TOKENS", defaultTokenQuotaMaxCompletionTokens)
	if value <= 0 {
		return defaultTokenQuotaMaxCompletionTokens
	}
	return value
}

func quotaDayStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func quotaMonthStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}
