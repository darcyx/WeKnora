package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/modelcontext"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type runtimeRetrievalChat struct {
	*mockChat
	boundaryCalls int
}

func (m *runtimeRetrievalChat) Chat(_ context.Context, messages []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	m.boundaryCalls++
	return &types.ChatResponse{Content: `{"decision":"allow","reason":"the question has no necessary missing user conditions"}`}, nil
}

func newRuntimeRetrievalProbeEngine(t *testing.T) (*AgentEngine, *retrievalProbe) {
	e, probe := newRetrievalProbeEngine(t)
	e.chatModel = &runtimeRetrievalChat{mockChat: &mockChat{}}
	return e, probe
}

func TestRuntimeRetrievalUsesOriginalToolsAndRecordedOutcomes(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	model := &runtimeRetrievalChat{mockChat: &mockChat{}}
	e.chatModel = model
	search.output = `<retrieval><document id="doc"><content>配置说明</content></document></retrieval>`
	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)), output: "完整配置步骤"}
	e.toolRegistry.RegisterTool(reader)
	for _, tool := range e.buildToolsForLLM() {
		require.NotEqual(t, reviewRetrievalTool, tool.Function.Name)
		require.NotContains(t, string(tool.Function.Parameters), `"retrieval"`)
		if tool.Function.Name == agenttools.ToolSearchKnowledge {
			require.Contains(t, tool.Function.Description, "an allowance, not a target")
			require.Contains(t, tool.Function.Description, "no usable answer and no grounded next lead")
			require.Contains(t, tool.Function.Description, "Neither case requires review_retrieval merely to finish")
		}
	}
	require.Empty(t, e.retrievalStatePrompt())
	first := retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
	require.True(t, first.Result.Success, first.Result.Error)
	read := retrievalProbeCall(e, "read", agenttools.ToolReadDocument, `{"id":"doc"}`)
	require.True(t, read.Result.Success, read.Result.Error)
	require.Len(t, reader.calls, 1)
	unknown := retrievalProbeCall(e, "unknown", agenttools.ToolReadDocument, `{"id":"invented"}`)
	require.False(t, unknown.Result.Success)
	require.Len(t, reader.calls, 1)
	require.Equal(t, retrievalPending, e.retrieval.snapshot().Gaps[0].Status, "execution success does not prove answer sufficiency")
	repeated := retrievalProbeCall(e, "repeat", agenttools.ToolReadDocument, `{"id":"doc"}`)
	require.False(t, repeated.Result.Success)
	require.Len(t, reader.calls, 1)
	require.Equal(t, 1, model.boundaryCalls)
	_, err := restoreRetrievalControl(e.retrieval.snapshot())
	require.NoError(t, err)
}

func TestRuntimeRetrievalGraceThenOriginalSupplementAndStop(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	for i := 0; i < questionSearchGrace; i++ {
		call := retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"query-%d"}`, i))
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.False(t, e.usesRuntimeRetrieval())
	require.Contains(t, e.retrievalStatePrompt(), fmt.Sprintf("first %d search calls", questionSearchGrace))
	compact := e.retrievalStatePrompt()
	e.runtimeRetrieval = false
	structured := e.retrievalStatePrompt()
	e.runtimeRetrieval = true
	require.Less(t, len(compact), len(structured)/2, "ordinary QA must not carry the full dependency and scope protocol")
	require.Equal(t, strings.Split(compact, "</retrieval_user_inputs>")[0], strings.Split(structured, "</retrieval_user_inputs>")[0], "compact guidance must retain the complete runtime state")
	t.Logf("retrieval notice: ordinary=%d bytes, structured=%d bytes", len(compact), len(structured))
	require.False(t, retrievalProbeCall(e, "unreviewed", agenttools.ToolSearchKnowledge, `{"query":"again"}`).Result.Success)
	require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", CallID: fmt.Sprint(questionSearchGrace - 1), Status: retrievalNoProgress}))
	call := retrievalProbeCall(e, "supplement", agenttools.ToolSearchKnowledge, `{"query":"TapTap configuration","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap"}}`)
	require.True(t, call.Result.Success, call.Result.Error)
	require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", CallID: "supplement", Status: retrievalNoProgress}))
	require.False(t, retrievalProbeCall(e, "spent", agenttools.ToolSearchKnowledge, `{"query":"yet another wording"}`).Result.Success)
	require.Len(t, search.calls, questionSearchGrace+1)
	_, err := restoreRetrievalControl(e.retrieval.snapshot())
	require.NoError(t, err)
}

func TestRuntimeRetrievalReadsPublishedRecentReferences(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.knowledgeBasesInfo = []*KnowledgeBaseInfo{nil, {ID: "kb", RecentDocs: []RecentDocInfo{
		{KnowledgeID: "doc", ChunkID: "faq"}, {KnowledgeID: "second"}, {KnowledgeID: "unpublished"},
	}}}
	reader := runtimeRegressionReader()
	e.toolRegistry.RegisterTool(reader)
	refs := []string{e.modelContext.RegisterDocument("doc"), e.modelContext.RegisterContextChunk(modelcontext.ChunkReference{ChunkID: "faq", KnowledgeID: "doc", KnowledgeBaseID: "kb"})}
	for i, ref := range refs {
		calls := []types.LLMToolCall{{ID: fmt.Sprintf("read-%d", i), Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: fmt.Sprintf(`{"id":%q}`, ref)}}}
		e.modelContext.DecodeToolCalls(calls)
		call := e.runToolCall(t.Context(), calls[0], 0, 0, 1, "session", "message")
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Len(t, reader.calls, 2)
	require.False(t, retrievalProbeCall(e, "hidden", agenttools.ToolReadDocument, `{"id":"unpublished"}`).Result.Success)
	require.False(t, retrievalProbeCall(e, "unknown", agenttools.ToolReadDocument, `{"id":"invented"}`).Result.Success)
	require.Len(t, reader.calls, 2)
}

func TestRuntimeRetrievalKeepsFailureAndPersistenceBoundaries(t *testing.T) {
	for _, saveFails := range []bool{false, true} {
		t.Run(fmt.Sprint(saveFails), func(t *testing.T) {
			e, search := newRuntimeRetrievalProbeEngine(t)
			e.EnableRuntimeRetrieval()
			e.chatModel = &runtimeRetrievalChat{mockChat: &mockChat{}}
			search.err = context.DeadlineExceeded
			if saveFails {
				e.retrieval.persist = func(retrievalState) error { return fmt.Errorf("storage unavailable") }
			}
			for i := 0; i < 3; i++ {
				require.False(t, retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
			}
			if saveFails {
				require.Empty(t, search.calls)
			} else {
				require.Len(t, search.calls, 2, "one execution and one recovery")
			}
		})
	}
}

func TestRuntimeRetrievalPreservesExplicitDependencies(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	id, err := e.retrieval.register(retrievalFactProposal{GoalQuote: "TapTap", Fact: "payment status"})
	require.NoError(t, err)
	require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: id, Status: retrievalNeedsUser, MissingUserFields: []string{"order"}}))
	require.False(t, e.usesRuntimeRetrieval())
	var names []string
	for _, tool := range e.buildToolsForLLM() {
		names = append(names, tool.Function.Name)
	}
	require.Contains(t, names, reviewRetrievalTool)
}

func TestRuntimeRetrievalPreservesExplicitStop(t *testing.T) {
	for _, status := range []retrievalStatus{retrievalSatisfied, retrievalExhausted} {
		t.Run(string(status), func(t *testing.T) {
			e, search := newRuntimeRetrievalProbeEngine(t)
			e.EnableRuntimeRetrieval()
			require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
			require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", CallID: "first", Status: status, Quotes: []string{"Only general information"}}))
			require.False(t, retrievalProbeCall(e, "next", agenttools.ToolSearchKnowledge, `{"query":"configuration steps"}`).Result.Success)
			require.Len(t, search.calls, 1)
		})
	}
}

func TestRuntimeRetrievalIdenticalNewMessageGetsFreshBudgetWithoutModelReview(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	raw, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	next, search := newRuntimeRetrievalProbeEngine(t)
	next.EnableRuntimeRetrieval()
	next.SetRetrievalUserMessage("next-message", "TapTap sandbox configuration")
	model := &runtimeRetrievalChat{mockChat: &mockChat{}}
	next.chatModel = model
	next.SetRetrievalPersistence(raw, nil)
	require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
	for i := 0; i < questionSearchGrace; i++ {
		require.True(t, retrievalProbeCall(next, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"query-%d"}`, i)).Result.Success)
	}
	require.False(t, retrievalProbeCall(next, "spent", agenttools.ToolSearchKnowledge, `{"query":"again"}`).Result.Success)
	require.Len(t, search.calls, questionSearchGrace)
	require.Len(t, next.retrieval.snapshot().Gaps, 1)
	require.Zero(t, model.boundaryCalls)
}

func TestRuntimeRetrievalNormalLoopNeedsOnlySearchAndAnswerRounds(t *testing.T) {
	model := &runtimeRetrievalChat{mockChat: &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls",
			ToolCalls: []types.LLMToolCall{{ID: "search", Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"sandbox"}`}}}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "请按文档配置。", Done: true, FinishReason: "stop"}}},
	}}}
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.chatModel = model
	state, err := e.Execute(t.Context(), "session", "message", "TapTap sandbox configuration", nil)
	require.NoError(t, err)
	require.Equal(t, "请按文档配置。", state.FinalAnswer)
	require.Len(t, search.calls, 1)
	require.Equal(t, 2, model.callCount)
	require.Equal(t, 1, model.boundaryCalls)
}

func TestRuntimeRetrievalDirectAnswerOrQuestionDoesNotNeedStatusReview(t *testing.T) {
	for _, answer := range []string{"可以在设置中配置。", "请提供订单号。", "通用配置方法见上述说明；查询这笔订单还需要订单号。"} {
		t.Run(answer, func(t *testing.T) {
			e, _ := newRuntimeRetrievalProbeEngine(t)
			e.EnableRuntimeRetrieval()
			model := &runtimeRetrievalChat{mockChat: &mockChat{}}
			e.chatModel = model
			got, err := e.publicAnswer(t.Context(), answer, nil)
			require.NoError(t, err)
			require.Equal(t, answer, got)
			require.Equal(t, 1, model.boundaryCalls)
			require.Empty(t, e.retrieval.snapshot().Gaps)
		})
	}
}

func TestRuntimeRetrievalPlanAloneDoesNotRequireStructuredProtocol(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.progress.state.Plan = &types.ExecutionPlan{}
	require.True(t, e.usesRuntimeRetrieval())
	var names []string
	for _, tool := range e.buildToolsForLLM() {
		names = append(names, tool.Function.Name)
	}
	require.NotContains(t, names, reviewRetrievalTool)
}

func TestRuntimeRetrievalReadBatchSharesQuestionWithoutInFlightRejection(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	search.output = `<retrieval><document id="one"/><document id="two"/></retrieval>`
	require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)), output: "document content"}
	e.toolRegistry.RegisterTool(reader)
	response := &types.ChatResponse{ToolCalls: []types.LLMToolCall{
		{ID: "one", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"one"}`}},
		{ID: "two", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"two"}`}},
	}}
	var step types.AgentStep
	require.Nil(t, e.executeToolCallsParallel(t.Context(), response, &step, 0, "session", "message"))
	require.Len(t, step.ToolCalls, 2)
	for _, call := range step.ToolCalls {
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Len(t, reader.calls, 2)
}

func TestRuntimeRetrievalRecoversEarlierPartialFailureAfterReading(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
	e.config.SearchTargets = targets
	e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
	e.EnableRuntimeRetrieval()
	search.result = &types.ToolResult{Success: true, Output: `<retrieval><document id="doc"/></retrieval>`,
		RetrievalFailures: []types.RetrievalFailure{{KnowledgeBaseIDs: []string{"kb"}, Targets: targets, Stage: "embedding", Cause: context.DeadlineExceeded}}}
	require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)), output: "usable evidence"}
	e.toolRegistry.RegisterTool(reader)
	require.True(t, retrievalProbeCall(e, "read", agenttools.ToolReadDocument, `{"id":"doc"}`).Result.Success)
	search.result = &types.ToolResult{Success: true, Output: "recovered evidence"}
	call := retrievalProbeCall(e, "recover", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["kb"]}`)
	require.True(t, call.Result.Success, call.Result.Error)
	state := e.retrieval.snapshot()
	require.Equal(t, "search", state.Attempts[2].RecoveryCallID)
	require.Empty(t, outstandingRetrievalFailures(state.Attempts, "gap-1"))
	_, err := restoreRetrievalControl(state)
	require.NoError(t, err)
}

func TestRuntimeRetrievalContinuationFromBroadQuestion(t *testing.T) {
	const goal = "sdk的沙盒支付怎么设置"
	for _, tc := range []struct {
		name, input, decision string
		used                  int
		newBranch             bool
	}{
		{"platform after two searches", "App Store iOS 内购", "allow", 2, true},
		{"platform matched to original fact", "App Store iOS 内购", "reuse", 2, false},
		{"platform after one search", "App Store iOS 内购", "allow", 1, true},
		{"rephrasing after two searches", "再查一下 SDK 沙盒支付配置", "reuse", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newRuntimeRetrievalProbeEngine(t)
			e.EnableRuntimeRetrieval()
			e.SetRetrievalUserMessage("original", goal)
			e.beginTurn(goal, nil, &types.AgentState{})
			for i := 0; i < tc.used; i++ {
				call := retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"query-%d"}`, i))
				require.True(t, call.Result.Success, call.Result.Error)
			}
			raw, err := json.Marshal(e.retrieval.snapshot())
			require.NoError(t, err)
			next, search := newRuntimeRetrievalProbeEngine(t)
			next.EnableRuntimeRetrieval()
			next.SetRetrievalUserMessage("followup", tc.input)
			next.beginTurn(tc.input, nil, &types.AgentState{})
			next.SetRetrievalPersistence(raw, nil)
			next.chatModel = &projectContextChat{mockChat: &mockChat{}, replies: []string{
				`{"decision":"continue","gap_id":"gap-1","reason":"same overall goal"}`,
				fmt.Sprintf(`{"decision":"%s","gap_id":"gap-1","reason":"checked the applicability of the current input"}`, tc.decision),
			}}
			require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
			for i := 0; i < questionSearchGrace; i++ {
				call := retrievalProbeCall(next, fmt.Sprintf("followup-%d", i), agenttools.ToolSearchKnowledge,
					fmt.Sprintf(`{"query":"query-%d","retrieval":{"action":"initial","goal_quote":%q,"fact":%q}}`, i, tc.input, tc.input))
				require.True(t, call.Result.Success, call.Result.Error)
			}
			// Even a fully grounded initial proposal cannot re-register this message for more searches.
			extra := retrievalProbeCall(next, "extra", agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"different wording","retrieval":{"action":"initial","goal_quote":%q,"goal_input_id":"message:followup","fact":"another fact"}}`, tc.input))
			require.False(t, extra.Result.Success)
			require.Len(t, next.chatModel.(*projectContextChat).calls, 2)
			require.Len(t, search.calls, questionSearchGrace)
			state := next.retrieval.snapshot()
			require.Len(t, state.Attempts, tc.used+questionSearchGrace)
			require.Equal(t, "message:original", state.Attempts[0].QuestionID)
			require.Equal(t, "message:followup", state.Attempts[tc.used].QuestionID)
			if tc.newBranch {
				require.Len(t, state.Gaps, 2)
				require.Equal(t, tc.input, state.Gaps[1].Conditions)
				require.Equal(t, state.Gaps[1].ID, state.Attempts[tc.used].GapID)
			} else {
				require.Len(t, state.Gaps, 1)
			}
			_, err = restoreRetrievalControl(state)
			require.NoError(t, err)
		})
	}
}

func TestQuestionBudgetSurvivesResumeAndLegacyAttempts(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			const goal = "SDK 沙盒支付"
			e, _ := newRuntimeRetrievalProbeEngine(t)
			e.EnableRuntimeRetrieval()
			e.SetRetrievalUserMessage("question", goal)
			e.beginTurn(goal, nil, &types.AgentState{})
			e.retrievalTurnID = "reply"
			for i := 0; i < questionSearchGrace; i++ {
				call := retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"query-%d"}`, i))
				require.True(t, call.Result.Success, call.Result.Error)
			}
			saved := e.retrieval.snapshot()
			if legacy {
				for i := range saved.Attempts {
					saved.Attempts[i].QuestionID = ""
				}
			}
			raw, err := json.Marshal(saved)
			require.NoError(t, err)
			for _, msg := range []struct {
				id, turn string
				want     bool
			}{{"question", "reply", false}, {"new-question", "new-reply", true}} {
				next, search := newRuntimeRetrievalProbeEngine(t)
				next.EnableRuntimeRetrieval()
				next.SetRetrievalUserMessage(msg.id, goal)
				next.beginTurn(goal, nil, &types.AgentState{})
				next.retrievalTurnID = msg.turn
				next.SetRetrievalPersistence(raw, nil)
				require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
				call := retrievalProbeCall(next, "retry", agenttools.ToolSearchKnowledge, `{"query":"new words"}`)
				require.Equal(t, msg.want, call.Result.Success, call.Result.Error)
				require.Equal(t, msg.want, len(search.calls) == 1)
				_, err = restoreRetrievalControl(next.retrieval.snapshot())
				require.NoError(t, err)
			}
		})
	}
}

func TestRuntimeRetrievalRejectedCallDoesNotReintroduceReviewProtocol(t *testing.T) {
	model := &runtimeRetrievalChat{mockChat: &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls",
			ToolCalls: []types.LLMToolCall{{ID: "read", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"invented"}`}}}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "当前没有可读取的文档。", Done: true, FinishReason: "stop"}}},
	}}}
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.chatModel = model
	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`))}
	e.toolRegistry.RegisterTool(reader)
	state, err := e.Execute(t.Context(), "session", "message", "TapTap sandbox configuration", nil)
	require.NoError(t, err)
	require.Equal(t, "当前没有可读取的文档。", state.FinalAnswer)
	require.Empty(t, reader.calls)
	require.Len(t, model.calls, 2)
	for _, message := range model.calls[1] {
		require.NotContains(t, message.Content, reviewRetrievalTool)
		require.NotContains(t, message.Content, "action=initial")
	}
	require.Equal(t, 1, model.boundaryCalls)
}

func TestRuntimeRetrievalMissingUserConditionsBlockFirstSearch(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"needs_user","missing_user_fields":["扣款情况","订单号"],"reason":"payment investigation requires these user facts"}`}
	call := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"payment missing"}`)
	require.False(t, call.Result.Success)
	require.Empty(t, search.calls)
	require.Equal(t, retrievalNeedsUser, e.retrieval.snapshot().Gaps[0].Status)
	require.False(t, e.usesRuntimeRetrieval())
}

func TestRuntimeRetrievalAnsweredConditionGetsBranchAdmission(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	for i := 0; i < questionSearchGrace; i++ {
		require.True(t, retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"query-%d"}`, i)).Result.Success)
	}
	require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", Status: retrievalNeedsUser, MissingUserFields: []string{"channel"}}))
	raw, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	next, search := newRuntimeRetrievalProbeEngine(t)
	next.EnableRuntimeRetrieval()
	next.SetRetrievalUserMessage("channel-answer", "iOS")
	next.beginTurn("iOS", nil, &types.AgentState{})
	next.SetRetrievalPersistence(raw, nil)
	next.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"continue","reason":"the user supplied the missing channel","user_answers":[{"gap_id":"gap-1","field":"channel","input_id":"message:channel-answer","quote":"iOS"}]}`}
	require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
	model := &runtimeRetrievalChat{mockChat: &mockChat{}}
	next.chatModel = model
	for i := 0; i < questionSearchGrace; i++ {
		call := retrievalProbeCall(next, fmt.Sprintf("ios-%d", i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"iOS configuration %d"}`, i))
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Equal(t, 1, model.boundaryCalls)
	require.Len(t, search.calls, questionSearchGrace)
	require.Len(t, next.retrieval.snapshot().Gaps, 2)
}

// The continuation guard is model output: answers for a gap that is not
// blocked on the user must be ignored, not fail the whole turn.
func TestRuntimeRetrievalContinuationIgnoresAnswersForUnblockedGap(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"payment setup"}`).Result.Success)
	require.Equal(t, retrievalPending, e.retrieval.snapshot().Gaps[0].Status)
	raw, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	next, _ := newRuntimeRetrievalProbeEngine(t)
	next.EnableRuntimeRetrieval()
	next.SetRetrievalUserMessage("followup", "iOS")
	next.beginTurn("iOS", nil, &types.AgentState{})
	next.SetRetrievalPersistence(raw, nil)
	next.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"continue","reason":"same goal","user_answers":[{"gap_id":"gap-1","field":"channel","input_id":"message:followup","quote":"iOS"}]}`}
	require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
	gap := next.retrieval.snapshot().Gaps[0]
	require.Equal(t, retrievalPending, gap.Status)
	require.Empty(t, gap.UserAnswers)
}

// Continuation answers are applied one by one: an invalid field must not drop
// a valid answer to another field of the same gap.
func TestRuntimeRetrievalContinuationKeepsValidAnswerBesideInvalidOne(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	for i := 0; i < questionSearchGrace; i++ {
		require.True(t, retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"query-%d"}`, i)).Result.Success)
	}
	require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", Status: retrievalNeedsUser, MissingUserFields: []string{"channel", "version"}}))
	raw, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	next, _ := newRuntimeRetrievalProbeEngine(t)
	next.EnableRuntimeRetrieval()
	next.SetRetrievalUserMessage("channel-answer", "iOS")
	next.beginTurn("iOS", nil, &types.AgentState{})
	next.SetRetrievalPersistence(raw, nil)
	next.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"continue","reason":"the user supplied the channel","user_answers":[{"gap_id":"gap-1","field":"channel","input_id":"message:channel-answer","quote":"iOS"},{"gap_id":"gap-1","field":"version","input_id":"message:channel-answer","quote":"1.0.3"}]}`}
	require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
	gap := next.retrieval.snapshot().Gaps[0]
	require.Equal(t, retrievalNeedsUser, gap.Status)
	require.Equal(t, []string{"version"}, gap.MissingUserFields)
	require.Len(t, gap.UserAnswers, 1)
}
