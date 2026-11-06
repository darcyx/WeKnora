package agent

import (
	"context"
	"encoding/json"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestRetrievalClarificationBeforeAnySearch(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	e.beginTurn("支付未到账", nil, &types.AgentState{})
	call := retrievalProbeCall(e, "clarify", reviewRetrievalTool, `{
		"new_fact":{"goal_quote":"支付未到账","fact":"当前支付到账情况"},
		"status":"needs_user","missing_user_fields":["扣款情况","订单号"]
	}`)
	require.True(t, call.Result.Success, call.Result.Error)
	s := e.retrieval.snapshot()
	require.Len(t, s.Gaps, 1)
	require.Equal(t, retrievalNeedsUser, s.Gaps[0].Status)
	require.Empty(t, s.Attempts)
	require.Empty(t, probe.calls, "registering a clarification must never search")
	require.False(t, s.Gaps[0].SupplementUsed)
	require.False(t, s.Gaps[0].RecoveryUsed)
	encoded, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"missing_user_fields":["扣款情况","订单号"]`)
	var restored retrievalState
	require.NoError(t, json.Unmarshal(encoded, &restored))
	e.retrieval, err = restoreRetrievalControl(restored)
	require.NoError(t, err)
	blocked := retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge,
		`{"query":"支付排查","retrieval":{"gap_id":"gap-1","action":"initial"}}`)
	require.False(t, blocked.Result.Success)
	require.Empty(t, probe.calls, "restoring a zero-search gap must retain its dependency block")
}

func TestRetrievalReviewNeedsUserRequiresBoundaryDecision(t *testing.T) {
	for _, mode := range []string{"new", "existing", "inline"} {
		t.Run(mode, func(t *testing.T) {
			e, probe := newRetrievalProbeEngine(t)
			goal := "怎么添加支付白名单?"
			e.SetRetrievalUserMessage("user", goal)
			e.beginTurn(goal, nil, &types.AgentState{})
			review := retrievalReview{Status: retrievalNeedsUser, MissingUserFields: []string{"渠道或包类型"}}
			fact := retrievalFactProposal{GoalQuote: goal, Fact: "添加支付白名单的后台入口与操作步骤"}
			if mode == "new" {
				review.NewFact = &fact
			} else {
				id, err := e.retrieval.register(fact)
				require.NoError(t, err)
				review.GapID = id
			}
			e.EnableRetrievalGuard()
			model := &projectContextChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"search documents before determining whether channels differ"}`}
			e.chatModel = model
			before := e.retrieval.snapshot()
			name := reviewRetrievalTool
			var args any = review
			if mode == "inline" {
				name = agenttools.ToolSearchKnowledge
				args = map[string]any{"query": "支付白名单", "retrieval": map[string]any{"gap_id": review.GapID, "action": "initial", "review": review}}
			}
			raw, err := json.Marshal(args)
			require.NoError(t, err)
			call := retrievalProbeCall(e, "premature-clarification", name, string(raw))
			require.False(t, call.Result.Success)
			require.Contains(t, call.Result.Error, "clarification is not established as necessary")
			require.Equal(t, before, e.retrieval.snapshot(), "a rejected clarification must not create or close a fact")
			require.Empty(t, probe.calls)
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(model.messages[len(model.messages)-1].Content[len("review\n"):]), &payload))
			input := payload["input"].(map[string]any)["review"].(map[string]any)
			require.Equal(t, "needs_user", input["status"])
			require.Equal(t, []any{"渠道或包类型"}, input["missing_user_fields"])
			require.NotContains(t, payload, "app_info")
		})
	}
}

func TestRetrievalReviewAcceptsVerifiedMissingUserFields(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	e.beginTurn("支付未到账", nil, &types.AgentState{})
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"needs_user","missing_user_fields":["扣款情况","订单号"],"reason":"required by the business workflow"}`}
	call := retrievalProbeCall(e, "clarify", reviewRetrievalTool, `{"new_fact":{"goal_quote":"支付未到账","fact":"支付到账情况"},"status":"needs_user","missing_user_fields":["支付信息"]}`)
	require.True(t, call.Result.Success, call.Result.Error)
	s := e.retrieval.snapshot()
	require.Len(t, s.Gaps, 1)
	require.Equal(t, retrievalNeedsUser, s.Gaps[0].Status)
	require.Equal(t, []string{"扣款情况", "订单号"}, s.Gaps[0].MissingUserFields)
	require.False(t, s.Gaps[0].SupplementUsed)
	require.False(t, s.Gaps[0].RecoveryUsed)
	require.Empty(t, probe.calls)
}

func TestRetrievalUnknownIDCanBeCorrectedWithoutInventingUserFields(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	before := e.retrieval.snapshot()
	bad := retrievalProbeCall(e, "bad", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"gap_id":"g1","action":"initial","goal_quote":"TapTap sandbox configuration","fact":"configuration"}}`)
	require.False(t, bad.Result.Success)
	require.Contains(t, bad.Result.Error, `unknown gap_id "g1"`)
	require.Contains(t, bad.Result.Error, "omitting gap_id")
	require.Equal(t, bad.Result.Error, bad.Result.Output, "live and stored tool output must retain the real failure")
	require.Equal(t, before, e.retrieval.snapshot())
	require.Empty(t, probe.calls)
	require.True(t, e.retrieval.takeDecisionCorrection())
	corrected := retrievalProbeCall(e, "corrected", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"action":"initial","goal_quote":"TapTap sandbox configuration","fact":"configuration"}}`)
	require.True(t, corrected.Result.Success, corrected.Result.Error)
	require.Len(t, probe.calls, 1)
	require.JSONEq(t, `{"query":"sandbox"}`, string(probe.calls[0]))
	require.False(t, e.retrieval.takeDecisionCorrection(), "the correction allowance remains bounded")
}

func TestRetrievalReviewRegistrationIsAtomic(t *testing.T) {
	for _, args := range []string{
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user"}`,
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user","missing_user_fields":[" "]}`,
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user","missing_user_fields":["platform","platform"]}`,
		`{"new_fact":{"goal_quote":"invented","fact":"configuration"},"status":"needs_user","missing_user_fields":["platform"]}`,
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"satisfied","call_id":"invented","quotes":["supported"]}`,
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"progress","call_id":"invented","quotes":["supported"]}`,
		`{"gap_id":"gap-1","new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user","missing_user_fields":["platform"]}`,
	} {
		t.Run(args, func(t *testing.T) {
			e, probe := newRetrievalProbeEngine(t)
			e.EnableRetrievalGuard()
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"the fact may be searched"}`}
			before := e.retrieval.snapshot()
			call := retrievalProbeCall(e, "review", reviewRetrievalTool, args)
			require.False(t, call.Result.Success)
			require.Equal(t, before, e.retrieval.snapshot(), "a rejected decision cannot leave an orphan fact")
			require.Empty(t, probe.calls)
		})
	}
}

func TestRetrievalOneObservationCanSatisfyIndependentFacts(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	probe.output = "TapTap supports sandbox. Configure sandbox in the developer console."
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{
		"query":"TapTap sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"sandbox support","action":"initial"}
	}`)
	require.True(t, first.Result.Success, first.Result.Error)
	resolved := retrievalProbeCall(e, "support", reviewRetrievalTool,
		`{"gap_id":"gap-1","call_id":"first","status":"satisfied","quotes":["TapTap supports sandbox."]}`)
	require.True(t, resolved.Result.Success, resolved.Result.Error)
	configuration := retrievalProbeCall(e, "reuse", reviewRetrievalTool, `{
		"new_fact":{"goal_quote":"TapTap sandbox configuration","fact":"sandbox configuration",
		"depends_on":["gap-1"],"distinct_from":[{"gap_id":"gap-1","reason":"configuration instructions are distinct from support"}]},
		"call_id":"first","status":"satisfied","quotes":["Configure sandbox in the developer console."]
	}`)
	require.True(t, configuration.Result.Success, configuration.Result.Error)
	s := e.retrieval.snapshot()
	require.Len(t, s.Gaps, 2)
	require.Equal(t, retrievalSatisfied, s.Gaps[1].Status)
	require.Empty(t, s.Gaps[1].LastCallID, "evidence reuse is not another execution")
	require.Len(t, s.Attempts, 1)
	require.Len(t, probe.calls, 1)
	encoded, err := json.Marshal(s)
	require.NoError(t, err)
	var saved retrievalState
	require.NoError(t, json.Unmarshal(encoded, &saved))
	control, err := restoreRetrievalControl(saved)
	require.NoError(t, err)
	dependent, err := control.register(retrievalFactProposal{GoalQuote: "TapTap sandbox configuration", Fact: "verification",
		DependsOn: []string{"gap-2"}, DistinctFrom: []retrievalDistinction{
			{GapID: "gap-1", Reason: "verification is a separate prerequisite"},
			{GapID: "gap-2", Reason: "verification is not the configuration instruction"},
		}})
	require.NoError(t, err)
	require.Error(t, control.acquire(dependent, "missing-evidence", retrievalInitial, "", "search_knowledge", `{}`))
	control.restoreObservation("first", probe.output)
	require.NoError(t, control.acquire(dependent, "available-evidence", retrievalInitial, "", "search_knowledge", `{}`))
}

func TestRetrievalWaitingForEvidenceIsNotUserClarification(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{
		"query":"sandbox support","retrieval":{"goal_quote":"TapTap","fact":"sandbox support","action":"initial"}
	}`)
	require.True(t, first.Result.Success, first.Result.Error)
	before := e.retrieval.snapshot()
	call := retrievalProbeCall(e, "clarify", reviewRetrievalTool, `{
		"new_fact":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","depends_on":["gap-1"],
		"distinct_from":[{"gap_id":"gap-1","reason":"configuration is distinct from support"}]},
		"status":"needs_user","missing_user_fields":["whether sandbox is supported"]
	}`)
	require.False(t, call.Result.Success, "a pending evidence prerequisite cannot be recast as a user condition")
	require.Equal(t, before, e.retrieval.snapshot())
	require.Len(t, probe.calls, 1)
}

func TestRetrievalClarificationPublishesOnlyTheQuestion(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	question := "请问是否已扣款？请提供订单号。"
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer,
			Content: "用户只说了支付未到账，缺少关键条件。", Done: true, FinishReason: "tool_calls",
			ToolCalls: []types.LLMToolCall{{ID: "clarification-state", Function: types.FunctionCall{
				Name: reviewRetrievalTool, Arguments: `{"new_fact":{"goal_quote":"支付未到账","fact":"支付到账情况"},"status":"needs_user","missing_user_fields":["扣款情况","订单号"]}`,
			}}}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: question, Done: true, FinishReason: "stop"}}},
	}}
	e.chatModel = model
	state := &types.AgentState{}
	e.beginTurn("支付未到账", nil, state)
	var published string
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		published += evt.Data.(event.AgentFinalAnswerData).Content
		return nil
	})
	state, err := e.executeLoop(context.Background(), state, "支付未到账", emptyMessages(), e.buildToolsForLLM(), "session", "message")
	require.NoError(t, err)
	require.Equal(t, "方便提供一下扣款情况、订单号吗？", state.FinalAnswer)
	require.Equal(t, state.FinalAnswer, published, "the controller's missing fields govern the published clarification")
	require.Len(t, model.calls, 2, "no separate classifier or supervisor call is introduced")
	require.Empty(t, probe.calls)
	require.Equal(t, retrievalNeedsUser, e.retrieval.snapshot().Gaps[0].Status)
}

func TestRetrievalReviewStateValidatesAndCopiesReferences(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	clarify := retrievalProbeCall(e, "clarify", reviewRetrievalTool,
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user","missing_user_fields":["platform"]}`)
	require.True(t, clarify.Result.Success, clarify.Result.Error)
	s := e.retrieval.snapshot()
	s.Gaps[0].MissingUserFields[0] = "changed"
	require.Equal(t, []string{"platform"}, e.retrieval.snapshot().Gaps[0].MissingUserFields)
	for _, change := range []func(*retrievalState){
		func(s *retrievalState) { s.Gaps[0].MissingUserFields = nil },
		func(s *retrievalState) { s.Gaps[0].Status = retrievalPending },
		func(s *retrievalState) { s.Gaps[0].MissingUserFields = []string{"platform", " Platform "} },
		func(s *retrievalState) { s.Gaps[0].EvidenceCallID = "invented" },
	} {
		s := e.retrieval.snapshot()
		change(&s)
		_, err := restoreRetrievalControl(s)
		require.Error(t, err)
	}

	e, p := newRetrievalProbeEngine(t)
	p.output = "TapTap supports sandbox."
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","retrieval":{"goal_quote":"TapTap","fact":"support","action":"initial"}}`)
	require.True(t, first.Result.Success)
	resolved := retrievalProbeCall(e, "satisfied", reviewRetrievalTool,
		`{"gap_id":"gap-1","call_id":"first","status":"satisfied","quotes":["TapTap supports sandbox."]}`)
	require.True(t, resolved.Result.Success, resolved.Result.Error)
	for _, change := range []func(*retrievalState){
		func(s *retrievalState) { s.Gaps[0].EvidenceCallID = "invented" },
		func(s *retrievalState) { s.Attempts[0].Outcome = retrievalUnknown },
		func(s *retrievalState) { s.Gaps[0].Evidence = nil },
		func(s *retrievalState) { s.Gaps[0].Evidence = []string{"invented evidence"} },
	} {
		s := e.retrieval.snapshot()
		change(&s)
		_, err := restoreRetrievalControl(s)
		require.Error(t, err)
	}
}

func TestRetrievalEvidenceReuseDoesNotReopenClosedGaps(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	probe.output = "TapTap supports sandbox."
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","retrieval":{"goal_quote":"TapTap","fact":"support","action":"initial"}}`)
	require.True(t, first.Result.Success)
	resolved := retrievalProbeCall(e, "satisfied", reviewRetrievalTool,
		`{"gap_id":"gap-1","call_id":"first","status":"satisfied","quotes":["TapTap supports sandbox."]}`)
	require.True(t, resolved.Result.Success)
	before := e.retrieval.snapshot()
	for _, args := range []string{
		`{"gap_id":"gap-1","call_id":"first","status":"satisfied","quotes":["TapTap supports sandbox."]}`,
		`{"gap_id":"gap-1","status":"needs_user","missing_user_fields":["platform"]}`,
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration","distinct_from":[{"gap_id":"gap-1","reason":"configuration differs"}]},"call_id":"first","status":"satisfied","quotes":["fabricated configuration"]}`,
	} {
		call := retrievalProbeCall(e, "rejected", reviewRetrievalTool, args)
		require.False(t, call.Result.Success)
		require.Equal(t, before, e.retrieval.snapshot())
	}
	require.Len(t, probe.calls, 1)
}
