package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type historyRetryRepo struct {
	historyRepo
	failures int
	calls    int
	err      error
	cancel   context.CancelFunc
}

func (r *historyRetryRepo) ListMessagesBySessionBeforeCursor(ctx context.Context, sessionID string, before time.Time, beforeID string, limit int) ([]*types.Message, error) {
	r.calls++
	if r.calls <= r.failures {
		if r.cancel != nil && r.calls == r.failures {
			r.cancel()
		}
		return nil, r.err
	}
	return r.historyRepo.ListMessagesBySessionBeforeCursor(ctx, sessionID, before, beforeID, limit)
}

func TestAgentQAHistoryRetriesOnceAndStopsWithoutHistory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		cancel   bool
		calls    int
		wantRun  bool
	}{
		{"success", 0, false, 1, true},
		{"transient failure", 1, false, 2, true},
		{"persistent failure", 2, false, 2, false},
		{"canceled after first failure", 1, true, 1, false},
		{"canceled after retry failure", 2, true, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1)))
			defer cancel()
			cause := errors.New("database unavailable")
			repo := &historyRetryRepo{historyRepo: historyRepo{rows: storedTurns(1)}, failures: tc.failures, err: cause}
			if tc.cancel {
				repo.cancel = cancel
			}
			engine := &retrievalInputEngine{}
			s := &sessionService{cfg: &config.Config{}, messageRepo: repo, agentService: &retrievalInputService{engine: engine},
				modelService: &stubModelService{chatModel: &captureChatModel{}, modelsByID: map[string]*types.Model{
					"model": {ID: "model", Type: types.ModelTypeKnowledgeQA},
				}}}
			req := &types.QARequest{Session: &types.Session{ID: "session", TenantID: 1}, TurnLeaseHeld: true,
				Query: "那安卓呢？", CustomAgent: &types.CustomAgent{ID: "agent", TenantID: 1, Config: types.CustomAgentConfig{
					ModelID: "model", KBSelectionMode: "none", SkillsSelectionMode: "none", MultiTurnEnabled: true,
					WebSearchProviderID: "unused", AllowedTools: []string{},
				}}}
			err := s.AgentQA(ctx, req, event.NewEventBus())
			if tc.wantRun {
				require.NoError(t, err)
				require.Equal(t, []string{"question 1"}, questions(engine.history))
			} else if tc.cancel {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, cause)
				require.ErrorContains(t, err, "please retry")
			}
			require.Equal(t, tc.calls, repo.calls)
			require.Equal(t, tc.wantRun, engine.called)
		})
	}
}
