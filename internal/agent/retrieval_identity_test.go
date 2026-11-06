package agent

import (
	"context"
	"encoding/json"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func executeRetrievalIdentityTurn(t *testing.T, e *AgentEngine, turn string, history []chat.Message, calls ...types.LLMToolCall) *types.AgentState {
	t.Helper()
	return executeRetrievalTurn(t, e, turn, "TapTap sandbox configuration", history, calls...)
}

func executeRetrievalTurn(t *testing.T, e *AgentEngine, turn, query string, history []chat.Message, calls ...types.LLMToolCall) *types.AgentState {
	t.Helper()
	model := &mockChat{}
	for _, call := range calls {
		model.responses = append(model.responses, mockResponse{chunks: []types.StreamResponse{{
			ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{call},
		}}})
	}
	model.responses = append(model.responses, mockResponse{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer, Content: "The remaining information is needed.", Done: true, FinishReason: "stop",
	}}})
	e.chatModel = model
	state, err := e.Execute(t.Context(), "session", turn, query, history)
	require.NoError(t, err)
	return state
}

func retrievalIdentityCall(id, tool, args string) types.LLMToolCall {
	return types.LLMToolCall{ID: id, Function: types.FunctionCall{Name: tool, Arguments: args}}
}

func TestRetrievalIdentityRepeatedCompletedCallReachesBoundedStop(t *testing.T) {
	e, searches := newRetrievalProbeEngine(t)
	call := retrievalIdentityCall("call_1", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","retrieval":{"goal_quote":"TapTap","fact":"configuration","action":"initial"}}`)
	state := executeRetrievalIdentityTurn(t, e, "turn-a", nil, call, call, call)
	require.True(t, state.IsComplete)
	require.Len(t, searches.calls, 1, "a completed invocation cannot execute again")
	model := e.chatModel.(*mockChat)
	require.Len(t, model.opts, 4)
	require.Equal(t, "none", model.opts[3].ToolChoice, "duplicate IDs must reach the stop path, not masquerade as newly observed calls")
}

func TestRetrievalIdentityScopesOriginalRecoveryCall(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	probe.err = context.DeadlineExceeded
	require.False(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, partialRetrievalInitial).Result.Success)
	probe.err = nil
	require.True(t, retrievalProbeCall(e, "retry", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","retrieval":{"gap_id":"gap-1","action":"recovery"}}`).Result.Success)
	saved := e.retrieval.snapshot()
	e.retrievalTurnID = "known-owner"
	e.scopeLegacyRetrievalSnapshot(&saved)
	_, err := restoreRetrievalControl(saved)
	require.NoError(t, err)
	require.Equal(t, saved.Attempts[0].ID, saved.Attempts[1].RecoveryCallID)
	require.True(t, saved.Gaps[0].RecoveryUsed)
}

func TestRetrievalIdentityResumeKeepsOwnerAndDoesNotReplayCompletedSearch(t *testing.T) {
	first, searches := newRetrievalProbeEngine(t)
	asker := &retrievalAsker{}
	first.toolRegistry.RegisterTool(agenttools.NewAskUserTool(asker))
	first.SetRunMode(types.RunModeAttended)
	first.EnableSuspension(json.RawMessage(`{}`))
	initial := retrievalIdentityCall("call_1", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","retrieval":{"goal_quote":"TapTap","fact":"configuration","action":"initial"}}`)
	state := executeRetrievalIdentityTurn(t, first, "turn-a", nil, initial,
		retrievalIdentityCall("ask", agenttools.ToolAskUser, `{"question":"Which platform?"}`))
	require.NotNil(t, state.Suspended)
	require.Len(t, searches.calls, 1)
	ref := first.retrieval.snapshot().Attempts[0].ID
	answered := &types.AgentPrompt{ID: "p1", Kind: types.AgentPromptKindAskUser, ToolCallID: "ask",
		Status: types.AgentPromptAnswered, Response: types.JSON(`{"text":"iOS"}`)}

	wrong, wrongSearches := newRetrievalProbeEngine(t)
	wrong.toolRegistry.RegisterTool(agenttools.NewAskUserTool(asker))
	_, err := wrong.Resume(t.Context(), "session", "different-message", nil, asker.snapshot, answered)
	require.ErrorContains(t, err, "different assistant message")
	require.Empty(t, wrongSearches.calls)
	require.Zero(t, asker.resumes)

	next, nextSearches := newRetrievalProbeEngine(t)
	next.toolRegistry.RegisterTool(agenttools.NewAskUserTool(asker))
	next.chatModel = &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{initial}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "More information is needed.", Done: true, FinishReason: "stop"}}},
	}}
	_, err = next.Resume(t.Context(), "session", "turn-a", nil, asker.snapshot, answered)
	require.NoError(t, err)
	require.Len(t, next.retrieval.snapshot().Attempts, 1)
	require.Equal(t, ref, next.retrieval.snapshot().Attempts[0].ID)
	require.Empty(t, nextSearches.calls)
	require.Equal(t, 1, asker.resumes)
}
