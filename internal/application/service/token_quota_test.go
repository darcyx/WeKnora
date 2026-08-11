package service

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type tokenQuotaTestSettings struct {
	values map[string]int64
}

func (s tokenQuotaTestSettings) GetInt(_ context.Context, key, _ string, def int64) int64 {
	if value, ok := s.values[key]; ok {
		return value
	}
	return def
}

type tokenQuotaQueryCounter struct {
	logger.Interface
	count int
}

func (l *tokenQuotaQueryCounter) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	l.count++
	l.Interface.Trace(ctx, begin, fc, err)
}

func TestTokenQuotaServiceReservesAndSettlesGlobalExternalUserUsage(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))

	svc := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{
			TokenQuotaDefaultDailyLimitSetting:   100,
			TokenQuotaDefaultMonthlyLimitSetting: 150,
			TokenQuotaMaxCompletionSetting:       80,
		}},
	)
	ctx := context.Background()
	dailyLimit := int64(30)

	reservation, err := svc.Reserve(ctx, "external-user-1", 30, 50)
	require.NoError(t, err)
	require.EqualValues(t, 80, reservation.ReservedTokens)

	_, err = svc.Reserve(ctx, "external-user-1", 30, 50)
	require.ErrorIs(t, err, ErrTokenQuotaExceeded)

	require.NoError(t, svc.Settle(ctx, reservation.ID, &types.TokenUsage{
		PromptTokens:     20,
		CompletionTokens: 20,
		TotalTokens:      40,
	}))

	// Settlement returns unused reservation tokens, so the next request is
	// checked against 40 used tokens rather than the original 80-token hold.
	second, err := svc.Reserve(ctx, "external-user-1", 10, 50)
	require.NoError(t, err)
	require.EqualValues(t, 60, second.ReservedTokens)
	require.NoError(t, svc.Release(ctx, second.ID))

	// A per-user override takes precedence over the platform default. The
	// subject is opaque to the service; callers pass the tenant-scoped key
	// built by types.TokenQuotaSubject.
	require.NoError(t, svc.UpsertUserOverride(ctx, &types.TokenQuotaOverride{
		SubjectID:       "external-user-2",
		DailyTokenLimit: &dailyLimit,
	}))
	_, err = svc.Reserve(ctx, "external-user-2", 30, 30)
	require.ErrorIs(t, err, ErrTokenQuotaExceeded)
}

func TestTokenQuotaServiceCapsDefaultCompletionToAvailableQuota(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota_low_limit?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))
	svc := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{
			TokenQuotaDefaultDailyLimitSetting:   500,
			TokenQuotaDefaultMonthlyLimitSetting: 500,
			TokenQuotaMaxCompletionSetting:       4_096,
		}},
	)

	reservation, err := svc.Reserve(context.Background(), "low-limit-user", 100, 0)
	require.NoError(t, err)
	require.EqualValues(t, 500, reservation.ReservedTokens)
}

func TestTokenQuotaServiceUsesBuiltInDefaultLimits(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota_builtin_limits?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))

	svc := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{}},
	)

	snapshot, err := svc.GetUserQuota(context.Background(), "fresh-external-user")
	require.NoError(t, err)
	require.EqualValues(t, 200_000_000, snapshot.Limits.DailyTokenLimit)
	require.EqualValues(t, 200_000_000, snapshot.Limits.MonthlyTokenLimit)
}

func TestTokenQuotaServiceListsObservedUsersWithinTenant(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota_tenant_users?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))
	now := time.Now().UTC()
	daily := quotaDayStart(now)
	require.NoError(t, db.Create(&types.TokenQuotaPeriodUsage{
		SubjectID:   types.TokenQuotaSubject(7, "alice"),
		Period:      types.TokenQuotaPeriodDay,
		PeriodStart: daily,
		TotalTokens: 123,
	}).Error)
	require.NoError(t, db.Create(&types.TokenQuotaPeriodUsage{
		SubjectID:   types.TokenQuotaSubject(7, "bob"),
		Period:      types.TokenQuotaPeriodDay,
		PeriodStart: daily,
		TotalTokens: 456,
	}).Error)
	require.NoError(t, db.Create(&types.TokenQuotaPeriodUsage{
		SubjectID:   types.TokenQuotaSubject(8, "alice"),
		Period:      types.TokenQuotaPeriodDay,
		PeriodStart: daily,
		TotalTokens: 789,
	}).Error)
	require.NoError(t, db.Create(&types.TokenQuotaOverride{
		SubjectID: types.TokenQuotaSubject(7, "carol"),
	}).Error)

	svc := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{}},
	)
	page, err := svc.ListTenantUsers(context.Background(), 7, 1, 2)
	require.NoError(t, err)
	require.EqualValues(t, 3, page.Total)
	require.Equal(t, 1, page.Page)
	require.Equal(t, 2, page.PageSize)
	require.Equal(t, []string{"alice", "bob"}, []string{
		page.Items[0].ExternalUserID,
		page.Items[1].ExternalUserID,
	})
	require.EqualValues(t, 123, page.Items[0].Quota.Daily.TotalTokens)
}

func TestTokenQuotaServiceListsTenantUsersInBoundedQueries(t *testing.T) {
	queryCounter := &tokenQuotaQueryCounter{Interface: logger.Default.LogMode(logger.Silent)}
	db, err := gorm.Open(sqlite.Open("file:token_quota_tenant_users_query_count?mode=memory&cache=shared"), &gorm.Config{
		Logger: queryCounter,
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))
	now := time.Now().UTC()
	for _, externalUserID := range []string{"alice", "bob", "carol"} {
		require.NoError(t, db.Create(&types.TokenQuotaPeriodUsage{
			SubjectID:   types.TokenQuotaSubject(7, externalUserID),
			Period:      types.TokenQuotaPeriodDay,
			PeriodStart: quotaDayStart(now),
		}).Error)
	}
	queryCounter.count = 0

	svc := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{}},
	)
	page, err := svc.ListTenantUsers(context.Background(), 7, 1, 3)

	require.NoError(t, err)
	require.Len(t, page.Items, 3)
	require.LessOrEqual(t, queryCounter.count, 4)
}
