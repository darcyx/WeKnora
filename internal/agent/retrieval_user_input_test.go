package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/approval"
	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestRetrievalUserConditionsRequireSemanticallyValidAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, input, quote, reply string
		accept                    bool
	}{
		{"refusal", "没有订单号，直接查", "没有订单号", `{"decision":"stop","reason":"the order number is unavailable"}`, false},
		{"valid", "订单号 ORDER-123", "ORDER-123", `{"decision":"allow","reason":"the order number is supplied"}`, true},
		{"invalid decision", "订单号 ORDER-123", "ORDER-123", `not-json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := registeredMissingFields(t, "订单号")
			e.EnableRetrievalGuard()
			e.retrieval.recordUserInput("message:u2", tc.input)
			model := &projectContextChat{mockChat: &mockChat{}, reply: tc.reply}
			e.chatModel = model
			before := e.retrieval.snapshot()
			err := e.applyRetrievalReview(t.Context(), retrievalReview{GapID: "gap-1", Status: retrievalUserConditions,
				UserAnswers: []retrievalUserAnswer{{Field: "订单号", InputID: "message:u2", Quote: tc.quote}}})
			if tc.accept {
				require.NoError(t, err)
				require.Equal(t, retrievalPending, e.retrieval.snapshot().Gaps[0].Status)
			} else {
				require.Error(t, err)
				require.Equal(t, before, e.retrieval.snapshot(), "rejected or unavailable decisions cannot clear blockers")
			}
			require.Len(t, model.calls, 1)
			var payload struct {
				Input struct {
					UserInputs map[string]string `json:"user_inputs"`
				} `json:"input"`
			}
			messages := model.messages
			require.NoError(t, json.Unmarshal([]byte(messages[len(messages)-1].Content[len("review\n"):]), &payload))
			require.Equal(t, tc.input, payload.Input.UserInputs["message:u2"])
		})
	}
}

func TestRetrievalUserConditionsResumeOnlyAfterAllNecessaryAnswers(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	e.beginTurn("支付未到账", nil, &types.AgentState{})
	clarify := retrievalProbeCall(e, "clarify", reviewRetrievalTool, `{
		"new_fact":{"goal_quote":"支付未到账","fact":"支付到账情况"},
		"status":"needs_user","missing_user_fields":["扣款情况","订单号"]
	}`)
	require.True(t, clarify.Result.Success, clarify.Result.Error)
	e.recordSteerMessage("u1", "已经扣款")
	partial := retrievalProbeCall(e, "partial", reviewRetrievalTool, `{
		"gap_id":"gap-1","status":"user_conditions",
		"user_answers":[{"field":"扣款情况","input_id":"message:u1","quote":"已经扣款"}]
	}`)
	require.True(t, partial.Result.Success, partial.Result.Error)
	s := e.retrieval.snapshot()
	require.Equal(t, retrievalNeedsUser, s.Gaps[0].Status)
	require.Equal(t, []string{"订单号"}, s.Gaps[0].MissingUserFields)
	blocked := retrievalProbeCall(e, "premature", agenttools.ToolSearchKnowledge,
		`{"query":"支付到账","retrieval":{"gap_id":"gap-1","action":"initial"}}`)
	require.False(t, blocked.Result.Success)
	require.Empty(t, probe.calls)

	encoded, err := json.Marshal(s)
	require.NoError(t, err)
	var saved retrievalState
	require.NoError(t, json.Unmarshal(encoded, &saved))
	e.retrieval, err = restoreRetrievalControl(saved)
	require.NoError(t, err)
	e.recordSteerMessage("u2", "订单号 ORDER-123")
	complete := retrievalProbeCall(e, "complete", reviewRetrievalTool, `{
		"gap_id":"gap-1","status":"user_conditions",
		"user_answers":[{"field":"订单号","input_id":"message:u2","quote":"ORDER-123"}]
	}`)
	require.True(t, complete.Result.Success, complete.Result.Error)
	s = e.retrieval.snapshot()
	require.Equal(t, retrievalPending, s.Gaps[0].Status)
	require.Empty(t, s.Gaps[0].MissingUserFields)
	require.Empty(t, s.Attempts, "answering is not another search")
	call := retrievalProbeCall(e, "ready", agenttools.ToolSearchKnowledge,
		`{"query":"支付到账处理规则","retrieval":{"gap_id":"gap-1","action":"initial"}}`)
	require.True(t, call.Result.Success, call.Result.Error)
	require.Len(t, probe.calls, 1)
}

func TestRetrievalUserConditionsRejectUngroundedOrUnrelatedUpdatesAtomically(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	clarify := retrievalProbeCall(e, "clarify", reviewRetrievalTool,
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user","missing_user_fields":["platform","engine"]}`)
	require.True(t, clarify.Result.Success)
	e.recordSteerMessage("u1", "iOS and Unity")
	for _, args := range []string{
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[]}`,
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"platform","input_id":"message:missing","quote":"iOS"}]}`,
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"platform","input_id":"message:u1","quote":"Android"}]}`,
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"channel","input_id":"message:u1","quote":"iOS"}]}`,
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"platform","input_id":"message:u1","quote":"iOS"},{"field":"platform","input_id":"message:u1","quote":"iOS"}]}`,
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"platform","input_id":"message:u1","quote":"iOS"},{"field":"engine","input_id":"message:u1","quote":"C++"}]}`,
		`{"gap_id":"gap-1","status":"pending","user_answers":[{"field":"platform","input_id":"message:u1","quote":"iOS"}]}`,
	} {
		t.Run(args, func(t *testing.T) {
			before := e.retrieval.snapshot()
			call := retrievalProbeCall(e, "bad", reviewRetrievalTool, args)
			require.False(t, call.Result.Success)
			require.Equal(t, before, e.retrieval.snapshot())
			require.Empty(t, probe.calls)
		})
	}
}

func TestRetrievalUserConditionsPreserveSpentAllowances(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","retrieval":{"goal_quote":"TapTap","fact":"configuration","action":"initial"}}`)
	require.True(t, first.Result.Success)
	probe.err = context.DeadlineExceeded
	supplement := retrievalProbeCall(e, "supplement", agenttools.ToolSearchKnowledge,
		`{"query":"TapTap","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"TapTap","review":{"call_id":"first","status":"no_progress"}}}`)
	require.False(t, supplement.Result.Success)
	probe.err = nil
	recovery := retrievalProbeCall(e, "recovery", agenttools.ToolSearchKnowledge,
		`{"query":"TapTap","retrieval":{"gap_id":"gap-1","action":"recovery"}}`)
	require.True(t, recovery.Result.Success)
	clarify := retrievalProbeCall(e, "clarify", reviewRetrievalTool,
		`{"gap_id":"gap-1","status":"needs_user","missing_user_fields":["error message"]}`)
	require.True(t, clarify.Result.Success)
	require.True(t, e.retrieval.takeDecisionCorrection())
	e.recordSteerMessage("u1", "the error is E123")
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"the error message is supplied"}`}
	answered := retrievalProbeCall(e, "answered", reviewRetrievalTool,
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"error message","input_id":"message:u1","quote":"E123"}]}`)
	require.True(t, answered.Result.Success, answered.Result.Error)
	s := e.retrieval.snapshot()
	require.True(t, s.Gaps[0].SupplementUsed)
	require.True(t, s.Gaps[0].RecoveryUsed)
	require.True(t, s.DecisionCorrectionUsed)
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"reuse","gap_id":"gap-1","reason":"the supplied error answers the existing fact, not a new applicability branch"}`}
	blocked := retrievalProbeCall(e, "repeat", agenttools.ToolSearchKnowledge,
		`{"query":"E123","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"E123","review":{"call_id":"recovery","status":"no_progress"}}}`)
	require.False(t, blocked.Result.Success)
	require.Len(t, probe.calls, 3, "an answer must not refund either retrieval allowance")
	reopen := retrievalProbeCall(e, "reopen", reviewRetrievalTool,
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"error message","input_id":"message:u1","quote":"E123"}]}`)
	require.False(t, reopen.Result.Success, "user_conditions cannot reopen an exhausted fact")
}

func TestRetrievalAskUserInputRequiresAnAcceptedAnswerAndCompletedBatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		call types.ToolCall
		want bool
	}{
		{"answered", types.ToolCall{Name: agenttools.ToolAskUser, Result: &types.ToolResult{Success: true, Data: map[string]any{
			"answered": true, "answer": types.AskUserPromptResponse{Values: map[string]any{"platform": "iOS"}},
		}}}, true},
		{"declined", types.ToolCall{Name: agenttools.ToolAskUser, Result: &types.ToolResult{Success: true, Data: map[string]any{
			"answered": true, "answer": types.AskUserPromptResponse{Text: "iOS", Declined: true},
		}}}, false},
		{"timeout", types.ToolCall{Name: agenttools.ToolAskUser, Result: &types.ToolResult{Success: true, Data: map[string]any{
			"answered": false, "timed_out": true, "answer": types.AskUserPromptResponse{Text: "iOS"},
		}}}, false},
		{"unsupported client", types.ToolCall{Name: agenttools.ToolAskUser, Result: &types.ToolResult{Success: true, Output: "Ask directly. iOS"}}, false},
		{"empty", types.ToolCall{Name: agenttools.ToolAskUser, Result: &types.ToolResult{Success: true, Data: map[string]any{
			"answered": true, "answer": types.AskUserPromptResponse{},
		}}}, false},
		{"not user input", types.ToolCall{Name: agenttools.ToolSearchKnowledge, Result: &types.ToolResult{Success: true, Data: map[string]any{
			"answered": true, "answer": types.AskUserPromptResponse{Text: "iOS"},
		}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, probe := newRetrievalProbeEngine(t)
			clarify := retrievalProbeCall(e, "clarify", reviewRetrievalTool,
				`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user","missing_user_fields":["platform"]}`)
			require.True(t, clarify.Result.Success)
			args := `{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"platform","input_id":"ask_user:ask","quote":"iOS"}]}`
			premature := retrievalProbeCall(e, "premature", reviewRetrievalTool, args)
			require.False(t, premature.Result.Success, "a future answer cannot be preconstructed")
			tc.call.ID = "ask"
			e.appendToolResults(emptyMessages(), types.AgentStep{ToolCalls: []types.ToolCall{tc.call}})
			accepted := retrievalProbeCall(e, "accepted", reviewRetrievalTool, args)
			require.Equal(t, tc.want, accepted.Result.Success, accepted.Result.Error)
			require.Empty(t, probe.calls)
		})
	}
}

func TestRetrievalUserConditionsSnapshotKeepsProvenanceWithoutCopyingInputs(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	clarify := retrievalProbeCall(e, "clarify", reviewRetrievalTool,
		`{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user","missing_user_fields":["platform"]}`)
	require.True(t, clarify.Result.Success)
	e.recordSteerMessage("u1", "Unrelated conversation detail. iOS")
	answered := retrievalProbeCall(e, "answered", reviewRetrievalTool,
		`{"gap_id":"gap-1","status":"user_conditions","user_answers":[{"field":"platform","input_id":"message:u1","quote":"iOS"}]}`)
	require.True(t, answered.Result.Success)
	encoded, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Unrelated conversation detail")
	var saved retrievalState
	require.NoError(t, json.Unmarshal(encoded, &saved))
	restored, err := restoreRetrievalControl(saved)
	require.NoError(t, err)
	require.Equal(t, retrievalPending, restored.snapshot().Gaps[0].Status)
	require.Empty(t, restored.availableUserInputs(), "a source ID alone is not available user text")
	require.Equal(t, "iOS", restored.snapshot().Gaps[0].UserAnswers[0].Quote)
	copy := e.retrieval.snapshot()
	copy.UserInputIDs[0], copy.Gaps[0].UserAnswers[0].Quote = "changed", "changed"
	require.Equal(t, "iOS", e.retrieval.snapshot().Gaps[0].UserAnswers[0].Quote)
	require.Equal(t, "message:u1", e.retrieval.snapshot().UserInputIDs[0])
	for _, mutate := range []func(*retrievalState){
		func(s *retrievalState) { s.UserInputIDs = nil },
		func(s *retrievalState) { s.UserInputIDs = append(s.UserInputIDs, s.UserInputIDs[0]) },
		func(s *retrievalState) { s.Gaps[0].UserAnswers[0].InputID = "message:unknown" },
		func(s *retrievalState) { s.Gaps[0].UserAnswers[0].Quote = " " },
		func(s *retrievalState) { s.Gaps[0].MissingUserFields = []string{"platform"} },
	} {
		s := e.retrieval.snapshot()
		mutate(&s)
		_, err := restoreRetrievalControl(s)
		require.Error(t, err)
	}
	known := retrievalProbeCall(e, "known", reviewRetrievalTool,
		`{"gap_id":"gap-1","status":"needs_user","missing_user_fields":["platform"]}`)
	require.False(t, known.Result.Success, "known conditions cannot be asked repeatedly")
}

type retrievalAsker struct {
	snapshot []byte
	resumes  int
}

func (a *retrievalAsker) AskUser(_ context.Context, request approval.AskRequest) (approval.Answer, error) {
	a.snapshot = request.Snapshot
	return approval.Answer{Suspended: &types.PromptSuspension{PromptID: "p1", Kind: types.AgentPromptKindAskUser}}, nil
}

func (a *retrievalAsker) ResumeAnswer(_ context.Context, prompt *types.AgentPrompt, _ *event.EventBus) approval.Answer {
	a.resumes++
	var response types.AskUserPromptResponse
	if json.Unmarshal(prompt.Response, &response) != nil {
		return approval.Answer{}
	}
	return approval.Answer{Answered: !response.Declined, Response: response}
}

func TestRetrievalUserConditionsThroughRealAskUserSuspensionAndResume(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	asker := &retrievalAsker{}
	e.toolRegistry.RegisterTool(agenttools.NewAskUserTool(asker))
	e.SetRunMode(types.RunModeAttended)
	e.EnableSuspension(json.RawMessage(`{}`))
	searchArgs := `{"query":"iOS sandbox","retrieval":{"gap_id":"gap-1","action":"initial","review":{
		"status":"user_conditions","user_answers":[{"field":"platform","input_id":"ask_user:ask","quote":"iOS"}]
	}}}`
	e.chatModel = &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls",
		ToolCalls: []types.LLMToolCall{
			{ID: "clarify", Function: types.FunctionCall{Name: reviewRetrievalTool, Arguments: `{"new_fact":{"goal_quote":"TapTap","fact":"configuration"},"status":"needs_user","missing_user_fields":["platform"]}`}},
			{ID: "ask", Function: types.FunctionCall{Name: agenttools.ToolAskUser, Arguments: `{"question":"Which platform?"}`}},
			{ID: "preconstructed", Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: searchArgs}},
		},
	}}}}}
	state := &types.AgentState{}
	e.beginTurn("TapTap sandbox configuration", nil, state)
	state, err := e.executeLoop(t.Context(), state, "TapTap sandbox configuration", emptyMessages(), e.buildToolsForLLM(), "session", "message")
	require.NoError(t, err)
	require.NotNil(t, state.Suspended)
	require.NotEmpty(t, asker.snapshot)

	next, probe := newRetrievalProbeEngine(t)
	next.toolRegistry.RegisterTool(agenttools.NewAskUserTool(asker))
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls",
			ToolCalls: []types.LLMToolCall{{ID: "regenerated", Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: searchArgs}}}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "done", Done: true, FinishReason: "stop"}}},
	}}
	next.chatModel = model
	state, err = next.Resume(t.Context(), "session", "message", nil, asker.snapshot, &types.AgentPrompt{
		ID: "p1", Kind: types.AgentPromptKindAskUser, ToolCallID: "ask", Status: types.AgentPromptAnswered,
		Response: types.JSON(`{"text":"iOS"}`),
	})
	require.NoError(t, err)
	require.Equal(t, 1, asker.resumes)
	require.False(t, state.RoundSteps[0].ToolCalls[2].Result.Success, "a call saved before the answer remains blocked")
	require.Len(t, probe.calls, 1, "only the call regenerated after observing the answer may execute")
	require.Equal(t, next.retrievalCallReference("regenerated"), next.retrieval.snapshot().Attempts[0].ID)
	require.Equal(t, "regenerated", state.RoundSteps[1].ToolCalls[0].ID, "provider IDs are preserved across resume")
	require.Equal(t, "iOS", next.retrieval.snapshot().Gaps[0].UserAnswers[0].Quote)
}
