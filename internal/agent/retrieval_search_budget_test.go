package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestQuestionSearchGraceRestoresOriginalRulesAfterAllowance(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"the user asks for sandbox configuration"}`}
	for i := 0; i < questionSearchGrace; i++ {
		args := fmt.Sprintf(`{"query":"query-%d"}`, i)
		if i == 1 {
			args = `{"query":"query-1","retrieval":{"gap_id":"invented","conditions":"iOS","action":"recovery","recovery_call_id":"invented"}}`
		}
		call := retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, args)
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Len(t, tool.calls, questionSearchGrace)
	require.Len(t, e.retrieval.snapshot().Gaps, 1)
	require.JSONEq(t, `{"query":"query-1"}`, string(tool.calls[1]))
	blocked := retrievalProbeCall(e, "illegal-initial", agenttools.ToolSearchKnowledge, `{"query":"again","retrieval":{"gap_id":"gap-1","action":"initial"}}`)
	require.False(t, blocked.Result.Success)
	require.Contains(t, blocked.Result.Error, "already been searched")
	require.Len(t, tool.calls, questionSearchGrace)

	supplement := retrievalProbeCall(e, "supplement", agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"supplement","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap","review":{"call_id":"%d","status":"no_progress"}}}`, questionSearchGrace-1))
	require.True(t, supplement.Result.Success, supplement.Result.Error)
	require.Len(t, tool.calls, questionSearchGrace+1, "the grace threshold is not a hard total search cap")
	state := e.retrieval.snapshot()
	require.True(t, state.Gaps[0].SupplementUsed)
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	var saved retrievalState
	require.NoError(t, json.Unmarshal(raw, &saved))
	restored, err := restoreRetrievalControl(saved)
	require.NoError(t, err)
	require.Equal(t, questionSearchGrace+1, restored.countSearches(&saved.Gaps[0], "", ""))
	spent := retrievalProbeCall(e, "spent", agenttools.ToolSearchKnowledge, `{"query":"again","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap","review":{"call_id":"supplement","status":"no_progress"}}}`)
	require.False(t, spent.Result.Success)
	require.Len(t, tool.calls, questionSearchGrace+1)
	require.Equal(t, retrievalExhausted, e.retrieval.snapshot().Gaps[0].Status)
}

func TestQuestionSearchGraceIsIndependentAcrossConversationBranches(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"a broad sandbox question"}`}
	for i := 0; i < questionSearchGrace; i++ {
		id := fmt.Sprint(i)
		require.True(t, retrievalProbeCall(e, id, agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	}
	previous, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	next, tool := newRetrievalProbeEngine(t)
	next.SetRetrievalUserMessage("ios", "iOS（App Store）包")
	next.beginTurn("iOS（App Store）包", nil, &types.AgentState{})
	next.EnableRetrievalGuard()
	model := &projectContextChat{mockChat: &mockChat{}, replies: []string{
		`{"decision":"continue","reason":"the user selects the requested platform"}`,
		`{"decision":"allow","reason":"iOS-specific steps require a separate conditioned answer"}`,
	}}
	next.chatModel = model
	next.SetRetrievalPersistence(previous, nil)
	require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
	for i := 0; i < questionSearchGrace; i++ {
		id := fmt.Sprintf("ios-%d", i)
		call := retrievalProbeCall(next, id, agenttools.ToolSearchKnowledge, `{"query":"iOS sandbox","retrieval":{"gap_id":"gap-1","conditions":"iOS App Store","action":"initial"}}`)
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Len(t, tool.calls, questionSearchGrace)
	state := next.retrieval.snapshot()
	require.Len(t, state.Gaps, 2)
	c := &retrievalControl{state: state}
	require.Equal(t, questionSearchGrace, c.countSearches(&state.Gaps[0], "", ""))
	require.Equal(t, questionSearchGrace, c.countSearches(&state.Gaps[1], "", ""))
	require.Equal(t, "message:ios", state.Gaps[1].GoalInputID)
	require.Equal(t, "iOS（App Store）包", state.Gaps[1].Conditions)
	_, err = restoreRetrievalControl(state)
	require.NoError(t, err)

	rephrased, rephrasedTool := newRetrievalProbeEngine(t)
	rephrased.EnableRetrievalGuard()
	rephrased.retrieval = next.retrieval
	rephrased.SetRetrievalUserMessage("ios-again", "再查一下 iOS 沙盒配置")
	rephrased.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"reuse","gap_id":"gap-2","reason":"the same iOS configuration question is merely reworded"}`}
	call := retrievalProbeCall(rephrased, "again", agenttools.ToolSearchKnowledge, `{"query":"another wording"}`)
	require.True(t, call.Result.Success, call.Result.Error)
	require.Len(t, rephrasedTool.calls, 1, "a new user message owns its allowance")
}

func TestDifferentFactsShareOneMessageAllowance(t *testing.T) {
	c := newRetrievalControl("分别查 iOS 和 Android", "scope")
	c.recordUserInput("message:one", c.state.Goal)
	for i, fact := range []string{"iOS", "Android", "iOS 和 Android"} {
		id, err := c.registerFact(retrievalFactProposal{GoalQuote: fact, Fact: fact}, false)
		require.NoError(t, err)
		callID := fmt.Sprint(i)
		err = c.acquire(id, callID, retrievalInitial, "", agenttools.ToolSearchKnowledge, `{"query":"lookup"}`)
		if i == questionSearchGrace {
			require.ErrorContains(t, err, "spent")
			break
		}
		require.NoError(t, err)
		require.NoError(t, c.observe(callID, "evidence", retrievalSuccess))
	}
	require.Len(t, c.snapshot().Attempts, questionSearchGrace)
}

func TestQuestionSearchGracePreservesFailureRecoveryLimit(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"sandbox configuration is requested"}`}
	tool.err = context.DeadlineExceeded
	for _, id := range []string{"one", "two", "three"} {
		require.False(t, retrievalProbeCall(e, id, agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	}
	require.Len(t, tool.calls, 2, "one failed operation and its original single recovery")
	require.True(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed)
	require.Equal(t, retrievalUnavailable, e.retrieval.snapshot().Gaps[0].Status)
}

func TestQuestionSearchGracePreservesExplicitStopReviews(t *testing.T) {
	for _, status := range []retrievalStatus{retrievalNeedsUser, retrievalSatisfied, retrievalExhausted} {
		t.Run(string(status), func(t *testing.T) {
			e, tool := newRetrievalProbeEngine(t)
			e.EnableRetrievalGuard()
			model := &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"sandbox configuration is requested"}`}
			e.chatModel = model
			require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
			review := retrievalReview{Status: status, CallID: "first"}
			if status == retrievalNeedsUser {
				review.MissingUserFields = []string{"platform"}
				model.reply = `{"necessary_user_inputs":[{"field":"platform","reason":"steps differ by platform"}]}`
			} else if status == retrievalSatisfied {
				review.Quotes = []string{"Only general information"}
			}
			args, err := json.Marshal(map[string]any{"query": "again", "retrieval": map[string]any{"review": review}})
			require.NoError(t, err)
			call := retrievalProbeCall(e, "second", agenttools.ToolSearchKnowledge, string(args))
			require.False(t, call.Result.Success)
			require.Len(t, tool.calls, 1, "explicit stop reviews remain effective inside the grace window")
			require.Equal(t, status, e.retrieval.snapshot().Gaps[0].Status)
		})
	}
}

func TestQuestionSearchGraceUsesInRunUserBranch(t *testing.T) {
	for _, source := range []string{"steer", "ask_user"} {
		t.Run(source, func(t *testing.T) {
			e, tool := newRetrievalProbeEngine(t)
			e.EnableRetrievalGuard()
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"sandbox configuration is requested"}`}
			for i := 0; i < questionSearchGrace; i++ {
				id := fmt.Sprint(i)
				require.True(t, retrievalProbeCall(e, id, agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
			}
			require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", Status: retrievalNeedsUser, MissingUserFields: []string{"platform"}}))
			inputID := "message:ios"
			if source == "steer" {
				e.recordSteerMessage("ios", "iOS App Store")
			} else {
				e.recordRetrievalUserAnswer(types.ToolCall{ID: "ios", Name: agenttools.ToolAskUser, Result: &types.ToolResult{
					Success: true, Data: map[string]any{"answered": true, "answer": types.AskUserPromptResponse{Selected: []string{"iOS App Store"}}},
				}})
				inputID = "ask_user:" + e.retrievalCallReference("ios")
			}
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"the selected iOS platform changes the required steps"}`}
			args, err := json.Marshal(map[string]any{"query": "iOS sandbox", "retrieval": map[string]any{
				"gap_id": "gap-1", "review": retrievalReview{Status: retrievalUserConditions, UserAnswers: []retrievalUserAnswer{
					{Field: "platform", InputID: inputID, Quote: "iOS App Store"},
				}},
			}})
			require.NoError(t, err)
			call := retrievalProbeCall(e, "ios-search", agenttools.ToolSearchKnowledge, string(args))
			require.True(t, call.Result.Success, call.Result.Error)
			require.Len(t, tool.calls, questionSearchGrace+1)
			state := e.retrieval.snapshot()
			require.Len(t, state.Gaps, 2)
			require.Equal(t, retrievalPending, state.Gaps[0].Status, "the old clarification is resolved without refunding its searches")
			require.Equal(t, inputID, state.Gaps[1].GoalInputID)
			require.Equal(t, "iOS App Store", state.Gaps[1].Conditions)
			require.Equal(t, 1, (&retrievalControl{state: state}).countSearches(&state.Gaps[1], "", ""))
		})
	}
}

func TestQuestionSearchGraceQueuesParallelSearches(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"sandbox configuration is requested"}`}
	response := &types.ChatResponse{}
	for i := 0; i < questionSearchGrace; i++ {
		id := fmt.Sprint(i)
		response.ToolCalls = append(response.ToolCalls, types.LLMToolCall{ID: id, Function: types.FunctionCall{
			Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"` + id + `"}`,
		}})
	}
	step := &types.AgentStep{}
	e.executeToolCallsParallel(t.Context(), response, step, 0, "session", "message")
	require.Len(t, tool.calls, questionSearchGrace)
	for _, call := range step.ToolCalls {
		require.True(t, call.Result.Success, call.Result.Error)
	}
}

func TestQuestionSearchGracePreservesExplicitScopeRevision(t *testing.T) {
	for _, count := range []int{1, questionSearchGrace} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			e, tool := retrievalScopeProbe(t, retrievalScopeTarget("a"))
			e.EnableRetrievalGuard()
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"configuration is requested in the newly available source range"}`}
			for i := 0; i < count; i++ {
				call := retrievalProbeCall(e, fmt.Sprintf("old-%d", i), agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["a"]}`)
				require.True(t, call.Result.Success, call.Result.Error)
			}
			e.config.SearchTargets = types.SearchTargets{retrievalScopeTarget("a"), retrievalScopeTarget("b")}
			require.NoError(t, e.retrieval.activateScope(retrievalScopeForTargets(e.config.SearchTargets)))
			call := retrievalProbeCall(e, "expanded", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["b"],"retrieval":{
				"action":"initial","goal_quote":"TapTap sandbox configuration","fact":"TapTap sandbox configuration","scope_from":"gap-1",
				"distinct_from":[{"gap_id":"gap-1","reason":"source b was not previously available"}]}}`)
			require.True(t, call.Result.Success, call.Result.Error)
			require.Len(t, tool.calls, count+1)
			require.Len(t, e.retrieval.snapshot().Gaps, 2)
			_, err := restoreRetrievalControl(e.retrieval.snapshot())
			require.NoError(t, err)
			call = retrievalProbeCall(e, "fake-revision", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["a"],"retrieval":{
				"action":"initial","goal_quote":"TapTap sandbox configuration","fact":"TapTap sandbox configuration","scope_from":"gap-2",
				"distinct_from":[{"gap_id":"gap-2","reason":"different query"}]}}`)
			require.False(t, call.Result.Success, "the grace window must not authorize a fictitious scope expansion")
			require.Len(t, tool.calls, count+1)
		})
	}
}

func TestQuestionSearchGracePreservesPrerequisiteSearch(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"test-account eligibility is a necessary prerequisite"}`}
	tool.output = "Before configuring sandbox, verify test-account regions."
	require.True(t, retrievalProbeCall(e, "root", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	call := retrievalProbeCall(e, "child", agenttools.ToolSearchKnowledge, `{"query":"test-account regions","retrieval":{
		"action":"initial","goal_quote":"TapTap sandbox configuration","fact":"test-account region eligibility",
		"source":{"call_id":"root","quote":"Before configuring sandbox, verify test-account regions."}}}`)
	require.True(t, call.Result.Success, call.Result.Error)
	call = retrievalProbeCall(e, "supplement", agenttools.ToolSearchKnowledge, `{"query":"test-account restrictions","retrieval":{
		"gap_id":"gap-2","action":"supplement","lead":"test-account regions","review":{"call_id":"child","status":"no_progress"}}}`)
	require.True(t, call.Result.Success, call.Result.Error)
	state := e.retrieval.snapshot()
	require.Len(t, tool.calls, 3)
	require.Equal(t, []string{"gap-2"}, state.Gaps[0].DependsOn)
	require.Equal(t, "gap-2", state.Attempts[2].GapID)
	require.True(t, state.Gaps[1].SupplementUsed, "prerequisites keep their original bounded supplement")
	call = retrievalProbeCall(e, "blocked-root", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
	require.False(t, call.Result.Success, "the parent must still wait for its prerequisite")
	require.Len(t, tool.calls, 3)
}

func TestQuestionSearchGracePreservesPartialFailureScopes(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, errors.New("permission denied")} {
		t.Run(cause.Error(), func(t *testing.T) {
			e, tool := newRetrievalProbeEngine(t)
			targets := types.SearchTargets{
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "a", TenantID: 1},
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "b", TenantID: 1},
			}
			e.config.SearchTargets = targets
			e.beginTurn("sandbox", nil, &types.AgentState{})
			e.EnableRetrievalGuard()
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"sandbox configuration is requested"}`}
			tool.result = &types.ToolResult{Success: true, Output: "useful evidence from a", RetrievalFailures: []types.RetrievalFailure{{
				KnowledgeBaseIDs: []string{"b"}, Targets: targets[1:], Stage: "search", Cause: cause,
			}}}
			require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["a","b"]}`).Result.Success)
			call := retrievalProbeCall(e, "retry", agenttools.ToolSearchKnowledge, `{"query":"another wording","knowledge_base_ids":["a","b"]}`)
			require.False(t, call.Result.Success)
			require.Contains(t, call.Result.Error, "failed scope")
			require.Len(t, tool.calls, 1)
			require.False(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed)
			tool.result = &types.ToolResult{Success: true, Output: "more evidence from a"}
			call = retrievalProbeCall(e, "independent", agenttools.ToolSearchKnowledge, `{"query":"another wording","knowledge_base_ids":["a"]}`)
			require.True(t, call.Result.Success, call.Result.Error)
			require.Len(t, tool.calls, 2)
			if errors.Is(cause, context.DeadlineExceeded) {
				call = retrievalProbeCall(e, "recover-first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["b"],"retrieval":{"gap_id":"gap-1","action":"recovery","recovery_call_id":"first","lead":"sandbox"}}`)
				require.True(t, call.Result.Success, call.Result.Error)
				require.Len(t, tool.calls, 3, "a later independent search does not hide the original recoverable failure")
				require.True(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed)
			}
			_, err := restoreRetrievalControl(e.retrieval.snapshot())
			require.NoError(t, err)
		})
	}
}

func TestWikiShadowsSearch(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
		want bool
	}{
		{"unscoped search", agenttools.ToolSearchKnowledge, `{"query":"防沉迷"}`, true},
		{"legacy name", agenttools.LegacyToolKnowledgeSearch, `{"query":"防沉迷"}`, true},
		{"empty scope", agenttools.ToolSearchKnowledge, `{"query":"防沉迷","knowledge_base_ids":[],"metadata":{}}`, true},
		{"knowledge base scoped", agenttools.ToolSearchKnowledge, `{"query":"防沉迷","knowledge_base_ids":["kb://1"]}`, false},
		{"metadata scoped", agenttools.ToolSearchKnowledge, `{"query":"防沉迷","metadata":{"region":"CN"}}`, false},
		{"wiki search itself", agenttools.ToolWikiSearch, `{"query":"防沉迷"}`, false},
		{"graph query", agenttools.ToolQueryKnowledgeGraph, `{"query":"防沉迷"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var args map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.args), &args))
			require.Equal(t, tc.want, wikiShadowsSearch(tc.tool, args))
		})
	}
}

func TestWikiShadowsSearchRejectsSameTurnLibrarySearch(t *testing.T) {
	cases := []struct {
		name       string
		parallel   bool
		calls      []string // tool:args
		dispatched int
	}{
		{"parallel wiki and unscoped search", true, []string{agenttools.ToolWikiSearch + `|{"query":"防沉迷"}`, agenttools.ToolSearchKnowledge + `|{"query":"防沉迷"}`}, 1},
		{"sequential search before wiki", false, []string{agenttools.ToolSearchKnowledge + `|{"query":"防沉迷"}`, agenttools.ToolWikiSearch + `|{"query":"防沉迷"}`}, 1},
		{"scoped search with wiki", true, []string{agenttools.ToolWikiSearch + `|{"query":"防沉迷"}`, agenttools.ToolSearchKnowledge + `|{"query":"防沉迷","knowledge_base_ids":["kb-1"]}`}, 1},
		{"search without wiki", true, []string{agenttools.ToolSearchKnowledge + `|{"query":"防沉迷"}`, agenttools.ToolSearchKnowledge + `|{"query":"实名认证"}`}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, probe := newRetrievalProbeEngine(t)
			e.EnableRetrievalGuard()
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"sandbox configuration"}`}
			e.config.ParallelToolCalls = tc.parallel
			e.toolRegistry.RegisterTool(&retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolWikiSearch, "test wiki", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`))})
			response := &types.ChatResponse{}
			for i, call := range tc.calls {
				name, args, _ := strings.Cut(call, "|")
				response.ToolCalls = append(response.ToolCalls, types.LLMToolCall{ID: fmt.Sprint("call-", i), Function: types.FunctionCall{Name: name, Arguments: args}})
			}
			step := &types.AgentStep{}
			require.Nil(t, e.executeToolCalls(context.Background(), response, step, 0, "session", "message"))
			require.Len(t, probe.calls, tc.dispatched)
			require.False(t, e.turnHasWikiSearch, "the turn flag must not leak into later rounds")
			for _, call := range step.ToolCalls {
				if call.Name != agenttools.ToolSearchKnowledge || call.Result.Success {
					continue
				}
				require.Equal(t, wikiParallelSearchError, call.Result.Error)
				require.Equal(t, wikiParallelSearchError, call.Result.Output, "the model sees the correction")
			}
			attempts := 0
			for _, attempt := range e.retrieval.snapshot().Attempts {
				if attempt.Tool == agenttools.ToolSearchKnowledge {
					attempts++
				}
			}
			require.Equal(t, tc.dispatched, attempts, "rejected searches stay out of the ledger")
		})
	}
}

func TestShadowedSearchKeepsOriginalOrderAndRemainingBudget(t *testing.T) {
	e, search := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"payment configuration"}`}
	e.config.ParallelToolCalls = true
	wiki := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolWikiSearch, "test wiki", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`))}
	e.toolRegistry.RegisterTool(wiki)
	for _, call := range []struct{ id, tool, args string }{
		{"prior-wiki", agenttools.ToolWikiSearch, `{"query":"taptap.*沙盒|沙盒.*支付"}`},
		{"prior-doc", agenttools.ToolSearchKnowledge, `{"query":"taptap 沙盒","mode":"keyword","limit":5}`},
	} {
		got := retrievalProbeCall(e, call.id, call.tool, call.args)
		require.True(t, got.Result.Success, got.Result.Error)
	}
	response := &types.ChatResponse{ToolCalls: []types.LLMToolCall{
		{ID: "target-search", Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"limit":5,"query":"TapTap 渠道沙盒支付怎么设置和测试？"}`}},
		{ID: "wiki-search", Function: types.FunctionCall{Name: agenttools.ToolWikiSearch, Arguments: `{"knowledge_base_ids":["kb-4","kb-6"],"query":"taptap|TapTap"}`}},
	}}
	step := &types.AgentStep{}
	require.Nil(t, e.executeToolCalls(context.Background(), response, step, 3, "session", "message"))
	require.Len(t, step.ToolCalls, 2)
	require.Equal(t, "target-search", step.ToolCalls[0].ID)
	require.True(t, step.ToolCalls[0].Result.Success, step.ToolCalls[0].Result.Error)
	require.Len(t, search.calls, 2, "original requested document search must be dispatched once")
	require.JSONEq(t, `{"limit":5,"query":"TapTap 渠道沙盒支付怎么设置和测试？"}`, string(search.calls[1]))
	require.False(t, step.ToolCalls[1].Result.Success, "the fourth ordinary search is still budgeted")
}

func TestShadowedBatchEmitsEachResultBeforeNextCall(t *testing.T) {
	e, search := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"payment configuration"}`}
	e.config.ParallelToolCalls = true
	var seen []string
	e.eventBus.On(event.EventAgentToolResult, func(_ context.Context, ev event.Event) error {
		seen = append(seen, ev.Data.(event.AgentToolResultData).ToolCallID)
		return nil
	})
	wiki := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolWikiSearch, "test wiki", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)), execute: func(_ context.Context, _ json.RawMessage) (*types.ToolResult, error) {
		require.Equal(t, []string{"search-first"}, seen, "first result must be emitted before the next call runs")
		return &types.ToolResult{Success: true, Output: "wiki evidence"}, nil
	}}
	e.toolRegistry.RegisterTool(wiki)
	response := &types.ChatResponse{ToolCalls: []types.LLMToolCall{
		{ID: "search-first", Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"TapTap 沙盒支付"}`}},
		{ID: "wiki-next", Function: types.FunctionCall{Name: agenttools.ToolWikiSearch, Arguments: `{"query":"TapTap"}`}},
	}}
	step := &types.AgentStep{}
	require.Nil(t, e.executeToolCalls(context.Background(), response, step, 0, "session", "message"))
	require.Equal(t, []string{"search-first", "wiki-next"}, seen)
	require.Len(t, search.calls, 1)
	require.Len(t, wiki.calls, 1)
	require.Len(t, step.ToolCalls, 2)
}
