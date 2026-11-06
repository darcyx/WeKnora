package agent

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReActAnswerWaitsForTerminalPurpose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		end    types.StreamResponse
		answer bool
	}{
		{name: "natural answer", end: types.StreamResponse{FinishReason: "stop", Done: true}, answer: true},
		{name: "truncated answer", end: types.StreamResponse{FinishReason: "length", Done: true}, answer: true},
		{name: "late tool call", end: types.StreamResponse{FinishReason: "tool_calls", Done: true,
			ToolCalls: []types.LLMToolCall{{ID: "search-1", Type: "function", Function: types.FunctionCall{Name: "search_knowledge", Arguments: `{}`}}}}},
		{name: "stream failure", end: types.StreamResponse{ResponseType: types.ResponseTypeError, Content: "connection reset", Done: true}},
		{name: "content filter", end: types.StreamResponse{FinishReason: "content_filter", Done: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
				{ResponseType: types.ResponseTypeAnswer, Content: "  请提供订单号。  "},
				{ResponseType: types.ResponseTypeThinking, Content: "barrier"},
				tc.end,
			}}}}
			engine := newTestEngine(t, model)
			var published string
			engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
				published += evt.Data.(event.AgentFinalAnswerData).Content
				return nil
			})
			engine.eventBus.On(event.EventAgentThought, func(_ context.Context, evt event.Event) error {
				if evt.Data.(event.AgentThoughtData).Content == "barrier" {
					assert.Empty(t, published, "content must wait until late tool calls and stream failure are ruled out")
				}
				return nil
			})
			response, err := engine.streamThinkingToEventBus(t.Context(), emptyMessages(), emptyTools(), 0, "session")
			if tc.end.ResponseType == types.ResponseTypeError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.answer, response.AnswerStreamed)
			}
			if tc.answer {
				assert.Equal(t, response.Content, published, "published body must match the body used for final state and history")
				assert.Equal(t, "请提供订单号。", published)
			} else {
				assert.Empty(t, published, "internal or failed drafts must never appear in answer events")
			}
		})
	}
}
