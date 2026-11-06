package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type retrievalProbe struct {
	agenttools.BaseTool
	mu      sync.Mutex
	calls   []json.RawMessage
	err     error
	output  string
	result  *types.ToolResult
	execute func(context.Context, json.RawMessage) (*types.ToolResult, error)
}

func (p *retrievalProbe) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, append(json.RawMessage(nil), args...))
	if p.execute != nil {
		return p.execute(ctx, args)
	}
	if p.err != nil {
		return nil, p.err
	}
	if p.result != nil {
		return p.result, nil
	}
	if p.output != "" {
		return &types.ToolResult{Success: true, Output: p.output}, nil
	}
	return &types.ToolResult{Success: true, Output: "Only general information; no sandbox configuration."}, nil
}

func newRetrievalProbeEngine(t *testing.T) (*AgentEngine, *retrievalProbe) {
	t.Helper()
	e := newTestEngine(t, &mockChat{})
	e.toolRegistry = agenttools.NewToolRegistry()
	p := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolSearchKnowledge, "test search", json.RawMessage(`{
		"type":"object","properties":{"query":{"type":"string"},"knowledge_base_ids":{"type":"array","items":{"type":"string"}}},"required":["query"],"additionalProperties":false
	}`))}
	e.toolRegistry.RegisterTool(p)
	e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
	return e, p
}

func retrievalProbeCall(e *AgentEngine, id, name, args string) types.ToolCall {
	return e.runToolCall(context.Background(), types.LLMToolCall{ID: id, Function: types.FunctionCall{
		Name: name, Arguments: args,
	}}, 0, 0, 1, "session", "message")
}

func TestRetrievalDispatchBlocksRephrasedInitialAndSpentSupplement(t *testing.T) {
	e, p := newRetrievalProbeEngine(t)
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"sandbox configuration","action":"initial"}}`)
	require.True(t, first.Result.Success, first.Result.Error)
	require.Contains(t, first.Args, "retrieval", "provider replay keeps the structured decision")
	require.JSONEq(t, `{"query":"sandbox"}`, string(p.calls[0]), "control metadata must not reach the original tool schema")
	rename := retrievalProbeCall(e, "renamed", agenttools.ToolSearchKnowledge, `{"query":"test payments","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"new name","action":"initial"}}`)
	require.False(t, rename.Result.Success)
	require.Len(t, p.calls, 1)
	second := retrievalProbeCall(e, "second", agenttools.ToolSearchKnowledge, `{"query":"TapTap test payments","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap","review":{"call_id":"first","status":"no_progress"}}}`)
	require.True(t, second.Result.Success, second.Result.Error)
	third := retrievalProbeCall(e, "third", agenttools.ToolSearchKnowledge, `{"query":"another wording","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap","review":{"call_id":"second","status":"no_progress"}}}`)
	require.False(t, third.Result.Success)
	require.Len(t, p.calls, 2, "no third external search after an unproductive supplement")
	require.Equal(t, retrievalExhausted, e.retrieval.snapshot().Gaps[0].Status)
}

func TestRetrievalDispatchRecoveryUsesTypedFailureAndOriginalOperation(t *testing.T) {
	e, p := newRetrievalProbeEngine(t)
	p.err = context.DeadlineExceeded
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","action":"initial"}}`)
	require.False(t, first.Result.Success)
	p.err = nil
	retry := retrievalProbeCall(e, "retry", agenttools.ToolSearchKnowledge, `{"retrieval":{"gap_id":"gap-1","action":"recovery"},"query":"sandbox"}`)
	require.True(t, retry.Result.Success, retry.Result.Error)
	require.Len(t, p.calls, 2, "canonical arguments allow a safe retry despite JSON key ordering")
	require.True(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed)
}

func TestRetrievalSchemaAndTerminalReview(t *testing.T) {
	e, p := newRetrievalProbeEngine(t)
	e.toolRegistry.RegisterTool(newCountingTool(agenttools.ToolAskUser))
	for _, tool := range e.buildToolsForLLM() {
		switch tool.Function.Name {
		case agenttools.ToolSearchKnowledge:
			require.Contains(t, string(tool.Function.Parameters), `"retrieval"`)
		case agenttools.ToolAskUser:
			require.JSONEq(t, `{"type":"object"}`, string(tool.Function.Parameters), "ask_user is unchanged")
		}
	}
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","action":"initial"}}`)
	require.True(t, first.Result.Success)
	resolved := retrievalProbeCall(e, "review", "review_retrieval", `{"gap_id":"gap-1","call_id":"first","status":"needs_user","quotes":["general information"],"missing_user_fields":["platform"]}`)
	require.True(t, resolved.Result.Success, resolved.Result.Error)
	blocked := retrievalProbeCall(e, "blocked", agenttools.ToolSearchKnowledge, `{"query":"guess platform","retrieval":{"gap_id":"gap-1","action":"read","lead":"TapTap"}}`)
	require.False(t, blocked.Result.Success)
	require.Len(t, p.calls, 1)
}

func TestRejectedRetrievalDecisionsFinalizeWithoutBurningRoundLimit(t *testing.T) {
	e, p := newRetrievalProbeEngine(t)
	call := func(id string) mockResponse {
		return mockResponse{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true,
			FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: id,
				Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"guess again","retrieval":{"gap_id":"gap-9","action":"search"}}`}}}}}}
	}
	model := &mockChat{responses: []mockResponse{call("bad-1"), call("bad-2"),
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "I could not confirm the configuration.", Done: true, FinishReason: "stop"}}},
	}}
	e.chatModel = model
	e.config.MaxIterations = 10
	state, err := e.executeLoop(context.Background(), &types.AgentState{}, "TapTap sandbox configuration", emptyMessages(), e.buildToolsForLLM(), "session", "message")
	require.NoError(t, err)
	require.True(t, state.IsComplete)
	require.Empty(t, p.calls)
	require.Len(t, model.calls, 3)
	require.Equal(t, "none", model.opts[2].ToolChoice)
	require.Equal(t, "I could not confirm the configuration.", state.FinalAnswer)
	require.True(t, e.retrieval.snapshot().DecisionCorrectionUsed)
}

func TestRetrievalParameterRecoveryAndUnknownFailures(t *testing.T) {
	e, p := newRetrievalProbeEngine(t)
	bad := retrievalProbeCall(e, "bad", agenttools.ToolSearchKnowledge, `{"retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","action":"initial"}}`)
	require.False(t, bad.Result.Success)
	require.Empty(t, p.calls)
	corrected := retrievalProbeCall(e, "corrected", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
	require.True(t, corrected.Result.Success, corrected.Result.Error)
	require.Len(t, p.calls, 1)
	for _, toolErr := range []error{errors.New("unknown failure"), errors.New("timeout mentioned in an untyped message")} {
		e, p := newRetrievalProbeEngine(t)
		p.err = toolErr
		first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","action":"initial"}}`)
		require.False(t, first.Result.Success)
		retry := retrievalProbeCall(e, "retry", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"gap_id":"gap-1","action":"recovery"}}`)
		require.False(t, retry.Result.Success)
		require.Len(t, p.calls, 1)
	}
}

func TestRetrievalSnapshotBlocksSavedSearchAfterResume(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","action":"initial"}}`)
	second := retrievalProbeCall(e, "second", agenttools.ToolSearchKnowledge, `{"query":"TapTap sandbox","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap","review":{"call_id":"first","status":"no_progress"}}}`)
	require.True(t, second.Result.Success, second.Result.Error)
	require.NoError(t, e.retrieval.review("gap-1", "second", retrievalExhausted, nil))
	e.progress.state.RoundSteps = []types.AgentStep{{ToolCalls: []types.ToolCall{first, second}}}
	e.beginRound(types.AgentStep{}, []types.LLMToolCall{
		{ID: "wait", Function: types.FunctionCall{Name: suspendingToolName, Arguments: `{}`}},
		{ID: "saved-search", Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"renamed","retrieval":{"gap_id":"gap-1","action":"initial"}}`}},
	})
	snap, err := e.snapshot(0)
	require.NoError(t, err)
	encoded, err := json.Marshal(snap)
	require.NoError(t, err)
	next, probe := newRetrievalProbeEngine(t)
	next.toolRegistry.RegisterTool(&suspendingTool{BaseTool: agenttools.NewBaseTool(suspendingToolName, "", json.RawMessage(`{"type":"object"}`))})
	next.chatModel = &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer,
		Content: "The configuration remains unconfirmed.", Done: true, FinishReason: "stop"}}}}}
	state, err := next.Resume(context.Background(), "session", "message", nil, encoded, &types.AgentPrompt{ID: "prompt", ToolCallID: "wait"})
	require.NoError(t, err)
	require.True(t, state.IsComplete)
	require.Empty(t, probe.calls, "a saved call cannot reopen an exhausted gap on resume")
	require.True(t, next.retrieval.snapshot().Gaps[0].SupplementUsed)
	require.Equal(t, retrievalExhausted, next.retrieval.snapshot().Gaps[0].Status)
}

func TestRetrievalParallelDispatchDoesNotMultiplyAllowance(t *testing.T) {
	e, p := newRetrievalProbeEngine(t)
	response := &types.ChatResponse{}
	for _, id := range []string{"a", "b"} {
		response.ToolCalls = append(response.ToolCalls, types.LLMToolCall{ID: id, Function: types.FunctionCall{
			Name:      agenttools.ToolSearchKnowledge,
			Arguments: `{"query":"` + id + `","retrieval":{"action":"initial","goal_quote":"TapTap sandbox configuration","fact":"configuration"}}`,
		}})
	}
	step := &types.AgentStep{}
	e.executeToolCallsParallel(context.Background(), response, step, 0, "session", "message")
	require.Len(t, p.calls, 1)
	require.Len(t, step.ToolCalls, 2)
	require.NotEqual(t, step.ToolCalls[0].Result.Success, step.ToolCalls[1].Result.Success)
	require.Len(t, e.retrieval.snapshot().Attempts, 1)
}

func TestRetrievalDispatchHonorsDocumentDiscoveredDependencies(t *testing.T) {
	e, search := newRetrievalProbeEngine(t)
	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "test reader", json.RawMessage(`{
		"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false
	}`)), output: "The configuration steps are now available."}
	e.toolRegistry.RegisterTool(reader)
	search.output = "Before configuring sandbox, verify test-account regions."
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration steps","action":"initial"}}`)
	require.True(t, first.Result.Success, first.Result.Error)
	require.True(t, retrievalProbeCall(e, "progress", reviewRetrievalTool, `{"gap_id":"gap-1","call_id":"first","status":"progress","quotes":["verify test-account regions"]}`).Result.Success)
	search.output = "US test accounts are supported."
	child := retrievalProbeCall(e, "child", agenttools.ToolSearchKnowledge, `{"query":"test-account regions","retrieval":{
		"goal_quote":"TapTap sandbox configuration","fact":"test-account region eligibility","action":"initial",
		"distinct_from":[{"gap_id":"gap-1","reason":"Account eligibility is a required prerequisite, separate from configuration steps"}],
		"source":{"call_id":"first","quote":"Before configuring sandbox, verify test-account regions."}
	}}`)
	require.True(t, child.Result.Success, child.Result.Error)
	require.JSONEq(t, `{"query":"test-account regions"}`, string(search.calls[1]), "the original tool receives no fact-control metadata")
	blocked := retrievalProbeCall(e, "premature", agenttools.ToolReadDocument, `{"id":"doc","retrieval":{"gap_id":"gap-1","action":"read","lead":"test-account regions"}}`)
	require.False(t, blocked.Result.Success)
	require.Empty(t, reader.calls)
	resolved := retrievalProbeCall(e, "resolved", reviewRetrievalTool, `{"gap_id":"gap-2","call_id":"child","status":"satisfied","quotes":["US test accounts are supported."]}`)
	require.True(t, resolved.Result.Success, resolved.Result.Error)
	allowed := retrievalProbeCall(e, "read", agenttools.ToolReadDocument, `{"id":"doc","retrieval":{"gap_id":"gap-1","action":"read","lead":"test-account regions"}}`)
	require.True(t, allowed.Result.Success, allowed.Result.Error)
	require.Len(t, reader.calls, 1)
	require.NotContains(t, e.retrievalStatePrompt(), "Before configuring sandbox", "the state prompt must not re-inject retained source quotes")
}

func TestRetrievalDispatchDefaultsMissingMetadataToInitial(t *testing.T) {
	e, p := newRetrievalProbeEngine(t)
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
	require.True(t, first.Result.Success, first.Result.Error)
	require.JSONEq(t, `{"query":"sandbox"}`, string(p.calls[0]))
	gaps := e.retrieval.snapshot().Gaps
	require.Len(t, gaps, 1)
	require.Equal(t, "TapTap sandbox configuration", gaps[0].Fact)
	again := retrievalProbeCall(e, "again", agenttools.ToolSearchKnowledge, `{"query":"sandbox again"}`)
	t.Logf("second defaulted call: success=%v err=%q", again.Result.Success, again.Result.Error)
	require.Len(t, e.retrieval.snapshot().Gaps, 1, "defaulted calls reuse the question's fact")
}
