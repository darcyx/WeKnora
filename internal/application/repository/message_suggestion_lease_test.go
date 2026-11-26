package repository

import (
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestSuggestionSaveRequiresCurrentGenerationLease(t *testing.T) {
	_, db := newMessageRepositoryForForkTest(t)
	require.NoError(t, db.AutoMigrate(&types.MessageSuggestionSet{}))
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX suggestion_test_cache ON message_suggestion_sets
		(tenant_id, assistant_message_id, placement, config_hash, locale)`).Error)
	repo := &messageSuggestionRepository{db: db}
	candidate := func() *types.MessageSuggestionSet {
		return &types.MessageSuggestionSet{TenantID: 1, SessionID: "session", AssistantMessageID: "answer",
			Placement: types.SuggestionPlacementAfterAnswer, ConfigHash: "config", Locale: "zh-CN"}
	}
	first, acquired, err := repo.AcquireGeneration(t.Context(), candidate(), false)
	require.NoError(t, err)
	require.True(t, acquired)
	// Simulate a worker acquired in the past, with its original persisted lease.
	expired := time.Now().Add(-time.Minute).Truncate(time.Second)
	first.LeaseUntil = &expired
	require.NoError(t, db.Model(&types.MessageSuggestionSet{}).Where("id = ?", first.ID).Update("lease_until", expired).Error)
	second, acquired, err := repo.AcquireGeneration(t.Context(), candidate(), false)
	require.NoError(t, err)
	require.True(t, acquired)
	first.Status = types.SuggestionStatusReady
	first.Questions = types.SuggestionItems{{ID: "stale", Text: "stale answer"}}
	require.Error(t, repo.Save(t.Context(), first), "an expired worker cannot overwrite a reclaimed generation")
	second.Status = types.SuggestionStatusReady
	second.Questions = types.SuggestionItems{{ID: "current", Text: "current answer"}}
	require.NoError(t, repo.Save(t.Context(), second))
	require.Nil(t, second.LeaseUntil)
	stored, err := repo.GetByID(t.Context(), 1, "session", first.ID)
	require.NoError(t, err)
	require.Equal(t, second.Questions, stored.Questions)

	third, acquired, err := repo.AcquireGeneration(t.Context(), candidate(), true)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, repo.DeleteByMessageID(t.Context(), 1, "session", "answer"))
	third.Status = types.SuggestionStatusReady
	require.Error(t, repo.Save(t.Context(), third), "a late result must never upsert deleted suggestions")
	var count int64
	require.NoError(t, db.Model(&types.MessageSuggestionSet{}).Count(&count).Error)
	require.Zero(t, count)
}
