package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestMessageRetrievalStatePreservesMetadataAndSessionBoundary(t *testing.T) {
	repo, db := newMessageRepositoryForForkTest(t)
	now := time.Now().UTC()
	seedMessage(t, db, "old", "session", "assistant", now.Add(-time.Second))
	seedMessage(t, db, "current", "session", "assistant", now)
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", "old").Update("execution_context", types.MessageExecutionContext{Locale: "zh-CN"}).Error)
	stale, err := repo.GetMessage(t.Context(), "session", "old")
	require.NoError(t, err)
	raw := json.RawMessage(`{"policy":"p","state":{"version":3}}`)
	require.NoError(t, repo.UpdateMessageRetrievalState(t.Context(), "session", "old", raw))
	stale.Content = "answer"
	require.NoError(t, repo.UpdateMessage(t.Context(), stale))
	previous, err := repo.GetPreviousRetrievalState(t.Context(), "session", "current")
	require.NoError(t, err)
	require.Equal(t, "old", previous.ID)
	require.JSONEq(t, string(raw), string(previous.ExecutionContext.RetrievalState))
	require.Equal(t, "zh-CN", previous.ExecutionContext.Locale)
	require.Error(t, repo.UpdateMessageRetrievalState(t.Context(), "other-session", "old", raw))
	require.NoError(t, repo.DeleteMessage(t.Context(), "session", "old"))
	previous, err = repo.GetPreviousRetrievalState(t.Context(), "session", "current")
	require.NoError(t, err)
	require.Nil(t, previous)
}
