package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestQuestionSearchGraceRestoresOriginalRulesAfterThreeCalls(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"the user asks for sandbox configuration"}`}
	for _, id := range []string{"one", "two", "three"} {
		args := `{"query":"` + id + `"}`
		if id == "two" {
			args = `{"query":"two","retrieval":{"gap_id":"invented","conditions":"iOS","action":"recovery","recovery_call_id":"invented"}}`
		}
		call := retrievalProbeCall(e, id, agenttools.ToolSearchKnowledge, args)
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Len(t, tool.calls, 3)
	require.Len(t, e.retrieval.snapshot().Gaps, 1)
	require.JSONEq(t, `{"query":"two"}`, string(tool.calls[1]))
	fourth := retrievalProbeCall(e, "illegal-fourth", agenttools.ToolSearchKnowledge, `{"query":"four","retrieval":{"gap_id":"gap-1","action":"initial"}}`)
	require.False(t, fourth.Result.Success)
	require.Contains(t, fourth.Result.Error, "already been searched")
	require.Len(t, tool.calls, 3)

	fourth = retrievalProbeCall(e, "four", agenttools.ToolSearchKnowledge, `{"query":"four","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap","review":{"call_id":"three","status":"no_progress"}}}`)
	require.True(t, fourth.Result.Success, fourth.Result.Error)
	require.Len(t, tool.calls, 4, "the grace threshold is not a hard total search cap")
	state := e.retrieval.snapshot()
	require.True(t, state.Gaps[0].SupplementUsed)
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	var saved retrievalState
	require.NoError(t, json.Unmarshal(raw, &saved))
	restored, err := restoreRetrievalControl(saved)
	require.NoError(t, err)
	require.Equal(t, 4, restored.searchCount(&saved.Gaps[0]))
	fifth := retrievalProbeCall(e, "five", agenttools.ToolSearchKnowledge, `{"query":"five","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap","review":{"call_id":"four","status":"no_progress"}}}`)
	require.False(t, fifth.Result.Success)
	require.Len(t, tool.calls, 4)
	require.Equal(t, retrievalExhausted, e.retrieval.snapshot().Gaps[0].Status)
}

func TestQuestionSearchGraceIsIndependentAcrossConversationBranches(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"a broad sandbox question"}`}
	for _, id := range []string{"one", "two", "three"} {
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
	for _, id := range []string{"ios-one", "ios-two", "ios-three"} {
		call := retrievalProbeCall(next, id, agenttools.ToolSearchKnowledge, `{"query":"iOS sandbox","retrieval":{"gap_id":"gap-1","conditions":"iOS App Store","action":"initial"}}`)
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Len(t, tool.calls, 3)
	state := next.retrieval.snapshot()
	require.Len(t, state.Gaps, 2)
	c := &retrievalControl{state: state}
	require.Equal(t, 3, c.searchCount(&state.Gaps[0]))
	require.Equal(t, 3, c.searchCount(&state.Gaps[1]))
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
	require.False(t, call.Result.Success)
	require.Empty(t, rephrasedTool.calls, "rewording cannot reopen the grace window")
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
				model.reply = `{"decision":"needs_user","missing_user_fields":["platform"],"reason":"steps differ by platform"}`
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
			for _, id := range []string{"one", "two", "three"} {
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
			require.Len(t, tool.calls, 4)
			state := e.retrieval.snapshot()
			require.Len(t, state.Gaps, 2)
			require.Equal(t, retrievalPending, state.Gaps[0].Status, "the old clarification is resolved without refunding its searches")
			require.Equal(t, inputID, state.Gaps[1].GoalInputID)
			require.Equal(t, "iOS App Store", state.Gaps[1].Conditions)
			require.Equal(t, 1, (&retrievalControl{state: state}).searchCount(&state.Gaps[1]))
		})
	}
}

func TestQuestionSearchGraceQueuesParallelSearches(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"sandbox configuration is requested"}`}
	response := &types.ChatResponse{}
	for _, id := range []string{"one", "two", "three"} {
		response.ToolCalls = append(response.ToolCalls, types.LLMToolCall{ID: id, Function: types.FunctionCall{
			Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"` + id + `"}`,
		}})
	}
	step := &types.AgentStep{}
	e.executeToolCallsParallel(t.Context(), response, step, 0, "session", "message")
	require.Len(t, tool.calls, 3)
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
				call = retrievalProbeCall(e, "recover-first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["b"],"retrieval":{"action":"recovery","recovery_call_id":"first"}}`)
				require.True(t, call.Result.Success, call.Result.Error)
				require.Len(t, tool.calls, 3, "a later independent search does not hide the original recoverable failure")
				require.True(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed)
			}
			_, err := restoreRetrievalControl(e.retrieval.snapshot())
			require.NoError(t, err)
		})
	}
}
