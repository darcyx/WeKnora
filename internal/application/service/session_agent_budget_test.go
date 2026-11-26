package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestBuildAgentConfigPreservesRetrievalAndOutputBudgets(t *testing.T) {
	svc := &sessionService{cfg: &config.Config{}, webSearchProviderRepo: &sharedAgentWebSearchRepo{}}
	for _, budget := range []int{0, 6000} {
		req := &types.QARequest{
			Session: &types.Session{ID: "session", TenantID: 1},
			CustomAgent: &types.CustomAgent{TenantID: 1, Config: types.CustomAgentConfig{
				AgentMode:     types.AgentModeSmartReasoning,
				EmbeddingTopK: 20, MaxToolOutputChars: budget,
			}},
		}
		cfg, err := svc.buildAgentConfig(t.Context(), req, &types.Tenant{ID: 1}, 1)
		require.NoError(t, err)
		require.Equal(t, 20, cfg.EmbeddingTopK)
		require.Equal(t, budget, cfg.MaxToolOutputChars)
	}
}
