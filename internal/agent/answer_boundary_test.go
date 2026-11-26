package agent

import (
	"context"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestReActAnswerStreamsBeforeTerminalChunk(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "答案"},
		{ResponseType: types.ResponseTypeThinking, Content: "barrier"},
		{ResponseType: types.ResponseTypeAnswer, Content: "。", Done: true, FinishReason: "stop"},
	}}}}
	engine := newTestEngine(t, model)
	var published string
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		published += evt.Data.(event.AgentFinalAnswerData).Content
		return nil
	})
	engine.eventBus.On(event.EventAgentThought, func(_ context.Context, evt event.Event) error {
		if evt.Data.(event.AgentThoughtData).Content == "barrier" {
			require.Equal(t, "答案", published, "body must arrive before the terminal chunk")
		}
		return nil
	})
	response, err := engine.streamThinkingToEventBus(t.Context(), emptyMessages(), emptyTools(), 0, "session")
	require.NoError(t, err)
	require.True(t, response.AnswerStreamed)
	require.Equal(t, "答案。", published)
	require.Equal(t, response.Content, published)
}

func TestContentFilterDoesNotRepeatStreamedBody(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "partial answer"},
		{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "content_filter"},
	}}}}
	engine := newTestEngine(t, model)
	recorder := &truncationRecorder{}
	recorder.attach(engine.eventBus)
	state, err := engine.executeLoop(t.Context(), &types.AgentState{}, "question", emptyMessages(), emptyTools(), "session", "message")
	require.NoError(t, err)
	var published string
	for _, data := range recorder.snapshot() {
		published += data.Content
	}
	require.Equal(t, "partial answer", state.FinalAnswer)
	require.Equal(t, state.FinalAnswer, published)
}

func TestFilteredToolCallDoesNotExecute(t *testing.T) {
	filtered := planToolCall("count")
	filtered.chunks[0].FinishReason = "content_filter"
	filtered.chunks[0].ToolCalls[0].Function.Arguments = "filtered arguments"
	filtered.chunks = append([]types.StreamResponse{
		{ResponseType: types.ResponseTypeToolCall, Data: map[string]any{"tool_call_id": "call-1", "tool_name": "count"}},
		{ResponseType: types.ResponseTypeToolCall, Data: map[string]any{"tool_call_id": "partial", "tool_name": "count"}},
	}, filtered.chunks...)
	model := &mockChat{responses: []mockResponse{filtered, plannerResponse("unexpected retry")}}
	engine := newTestEngine(t, model)
	engine.toolRegistry = agenttools.NewToolRegistry()
	tool := newCountingTool("count")
	engine.toolRegistry.RegisterTool(tool)
	var ended []string
	engine.eventBus.On(event.EventAgentToolResult, func(_ context.Context, evt event.Event) error {
		result := evt.Data.(event.AgentToolResultData)
		require.False(t, result.Success)
		ended = append(ended, result.ToolCallID)
		return nil
	})
	state, err := engine.Execute(t.Context(), "s", "m", "question", nil)
	require.NoError(t, err)
	require.Zero(t, tool.calls)
	require.Len(t, model.calls, 1)
	require.ElementsMatch(t, []string{"call-1", "partial"}, ended)
	require.Len(t, state.RoundSteps, 1)
	require.Len(t, state.RoundSteps[0].ToolCalls, 2)
	for _, call := range state.RoundSteps[0].ToolCalls {
		require.Empty(t, call.Args)
		require.False(t, call.Result.Success)
	}
	require.NotEmpty(t, state.FinalAnswer)
}
