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

type boundaryChat struct {
	*mockChat
	reply string
}

func (m *boundaryChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return &types.ChatResponse{Content: m.reply}, nil
}

func TestRetrievalBoundaryMissingUserAndSynonym(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		blocked     bool
	}{
		{"missing", `{"decision":"needs_user","missing_user_fields":["平台"],"reason":"the required platform is unknown"}`, true},
		{"same fact", `{"decision":"reuse","gap_id":"gap-1","reason":"the same sandbox setting is already covered"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, tool := newRetrievalProbeEngine(t)
			if !tc.blocked {
				id, err := e.retrieval.register(retrievalFactProposal{GoalQuote: "TapTap sandbox configuration", Fact: "sandbox setting"})
				require.NoError(t, err)
				require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: id, Status: retrievalNeedsUser, MissingUserFields: []string{"平台"}}))
			}
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: tc.reply}
			e.EnableRetrievalGuard()
			call := retrievalProbeCall(e, "attempt", agenttools.ToolSearchKnowledge, `{"query":"another phrasing","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configure test payment","action":"initial"}}`)
			require.False(t, call.Result.Success)
			require.Empty(t, tool.calls)
			require.Len(t, e.retrieval.snapshot().Gaps, 1)
			require.Equal(t, retrievalNeedsUser, e.retrieval.snapshot().Gaps[0].Status)
		})
	}
}

func TestRetrievalBoundaryFirstSearchRepairsRedundantMetadata(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	e.beginTurn("海外支持哪些支付方式和有没有参数需要申请", nil, &types.AgentState{})
	e.EnableRetrievalGuard()
	model := &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"the user independently requests payment methods and application parameters"}`}
	e.chatModel = model
	for i, tc := range []struct{ id, fact string }{{"methods", "支持的支付方式"}, {"parameters", "需要申请的参数"}} {
		call := retrievalProbeCall(e, tc.id, agenttools.ToolSearchKnowledge, `{"query":"`+tc.fact+`","retrieval":{"action":"initial","goal_quote":"海外支持哪些支付方式和有没有参数需要申请","fact":"`+tc.fact+`","conditions":"海外"}}`)
		require.True(t, call.Result.Success, call.Result.Error)
		require.Len(t, tool.calls, i+1)
	}
	state := e.retrieval.snapshot()
	require.Len(t, state.Gaps, 1, "model fact labels within one user question share its search window")
	_, err := restoreRetrievalControl(state)
	require.NoError(t, err, "runtime-inferred quotes and optional distinctions survive replay")

	model.reply = `{"decision":"reuse","gap_id":"gap-1","reason":"payment options and payment methods require the same answer"}`
	duplicate := retrievalProbeCall(e, "duplicate", agenttools.ToolSearchKnowledge, `{"query":"支付手段","retrieval":{"action":"initial","goal_quote":"海外支持哪些支付方式和有没有参数需要申请","fact":"海外支付手段","conditions":"海外"}}`)
	require.False(t, duplicate.Result.Success, "a reworded initial search cannot bypass the two-call allowance")
	require.Contains(t, duplicate.Result.Error, "already been searched")
	require.Len(t, tool.calls, 2)
	require.Len(t, e.retrieval.snapshot().Attempts, 2)
}

func TestRetrievalBoundaryReusesUnsearchedFact(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	id, err := e.retrieval.register(retrievalFactProposal{GoalQuote: "TapTap sandbox configuration", Fact: "sandbox setting"})
	require.NoError(t, err)
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"reuse","gap_id":"` + id + `","reason":"the registered fact has not been searched"}`}
	call := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"action":"initial","goal_quote":"TapTap sandbox configuration","fact":"configure test payment"}}`)
	require.True(t, call.Result.Success, call.Result.Error)
	require.Len(t, tool.calls, 1)
	require.Len(t, e.retrieval.snapshot().Gaps, 1)
	require.Len(t, e.retrieval.snapshot().Attempts, 1)
}

func TestRetrievalBoundaryContinuationKeepsStopAndAllowsNewGoal(t *testing.T) {
	old := newRetrievalControl("payment missing", "scope")
	id, err := old.register(retrievalFactProposal{GoalQuote: "payment missing", Fact: "payment status"})
	require.NoError(t, err)
	require.NoError(t, old.applyReview(retrievalReview{GapID: id, Status: retrievalNeedsUser, MissingUserFields: []string{"order"}}))
	previous, err := json.Marshal(old.snapshot())
	require.NoError(t, err)
	for _, decision := range []string{"continue", "new_goal"} {
		t.Run(decision, func(t *testing.T) {
			e, _ := newRetrievalProbeEngine(t)
			e.retrieval = newRetrievalControl("payment missing again", "scope")
			e.SetRetrievalUserMessage("user-2", "payment missing again")
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"` + decision + `","reason":"user goal relationship"}`}
			var saved json.RawMessage
			e.SetRetrievalPersistence(previous, func(_ context.Context, raw json.RawMessage) error {
				saved = append(json.RawMessage(nil), raw...)
				return nil
			})
			require.NoError(t, e.initializeRetrievalContinuation(t.Context()))
			require.NotEmpty(t, saved)
			if decision == "continue" {
				require.Equal(t, retrievalNeedsUser, e.retrieval.snapshot().Gaps[0].Status)
			} else {
				require.Empty(t, e.retrieval.snapshot().Gaps)
			}
		})
	}
}

func TestRetrievalBoundaryPublicProsePreservesQuestionsCodeAndQuotes(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`现在我有足够信息组织回复。用户只说了"支付未到账"，缺少关键条件：` + "\n请问是否已扣款？请提供订单号。", "请问是否已扣款？请提供订单号。"},
		{"我需要您提供订单号信息。", "我需要您提供订单号信息。"},
		{"```text\n现在我有足够信息组织回复。\n```", "```text\n现在我有足够信息组织回复。\n```"},
		{"> 现在我有足够信息组织回复。", "> 现在我有足够信息组织回复。"},
	} {
		got, _ := stripProcessNarration(tc.input)
		require.Equal(t, tc.want, got)
	}
}

func TestRetrievalBoundaryDirectAnswerCannotSkipRequiredUserFields(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.SetRetrievalUserMessage("user", "支付未到账")
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"needs_user","missing_user_fields":["扣款情况","订单号"],"reason":"required by the business workflow"}`}
	answer, err := e.publicAnswer(t.Context(), "请重新支付。", nil)
	require.NoError(t, err)
	require.Equal(t, "方便提供一下扣款情况、订单号吗？", answer)
	require.Equal(t, retrievalNeedsUser, e.retrieval.snapshot().Gaps[0].Status)
}

func TestRetrievalBoundaryIsolatedRecoveryDoesNotRecallOrReplay(t *testing.T) {
	e, tool := newRetrievalProbeEngine(t)
	target := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
	e.config.SearchTargets = target
	e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
	retries := 0
	tool.result = &types.ToolResult{Success: true, Output: "retained candidates", RetrievalFailures: []types.RetrievalFailure{{
		KnowledgeBaseIDs: []string{"kb"}, Targets: target, Stage: "rerank", Cause: context.DeadlineExceeded,
		Retry: func(context.Context) (*types.ToolResult, error) {
			retries++
			return &types.ToolResult{Success: true, Output: "ranked retained candidates"}, nil
		},
	}}}
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"sandbox","action":"initial"}}`)
	require.True(t, first.Result.Success)
	args := `{"query":"sandbox","knowledge_base_ids":["kb"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"TapTap"}}`
	recovered := retrievalProbeCall(e, "recovered", agenttools.ToolSearchKnowledge, args)
	require.True(t, recovered.Result.Success, recovered.Result.Error)
	require.Equal(t, 1, retries)
	require.Len(t, tool.calls, 1)
	require.Empty(t, outstandingRetrievalFailures(e.retrieval.snapshot().Attempts, "gap-1"))
	again := retrievalProbeCall(e, "again", agenttools.ToolSearchKnowledge, args)
	require.False(t, again.Result.Success)
	require.Equal(t, 1, retries)
	raw, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	var state retrievalState
	require.NoError(t, json.Unmarshal(raw, &state))
	_, err = restoreRetrievalControl(state)
	require.NoError(t, err)
}

func TestRetrievalBoundaryPartialStageRecoveryKeepsSuccessfulEvidence(t *testing.T) {
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "a", TenantID: 1}, {Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "b", TenantID: 1}}
	c := newRetrievalControl("sandbox", retrievalScopeForTargets(targets))
	id, err := c.register(retrievalFactProposal{GoalQuote: "sandbox", Fact: "sandbox setting"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(id, "first", retrievalInitial, "", "search_knowledge", `{"query":"sandbox"}`))
	failures := []retrievalFailure{
		{CallID: "first", KnowledgeBaseIDs: []string{"a"}, Targets: targets[:1], Stage: "faq_exact", Outcome: retrievalTransient, RetryAvailable: true, Retry: func(context.Context) (*types.ToolResult, error) {
			return &types.ToolResult{Success: true, Output: "recovered evidence"}, nil
		}},
		{CallID: "first", KnowledgeBaseIDs: []string{"b"}, Targets: targets[1:], Stage: "faq_rerank", Outcome: retrievalTransient, RetryAvailable: true, Retry: func(context.Context) (*types.ToolResult, error) { return nil, context.DeadlineExceeded }},
	}
	require.NoError(t, c.observe("first", "partial evidence", retrievalSuccess, failures...))
	require.NoError(t, c.acquire(id, "recovery", retrievalRecovery, "sandbox", "search_knowledge", `{"knowledge_base_ids":["a","b"],"query":"sandbox"}`))
	result, err := c.dispatchRecovery("recovery")(t.Context())
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Contains(t, result.Output, "recovered evidence")
	require.Len(t, result.RetrievalFailures, 1)
	require.NoError(t, c.observe("recovery", result.Output, retrievalSuccess, retrievalCallFailures(t.Context(), result)...))
	remaining := outstandingRetrievalFailures(c.snapshot().Attempts, id)
	require.Len(t, remaining, 1)
	require.Equal(t, []string{"b"}, remaining[0].KnowledgeBaseIDs)
}
