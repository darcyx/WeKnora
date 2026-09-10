package service

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestFeedbackAdminRejectsNonAdminsBeforeQuery(t *testing.T) {
	s := &FeedbackAdminService{}
	for _, role := range []types.TenantRole{types.TenantRoleViewer, types.TenantRoleContributor} {
		ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
		ctx = types.WithCaller(ctx, types.Caller{TenantID: 1, Role: role})
		_, err := s.List(ctx, types.FeedbackQuery{})
		require.Error(t, err)
		_, err = s.Summary(ctx, types.FeedbackQuery{})
		require.Error(t, err)
		_, err = s.Detail(ctx, "faq", "s1", "1", "")
		require.Error(t, err)
		_, err = s.Export(ctx, types.FeedbackQuery{})
		require.Error(t, err)
	}
	_, err := feedbackAdminTenant(context.Background())
	require.Error(t, err)
}
func TestFeedbackAdminQueryValidation(t *testing.T) {
	q := types.FeedbackQuery{Page: 1, PageSize: 20, Reasons: []string{"other"}}
	require.NoError(t, validateFeedbackQuery(&q))
	require.Equal(t, "dislike", q.Vote)
	require.Equal(t, 7*24*time.Hour, q.To.Sub(q.From))
	for _, mutate := range []func(*types.FeedbackQuery){
		func(q *types.FeedbackQuery) { q.Source = "unknown" }, func(q *types.FeedbackQuery) { q.Vote = "like" },
		func(q *types.FeedbackQuery) { q.Reasons = []string{"made-up"} }, func(q *types.FeedbackQuery) { q.Page = -1 },
		func(q *types.FeedbackQuery) { q.PageSize = 10001 }, func(q *types.FeedbackQuery) { q.To = q.From },
		func(q *types.FeedbackQuery) { q.To = q.From.Add(367 * 24 * time.Hour) }, func(q *types.FeedbackQuery) { q.To = time.Time{} },
	} {
		copy := q
		mutate(&copy)
		require.Error(t, validateFeedbackQuery(&copy))
	}
}
