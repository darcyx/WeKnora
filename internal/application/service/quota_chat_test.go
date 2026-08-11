package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type quotaStreamChat struct {
	options *chat.ChatOptions
	usage   *types.TokenUsage
}

func (c *quotaStreamChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return nil, nil
}

func (c *quotaStreamChat) ChatStream(
	_ context.Context,
	_ []chat.Message,
	opts *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	c.options = opts
	responses := make(chan types.StreamResponse, 2)
	responses <- types.StreamResponse{Content: "answer"}
	usage := c.usage
	if usage == nil {
		usage = &types.TokenUsage{
			PromptTokens:     12,
			CompletionTokens: 8,
			TotalTokens:      20,
		}
	}
	responses <- types.StreamResponse{Done: true, Usage: usage}
	close(responses)
	return responses, nil
}

func TestQuotaChatChargesReservationWhenStreamUsageIsUnavailable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota_stream_without_usage?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))
	quota := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{
			TokenQuotaDefaultDailyLimitSetting:   1_000,
			TokenQuotaDefaultMonthlyLimitSetting: 1_000,
		}},
	)
	upstream := &quotaStreamChat{usage: &types.TokenUsage{}}
	wrapped := newQuotaChat(upstream, quota)
	ctx := types.WithPrincipal(context.Background(), types.Principal{
		Type: types.PrincipalAPIExternalUser,
		ID:   "7:external-user-without-usage",
	})
	messages := []chat.Message{{Role: "user", Content: "hello"}}
	opts := &chat.ChatOptions{MaxTokens: 32}

	stream, err := wrapped.ChatStream(ctx, messages, opts)
	require.NoError(t, err)
	for range stream {
	}

	snapshot, err := quota.GetUserQuota(context.Background(), "7:external-user-without-usage")
	require.NoError(t, err)
	require.NotNil(t, snapshot.Daily)
	require.EqualValues(t, estimateQuotaPromptTokens(messages, opts)+32, snapshot.Daily.TotalTokens)
	require.Zero(t, snapshot.Daily.ReservedTokens)
}

func (c *quotaStreamChat) GetModelName() string { return "test" }
func (c *quotaStreamChat) GetModelID() string   { return "test-model" }

type blockingQuotaStreamChat struct{}

func (c *blockingQuotaStreamChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return nil, nil
}

func (c *blockingQuotaStreamChat) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return make(chan types.StreamResponse), nil
}

func (c *blockingQuotaStreamChat) GetModelName() string { return "test" }
func (c *blockingQuotaStreamChat) GetModelID() string   { return "test-model" }

func TestQuotaChatChargesReservationWhenCallerCancelsStream(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota_cancelled_stream?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))
	quota := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{
			TokenQuotaDefaultDailyLimitSetting:   1_000,
			TokenQuotaDefaultMonthlyLimitSetting: 1_000,
		}},
	)
	wrapped := newQuotaChat(&blockingQuotaStreamChat{}, quota)
	ctx, cancel := context.WithCancel(types.WithPrincipal(context.Background(), types.Principal{
		Type: types.PrincipalAPIExternalUser,
		ID:   "7:external-user-cancelled-stream",
	}))
	messages := []chat.Message{{Role: "user", Content: "hello"}}
	opts := &chat.ChatOptions{MaxTokens: 32}

	stream, err := wrapped.ChatStream(ctx, messages, opts)
	require.NoError(t, err)
	cancel()
	for range stream {
	}

	snapshot, err := quota.GetUserQuota(context.Background(), "7:external-user-cancelled-stream")
	require.NoError(t, err)
	require.NotNil(t, snapshot.Daily)
	require.EqualValues(t, estimateQuotaPromptTokens(messages, opts)+32, snapshot.Daily.TotalTokens)
	require.Zero(t, snapshot.Daily.ReservedTokens)
}

// Two workspaces that happen to issue the same external user ID must not share
// a budget: otherwise one workspace could exhaust the other's users by sending
// an unverified X-External-User-ID header.
func TestQuotaChatKeepsQuotaSeparatePerTenantForSameExternalUserID(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota_tenant_scope?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))
	quota := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{
			TokenQuotaDefaultDailyLimitSetting:   1_000,
			TokenQuotaDefaultMonthlyLimitSetting: 1_000,
		}},
	)
	wrapped := newQuotaChat(&quotaStreamChat{}, quota)

	for _, principalID := range []string{"7:alice", "8:alice"} {
		ctx := types.WithPrincipal(context.Background(), types.Principal{
			Type: types.PrincipalAPIExternalUser,
			ID:   principalID,
		})
		stream, err := wrapped.ChatStream(ctx, []chat.Message{{Role: "user", Content: "hello"}}, &chat.ChatOptions{MaxTokens: 32})
		require.NoError(t, err)
		for range stream {
		}
	}

	for _, subjectID := range []string{"7:alice", "8:alice"} {
		snapshot, err := quota.GetUserQuota(context.Background(), subjectID)
		require.NoError(t, err)
		require.NotNil(t, snapshot.Daily, "expected an isolated usage row for %s", subjectID)
		require.EqualValues(t, 20, snapshot.Daily.TotalTokens, "usage for %s must not include the other tenant", subjectID)
	}

	// The bare external user ID is never an accounting key on its own.
	bare, err := quota.GetUserQuota(context.Background(), "alice")
	require.NoError(t, err)
	require.Nil(t, bare.Daily)
}

// failingSettleQuota reserves normally but never settles, standing in for a
// transient database failure during settlement.
type failingSettleQuota struct {
	interfaces.TokenQuotaService
	settleErr error
}

func (q *failingSettleQuota) Settle(context.Context, string, *types.TokenUsage) error {
	return q.settleErr
}

type usageReportingChat struct{ response *types.ChatResponse }

func (c *usageReportingChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return c.response, nil
}

func (c *usageReportingChat) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, nil
}

func (c *usageReportingChat) GetModelName() string { return "test" }
func (c *usageReportingChat) GetModelID() string   { return "test-model" }

// The provider has already produced (and billed) the answer by the time
// settlement runs, so a settlement failure must not turn a successful call into
// an error response.
func TestQuotaChatReturnsResponseWhenSettlementFails(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota_settle_failure?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))
	quota := &failingSettleQuota{
		TokenQuotaService: NewTokenQuotaService(
			repository.NewTokenQuotaRepository(db),
			tokenQuotaTestSettings{values: map[string]int64{
				TokenQuotaDefaultDailyLimitSetting:   1_000,
				TokenQuotaDefaultMonthlyLimitSetting: 1_000,
			}},
		),
		settleErr: errors.New("database unavailable"),
	}
	expected := &types.ChatResponse{
		Content: "answer",
		Usage:   types.TokenUsage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20},
	}
	wrapped := newQuotaChat(&usageReportingChat{response: expected}, quota)
	ctx := types.WithPrincipal(context.Background(), types.Principal{
		Type: types.PrincipalAPIExternalUser,
		ID:   "7:settle-failure-user",
	})

	response, err := wrapped.Chat(ctx, []chat.Message{{Role: "user", Content: "hello"}}, &chat.ChatOptions{MaxTokens: 32})
	require.NoError(t, err)
	require.Same(t, expected, response)
}

func TestQuotaChatSettlesUsageForAPIExternalUser(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:token_quota_stream?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.TokenQuotaOverride{},
		&types.TokenQuotaPeriodUsage{},
		&types.TokenQuotaReservation{},
	))
	quota := NewTokenQuotaService(
		repository.NewTokenQuotaRepository(db),
		tokenQuotaTestSettings{values: map[string]int64{
			TokenQuotaDefaultDailyLimitSetting:   1_000,
			TokenQuotaDefaultMonthlyLimitSetting: 1_000,
		}},
	)
	upstream := &quotaStreamChat{}
	wrapped := newQuotaChat(upstream, quota)
	ctx := types.WithPrincipal(context.Background(), types.Principal{
		Type: types.PrincipalAPIExternalUser,
		ID:   "7:external-user-1",
	})

	stream, err := wrapped.ChatStream(ctx, []chat.Message{{Role: "user", Content: "hello"}}, &chat.ChatOptions{MaxTokens: 32})
	require.NoError(t, err)
	for range stream {
	}

	require.NotNil(t, upstream.options)
	require.Equal(t, 32, upstream.options.MaxTokens)
	snapshot, err := quota.GetUserQuota(context.Background(), "7:external-user-1")
	require.NoError(t, err)
	require.NotNil(t, snapshot.Daily)
	require.EqualValues(t, 20, snapshot.Daily.TotalTokens)
	require.Zero(t, snapshot.Daily.ReservedTokens)
}
