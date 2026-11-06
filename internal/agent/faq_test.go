package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type faqDispatchTool struct {
	agenttools.BaseTool
	calls int
	check func(context.Context, json.RawMessage)
}

func (t *faqDispatchTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	if meta, ok := agenttools.ToolExecFromContext(ctx); ok && meta.KnowledgeSearch != nil {
		return meta.KnowledgeSearch(ctx, args)
	}
	t.calls++
	if t.check != nil {
		t.check(ctx, args)
	}
	return &types.ToolResult{Success: true, Output: "document evidence"}, nil
}

func faqDispatchFixture(t *testing.T, model *mockChat) (*AgentEngine, *faqDispatchTool) {
	t.Helper()
	e := newTestEngine(t, model)
	e.config.SearchTargets = types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "faq", TenantID: 1},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "doc", KnowledgeIDs: []string{"selected-doc"}, TenantID: 1},
	}
	e.toolRegistry = agenttools.NewToolRegistry()
	tool := &faqDispatchTool{BaseTool: agenttools.NewBaseTool(agenttools.ToolSearchKnowledge, "search", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1},"knowledge_base_ids":{"type":"array","items":{"type":"string"}}},"required":["query"],"additionalProperties":false}`))}
	e.toolRegistry.RegisterTool(tool)
	e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
	return e, tool
}

const faqInitialArgs = `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"sandbox configuration","action":"initial"}}`

func faqResolution(reply string) *FAQResolution {
	return &FAQResolution{Observations: json.RawMessage(`[{"stage":"faq_exact","completed_targets":["faq"]}]`),
		Result: &types.ToolResult{Success: true, Output: "verified FAQ evidence", FAQ: &types.FAQToolResult{Reply: reply, Usage: types.TokenUsage{TotalTokens: 7}}}}
}

func TestFAQDispatchRequiresAdmittedInitialAction(t *testing.T) {
	e, tool := faqDispatchFixture(t, &mockChat{})
	calls := 0
	e.SetFAQResolver(func(context.Context, types.SearchTargets) (*FAQResolution, error) {
		calls++
		return faqResolution("原文答案"), nil
	})
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, faqInitialArgs)
	require.True(t, first.Result.Success, first.Result.Error)
	require.Equal(t, 1, calls)
	require.Zero(t, tool.calls)
	require.Equal(t, retrievalSatisfied, e.retrieval.snapshot().Gaps[0].Status)
	second := retrievalProbeCall(e, "again", agenttools.ToolSearchKnowledge, `{"query":"test payments","retrieval":{"gap_id":"gap-1","action":"initial"}}`)
	require.False(t, second.Result.Success)
	require.Equal(t, 1, calls, "a rephrasing cannot re-enter FAQ")
}

func TestFAQDispatchPreservesRegistryAuthorityAndValidation(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "authority", true: "schema"}[invalid], func(t *testing.T) {
			e, tool := faqDispatchFixture(t, &mockChat{})
			e.SetFAQResolver(func(context.Context, types.SearchTargets) (*FAQResolution, error) {
				t.Fatal("FAQ must not execute before registry authorization and validation")
				return nil, nil
			})
			args := faqInitialArgs
			if invalid {
				args = `{"query":"","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"sandbox configuration","action":"initial"}}`
			} else {
				e.toolRegistry.SetExecutionGuard(func(context.Context, types.Tool) error { return errors.New("not authorized") })
			}
			call := retrievalProbeCall(e, "denied", agenttools.ToolSearchKnowledge, args)
			require.False(t, call.Result.Success)
			require.Zero(t, tool.calls)
		})
	}
}

func TestFAQDispatchRecordsBeforeOneDocumentHandoff(t *testing.T) {
	e, tool := faqDispatchFixture(t, &mockChat{})
	faqCalls := 0
	e.SetFAQResolver(func(_ context.Context, targets types.SearchTargets) (*FAQResolution, error) {
		faqCalls++
		require.Equal(t, e.config.SearchTargets, targets)
		resolution := faqResolution("")
		resolution.DocumentTargets = e.config.SearchTargets[1:].Clone()
		return resolution, nil
	})
	tool.check = func(ctx context.Context, raw json.RawMessage) {
		saved := e.retrieval.snapshot()
		require.Len(t, saved.Attempts, 1)
		require.NotNil(t, saved.Attempts[0].FAQ, "FAQ observations must be recorded before the document call")
		meta, ok := agenttools.ToolExecFromContext(ctx)
		require.True(t, ok)
		require.Equal(t, e.config.SearchTargets[1:], meta.RetrievalTargets)
		var input agenttools.SearchKnowledgeInput
		require.NoError(t, json.Unmarshal(raw, &input))
		require.Equal(t, []string{"doc"}, input.KnowledgeBaseIDs)
	}
	call := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, faqInitialArgs)
	require.True(t, call.Result.Success, call.Result.Error)
	require.Contains(t, call.Result.Output, "verified FAQ evidence")
	require.Contains(t, call.Result.Output, "document evidence")
	require.Equal(t, 1, faqCalls)
	require.Equal(t, 1, tool.calls)
	s := e.retrieval.snapshot()
	require.Len(t, s.Attempts, 1, "handoff remains part of the original initial action")
	require.False(t, s.Gaps[0].SupplementUsed)
	require.False(t, s.Gaps[0].RecoveryUsed)
	restored, err := restoreRetrievalControl(s)
	require.NoError(t, err)
	require.NotNil(t, restored.snapshot().Attempts[0].FAQ)
}

func TestFAQDispatchOriginalAnswerCompletesAfterRecordedDecision(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer,
		ToolCalls: []types.LLMToolCall{{ID: "first", Type: "function", Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: faqInitialArgs}}},
		Done:      true, FinishReason: "tool_calls"}}}}}
	e, tool := faqDispatchFixture(t, model)
	const answer = "\n 原文答案\n\n```code\nkeep formatting\n```\n"
	e.SetFAQResolver(func(context.Context, types.SearchTargets) (*FAQResolution, error) { return faqResolution(answer), nil })
	var public []string
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
		require.Equal(t, retrievalSatisfied, e.retrieval.snapshot().Gaps[0].Status, "decision must be recorded before publication")
		public = append(public, ev.Data.(event.AgentFinalAnswerData).Content)
		return nil
	})
	state, err := e.Execute(t.Context(), "session", "message", "TapTap sandbox configuration", []chat.Message{})
	require.NoError(t, err)
	require.True(t, state.IsComplete)
	require.Equal(t, answer, state.FinalAnswer)
	require.Equal(t, []string{answer}, public)
	require.Equal(t, 1, model.callCount, "verified FAQ must not need another answer-generation call")
	require.Equal(t, 7, state.Usage.TotalTokens)
	require.Zero(t, tool.calls)
}

func TestFAQReplyRespectsOutstandingFacts(t *testing.T) {
	for _, tc := range []struct {
		name          string
		otherGap      bool
		otherStatus   retrievalStatus
		clarification bool
		wantReply     bool
	}{
		{name: "complete evidence", wantReply: true},
		{name: "other gap needs user", otherGap: true, otherStatus: retrievalNeedsUser},
		{name: "other gap pending", otherGap: true, otherStatus: retrievalPending},
		{name: "other gap in flight", otherGap: true, otherStatus: retrievalInFlight},
		{name: "other gap exhausted", otherGap: true, otherStatus: retrievalExhausted},
		{name: "other gap evidence unavailable", otherGap: true, otherStatus: retrievalSatisfied},
		{name: "question omits other missing fields", otherGap: true, otherStatus: retrievalNeedsUser, clarification: true},
		{name: "question for current gap", clarification: true, wantReply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := faqDispatchFixture(t, &mockChat{})
			e.SetFAQResolver(func(context.Context, types.SearchTargets) (*FAQResolution, error) {
				r := faqResolution("适用答案")
				if tc.clarification {
					r.Result.FAQ.Reply = "请问使用哪个平台？"
					r.Result.FAQ.MissingUserFields = []string{"平台"}
				}
				return r, nil
			})
			call := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, faqInitialArgs)
			require.True(t, call.Result.Success, call.Result.Error)
			if tc.otherGap {
				e.retrieval.state.Gaps = append(e.retrieval.state.Gaps, retrievalGap{ID: "other", Status: tc.otherStatus})
			}
			state := &types.AgentState{}
			var messages []chat.Message
			var public []string
			e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
				public = append(public, ev.Data.(event.AgentFinalAnswerData).Content)
				return nil
			})
			finished, err := e.finishFAQReply(t.Context(), state, types.AgentStep{ToolCalls: []types.ToolCall{call}}, &messages, "session", "message")
			require.NoError(t, err)
			require.Equal(t, tc.wantReply, finished)
			if tc.wantReply {
				require.Equal(t, []string{call.Result.FAQ.Reply}, public)
			} else {
				require.Empty(t, public)
				require.Empty(t, state.FinalAnswer)
			}
		})
	}
}

func TestFAQInterruptedEvaluationRetainsObservedEvidenceAndUsage(t *testing.T) {
	e, tool := faqDispatchFixture(t, &mockChat{})
	e.SetFAQResolver(func(context.Context, types.SearchTargets) (*FAQResolution, error) {
		r := faqResolution("stale answer")
		r.DocumentTargets = e.config.SearchTargets[1:].Clone()
		return r, context.Canceled
	})
	call := retrievalProbeCall(e, "interrupted", agenttools.ToolSearchKnowledge, faqInitialArgs)
	require.False(t, call.Result.Success)
	require.Contains(t, call.Result.Error, context.Canceled.Error())
	require.Contains(t, call.Result.Output, "verified FAQ evidence")
	require.NotNil(t, call.Result.FAQ)
	require.Empty(t, call.Result.FAQ.Reply)
	require.Equal(t, 7, call.Result.FAQ.Usage.TotalTokens)
	require.NotNil(t, e.retrieval.snapshot().Attempts[0].FAQ)
	require.Zero(t, tool.calls)
}

func TestFAQResolverIsSkippedForDirectClarification(t *testing.T) {
	const question = "请问是否已扣款？请提供订单号。"
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "用户只说了支付未到账，缺少关键条件。",
			ToolCalls: []types.LLMToolCall{{ID: "clarify", Function: types.FunctionCall{Name: reviewRetrievalTool,
				Arguments: `{"new_fact":{"goal_quote":"支付未到账","fact":"当前支付到账情况"},"status":"needs_user","missing_user_fields":["扣款情况","订单号"]}`}}},
			Done: true, FinishReason: "tool_calls"}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: question, Done: true, FinishReason: "stop"}}},
	}}
	e, tool := faqDispatchFixture(t, model)
	e.SetFAQResolver(func(context.Context, types.SearchTargets) (*FAQResolution, error) {
		t.Fatal("direct clarification must not start hidden FAQ retrieval")
		return nil, nil
	})
	var public string
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
		public += ev.Data.(event.AgentFinalAnswerData).Content
		return nil
	})
	state, err := e.Execute(t.Context(), "session", "message", "支付未到账", nil)
	require.NoError(t, err)
	require.Equal(t, "方便提供一下扣款情况、订单号吗？", public)
	require.Equal(t, public, state.FinalAnswer)
	require.Equal(t, retrievalNeedsUser, e.retrieval.snapshot().Gaps[0].Status)
	require.Empty(t, e.retrieval.snapshot().Attempts)
	require.Zero(t, tool.calls)
}

func TestFAQClarificationStopsDependentRetrieval(t *testing.T) {
	e, tool := faqDispatchFixture(t, &mockChat{})
	const question = "请问使用哪个平台？"
	e.SetFAQResolver(func(context.Context, types.SearchTargets) (*FAQResolution, error) {
		r := faqResolution(question)
		r.Result.FAQ.MissingUserFields = []string{"平台"}
		r.Result.FAQ.Clarification = json.RawMessage(`{"ids":["faq"]}`)
		return r, nil
	})
	call := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, faqInitialArgs)
	require.True(t, call.Result.Success, call.Result.Error)
	state := &types.AgentState{}
	var messages []chat.Message
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
		saved := e.retrieval.snapshot()
		require.Equal(t, retrievalNeedsUser, saved.Gaps[0].Status)
		require.Equal(t, []string{"平台"}, saved.Gaps[0].MissingUserFields)
		require.Equal(t, question, ev.Data.(event.AgentFinalAnswerData).Content)
		return nil
	})
	finished, err := e.finishFAQReply(t.Context(), state, types.AgentStep{ToolCalls: []types.ToolCall{call}}, &messages, "session", "message")
	require.NoError(t, err)
	require.True(t, finished)
	require.Equal(t, question, state.FinalAnswer)
	require.Equal(t, types.FAQClarificationToolCallID, state.RoundSteps[0].ToolCalls[0].ID)
	again := retrievalProbeCall(e, "again", agenttools.ToolSearchKnowledge, `{"query":"test payments","retrieval":{"gap_id":"gap-1","action":"initial"}}`)
	require.False(t, again.Result.Success)
	require.Zero(t, tool.calls)
}

func TestFAQDocumentHandoffKeepsModelVisibleStageDiagnostics(t *testing.T) {
	e, _ := faqDispatchFixture(t, &mockChat{})
	faq := &types.ToolResult{Success: true, Output: "FAQ source", Data: map[string]any{
		"display_type": "search_results", "omitted_for_budget": 2,
		"results": []map[string]any{{"faq_id": "source-faq-id", "knowledge_id": "source-faq-document", "knowledge_base_id": "source-faq-kb", "content": "FAQ evidence"}},
	}}
	documents := &types.ToolResult{Success: true, Output: "document source", Data: map[string]any{
		"display_type": "search_results", "mode": "keyword", "rerank_outcome": "model_error",
		"partial_failures": []string{"one document source timed out"},
		"results":          []map[string]any{{"chunk_id": "source-chunk-id", "knowledge_id": "source-selected-document", "knowledge_base_id": "source-document-kb", "content": "document evidence"}},
	}}
	merged := mergeFAQSearchResult(faq, documents)
	require.Equal(t, "search_results", merged.Data["display_type"])
	require.Equal(t, 2, merged.Data["count"])
	rows := merged.Data["results"].([]map[string]any)
	require.Equal(t, "source-faq-id", rows[0]["chunk_id"], "FAQ cards need the same chunk identity as document cards")
	require.NotContains(t, faq.Data["results"].([]map[string]any)[0], "chunk_id", "presentation must not mutate stage evidence")
	require.Equal(t, map[string]int{"source-faq-kb": 1, "source-document-kb": 1}, merged.Data["kb_counts"])
	for _, replay := range []bool{false, true} {
		if replay {
			raw, err := json.Marshal(merged)
			require.NoError(t, err)
			merged = &types.ToolResult{}
			require.NoError(t, json.Unmarshal(raw, merged))
			require.Equal(t, float64(2), merged.Data["count"])
		}
		visible := e.modelContext.ModelToolResultForTool(agenttools.ToolSearchKnowledge, merged)
		require.Contains(t, visible, "FAQ evidence")
		require.Contains(t, visible, "document evidence")
		require.Contains(t, visible, `<omitted count="2"`)
		require.Contains(t, visible, `<rerank outcome="model_error"`)
		require.Contains(t, visible, "one document source timed out")
		require.Equal(t, 2, strings.Count(visible, "<retrieval "), "each stage retains its own coverage and retrieval mode")
	}
}

func TestFAQDocumentHandoffCountsOnlyUniqueSuccessfulResults(t *testing.T) {
	row := map[string]any{"chunk_id": "chunk", "knowledge_id": "document", "knowledge_base_id": "kb", "content": "evidence"}
	faq := &types.ToolResult{Success: true, Data: map[string]any{"display_type": "search_results", "results": []map[string]any{row}}}
	documents := &types.ToolResult{Success: true, Data: map[string]any{"display_type": "search_results", "results": []any{row}}}
	merged := mergeFAQSearchResult(faq, documents)
	require.True(t, merged.Success)
	require.Equal(t, 1, merged.Data["count"])
	require.Equal(t, map[string]int{"kb": 1}, merged.Data["kb_counts"])

	documents.Success, documents.Error = false, "document search timed out"
	documents.Data["results"] = []any{map[string]any{"chunk_id": "failed-chunk", "knowledge_base_id": "failed-kb"}}
	partial := mergeFAQSearchResult(faq, documents)
	require.True(t, partial.Success)
	require.Equal(t, "search_results", partial.Data["display_type"])
	require.Equal(t, 1, partial.Data["count"])
	require.Contains(t, partial.Error, "timed out")

	faq.Success = false
	failed := mergeFAQSearchResult(faq, documents)
	require.False(t, failed.Success)
	require.Equal(t, "search_stages", failed.Data["display_type"])
	require.Equal(t, 0, failed.Data["count"])
	require.Empty(t, failed.Data["results"])
}

func TestMergedRecoveryKeepsNonSearchDiagnostics(t *testing.T) {
	graph := &types.ToolResult{Success: true, Output: "Recovered isolated graph stage", Data: map[string]any{
		"display_type": "graph_query_results", "results": []map[string]any{{"chunk_id": "graph-chunk", "content": "graph evidence"}},
	}}
	failed := &types.ToolResult{Output: "The isolated graph stage did not complete", Error: "graph timeout"}
	for _, tc := range []struct {
		name        string
		left, right *types.ToolResult
		success     bool
	}{
		{"successful graph stages", graph, graph, true},
		{"partial failure", graph, failed, true},
		{"all failed", failed, failed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeFAQSearchResult(tc.left, tc.right)
			require.Equal(t, tc.success, merged.Success)
			require.Equal(t, "search_stages", merged.Data["display_type"])
			require.Contains(t, merged.Output, tc.left.Output)
			require.Contains(t, merged.Output, tc.right.Output)
			require.Contains(t, merged.Error, tc.right.Error)
			stages := merged.Data["stages"].([]map[string]any)
			require.Equal(t, tc.left.Data, stages[0]["data"])
			require.Equal(t, tc.right.Error, stages[1]["error"])
		})
	}
}
