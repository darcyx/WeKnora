package agent

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestFAQPreflightUsageSurvivesAgentFallback(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer, Content: "answer", Done: true, FinishReason: "stop",
	}}}}}
	engine := newTestEngine(t, model)
	engine.toolRegistry = tools.NewToolRegistry()
	engine.config.FAQPreflightUsage = &types.TokenUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}
	state, err := engine.Execute(context.Background(), "s", "m", "question", nil, nil)
	require.NoError(t, err)
	require.GreaterOrEqual(t, state.TurnUsage.TotalTokens, 7)
	require.GreaterOrEqual(t, state.Usage.TotalTokens, 7)
}
