package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Tencent/WeKnora/internal/agent/collaboration"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestAskUserEndsTurnWithoutAnotherModelCall(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(map[bool]string{false: "sequential", true: "parallel"}[parallel], func(t *testing.T) {
			model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{
				ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls",
				ToolCalls: []types.LLMToolCall{
					{ID: "ask", Function: types.FunctionCall{Name: agenttools.ToolAskUser, Arguments: `{"question":"请提供订单号。"}`}},
					{ID: "dependent", Function: types.FunctionCall{Name: "dependent_action", Arguments: `{}`}},
				},
			}}}}}
			e := newTestEngine(t, model)
			e.config.ParallelToolCalls = parallel
			e.toolRegistry = agenttools.NewToolRegistry()
			e.toolRegistry.RegisterTool(agenttools.NewAskUserTool(nil))
			dependent := newCountingTool("dependent_action")
			e.toolRegistry.RegisterTool(dependent)
			questions, complete, prompts := 0, 0, 0
			e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
				data := ev.Data.(event.AgentFinalAnswerData)
				if data.Content != "" {
					questions++
					require.Equal(t, "请提供订单号。", data.Content)
				}
				return nil
			})
			e.eventBus.On(event.EventAgentComplete, func(context.Context, event.Event) error { complete++; return nil })
			e.eventBus.On(event.EventAgentPromptRequired, func(context.Context, event.Event) error { prompts++; return nil })
			state, err := e.Execute(t.Context(), "session", "message", "支付未到账", nil)
			require.NoError(t, err)
			require.True(t, state.IsComplete)
			require.Equal(t, "请提供订单号。", state.FinalAnswer)
			require.Nil(t, state.Suspended)
			require.Equal(t, 1, model.callCount)
			require.Zero(t, dependent.calls, "no dependent operation may run after asking")
			require.Equal(t, 1, questions)
			require.Equal(t, 1, complete)
			require.Zero(t, prompts)
		})
	}
}

func TestAskUserDocumentBranchCanBeReadBeforeQuestion(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: "read", Function: types.FunctionCall{Name: "read_branch", Arguments: `{}`}}}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: "ask", Function: types.FunctionCall{Name: agenttools.ToolAskUser, Arguments: `{"question":"文档区分了平台，当前是 Android 还是 iOS？"}`}}}}}},
	}}
	e := newTestEngine(t, model)
	e.toolRegistry = agenttools.NewToolRegistry()
	reader := newCountingTool("read_branch")
	e.toolRegistry.RegisterTool(reader)
	e.toolRegistry.RegisterTool(agenttools.NewAskUserTool(nil))
	state, err := e.Execute(t.Context(), "session", "message", "如何配置？", nil)
	require.NoError(t, err)
	require.Equal(t, 1, reader.calls)
	require.Equal(t, 2, model.callCount)
	require.Equal(t, "文档区分了平台，当前是 Android 还是 iOS？", state.FinalAnswer)
}

// Keep generating a historical suspended run to exercise upgrade compatibility.
type legacyAskUserTool struct {
	*agenttools.AskUserTool
	asker *retrievalAsker
}

func newLegacyAskUserTool(asker *retrievalAsker) *legacyAskUserTool {
	return &legacyAskUserTool{AskUserTool: agenttools.NewAskUserTool(asker), asker: asker}
}
func (t *legacyAskUserTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	meta, _ := agenttools.ToolExecFromContext(ctx)
	if meta.Answered != nil {
		return t.AskUserTool.Execute(ctx, args)
	}
	snapshot, _, err := meta.Snapshot()
	if err != nil {
		return nil, err
	}
	t.asker.snapshot = snapshot
	return &types.ToolResult{Suspend: &types.PromptSuspension{PromptID: "p1", Kind: types.AgentPromptKindAskUser}}, nil
}

func TestAskUserReachedDuringLegacyResumeEndsWithoutDependentWork(t *testing.T) {
	model := &mockChat{}
	e := newTestEngine(t, model)
	e.toolRegistry = agenttools.NewToolRegistry()
	e.toolRegistry.RegisterTool(agenttools.NewAskUserTool(nil))
	approved := newCountingTool("approved_action")
	dependent := newCountingTool("dependent_action")
	e.toolRegistry.RegisterTool(approved)
	e.toolRegistry.RegisterTool(dependent)
	snap := RunSnapshot{Version: RunSnapshotVersion, Query: "支付未到账", PendingRound: PendingRound{Calls: []PendingCall{
		{ID: "approved", Name: "approved_action", Arguments: `{}`},
		{ID: "ask", Name: agenttools.ToolAskUser, Arguments: `{"question":"请提供订单号。"}`},
		{ID: "dependent", Name: "dependent_action", Arguments: `{}`},
	}}}
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	completed := 0
	e.eventBus.On(event.EventAgentComplete, func(context.Context, event.Event) error { completed++; return nil })
	state, err := e.Resume(t.Context(), "session", "message", nil, raw, &types.AgentPrompt{ToolCallID: "approved", Status: types.AgentPromptAnswered})
	require.NoError(t, err)
	require.True(t, state.IsComplete)
	require.Equal(t, "请提供订单号。", state.FinalAnswer)
	require.Nil(t, state.Suspended)
	require.Zero(t, model.callCount)
	require.Equal(t, 1, approved.calls)
	require.Zero(t, dependent.calls)
	require.Equal(t, 1, completed)
}

func TestAskUserDefersToCollaborationFinishProtocol(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	e.collaboration = &collaboration.Binding{}
	messages := []chat.Message{}
	state := &types.AgentState{}
	step := types.AgentStep{ToolCalls: []types.ToolCall{{Name: agenttools.ToolAskUser, Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请提供订单号。"})}}}
	finished, err := e.finishUserQuestion(t.Context(), state, step, &messages, "session", "message")
	require.NoError(t, err)
	require.False(t, finished)
	require.False(t, state.IsComplete)
	require.Empty(t, state.FinalAnswer)
	require.Contains(t, messages[0].Content, "collect delegated results")
}

func TestAskUserSteerAtFinalIterationGetsExecutionRound(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: "ask", Function: types.FunctionCall{Name: agenttools.ToolAskUser, Arguments: `{"question":"请提供订单号。"}`}}}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "已收到订单号 ORDER-123。", Done: true, FinishReason: "stop"}}},
	}}
	e := newTestEngine(t, model)
	e.config.MaxIterations = 1
	e.toolRegistry = agenttools.NewToolRegistry()
	e.toolRegistry.RegisterTool(agenttools.NewAskUserTool(nil))
	sink := &fakeSteerSink{}
	e.SetSteerSink(sink)
	e.eventBus.On(event.EventAgentToolResult, func(context.Context, event.Event) error {
		sink.queued = append(sink.queued, steerEntry("answer", "ORDER-123"))
		return nil
	})
	state, err := e.Execute(t.Context(), "session", "message", "支付未到账", nil)
	require.NoError(t, err)
	require.Equal(t, 2, model.callCount)
	require.Equal(t, "已收到订单号 ORDER-123。", state.FinalAnswer)
	require.Contains(t, model.calls[1][len(model.calls[1])-1].Content, "ORDER-123")
}

func TestAskUserRetainsIndependentRequests(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	goal := "TapTap sandbox configuration"
	_, err := e.retrieval.register(retrievalFactProposal{GoalQuote: goal, Fact: goal})
	require.NoError(t, err)
	require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", Status: retrievalNeedsUser, MissingUserFields: []string{"订单号"}, independentRequests: []string{"sandbox configuration"}}))
	state := &types.AgentState{}
	messages := []chat.Message{}
	step := types.AgentStep{ToolCalls: []types.ToolCall{{Name: agenttools.ToolAskUser, Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请提供订单号。"})}}}
	finished, err := e.finishUserQuestion(t.Context(), state, step, &messages, "session", "message")
	require.NoError(t, err)
	require.False(t, finished)
	require.False(t, state.IsComplete)
	require.Contains(t, messages[0].Content, "independent work")
}

func TestAskUserDeliveryFailureStillCompletesRunOnce(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: "ask", Function: types.FunctionCall{Name: agenttools.ToolAskUser, Arguments: `{"question":"请提供订单号。"}`}}}}}}}}
	e := newTestEngine(t, model)
	e.toolRegistry = agenttools.NewToolRegistry()
	e.toolRegistry.RegisterTool(agenttools.NewAskUserTool(nil))
	delivered := errors.New("delivery failed")
	completed := 0
	e.eventBus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error { return delivered })
	e.eventBus.On(event.EventAgentComplete, func(context.Context, event.Event) error { completed++; return nil })
	_, err := e.Execute(t.Context(), "session", "message", "支付未到账", nil)
	require.ErrorIs(t, err, delivered)
	require.Equal(t, 1, completed)
}

func TestAskUserNextUserMessageContinuesOrdinaryConversation(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: "ask", Function: types.FunctionCall{Name: agenttools.ToolAskUser, Arguments: `{"question":"请提供订单号。"}`}}}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "已收到订单号。", Done: true, FinishReason: "stop"}}},
	}}
	e := newTestEngine(t, model)
	e.toolRegistry = agenttools.NewToolRegistry()
	e.toolRegistry.RegisterTool(agenttools.NewAskUserTool(nil))
	first, err := e.Execute(t.Context(), "session", "message-1", "支付未到账", nil)
	require.NoError(t, err)
	second, err := e.Execute(t.Context(), "session", "message-2", "ORDER-123", []chat.Message{{Role: "user", Content: "支付未到账"}, {Role: "assistant", Content: first.FinalAnswer}})
	require.NoError(t, err)
	require.Equal(t, "已收到订单号。", second.FinalAnswer)
	require.Equal(t, 2, model.callCount)
	require.Nil(t, second.Suspended)
	found := false
	for _, msg := range model.calls[1] {
		if msg.Role == "assistant" && msg.Content == first.FinalAnswer {
			found = true
		}
	}
	require.True(t, found, "question must remain available as ordinary conversation context")
}

func TestAskUserResolvesKnownProjectFields(t *testing.T) {
	e := registeredMissingFields(t, "渠道", "订单号")
	e.EnableRetrievalGuard()
	e.appInfo = `{"channel":"微信小游戏"}`
	raw, _ := json.Marshal(retrievalAdmission{Decision: "continue", Reason: "channel supplied by app context", UserAnswers: []retrievalConditionAnswer{{GapID: "gap-1", retrievalUserAnswer: retrievalUserAnswer{Field: "渠道", InputID: e.projectInputID(), Quote: "微信小游戏"}}}})
	model := &projectContextChat{mockChat: &mockChat{}, reply: string(raw)}
	e.chatModel = model
	state := &types.AgentState{}
	messages := []chat.Message{}
	step := types.AgentStep{ToolCalls: []types.ToolCall{{Name: agenttools.ToolAskUser, Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "能发一下订单号吗？"})}}}
	finished, err := e.finishUserQuestion(t.Context(), state, step, &messages, "session", "message")
	require.NoError(t, err)
	require.True(t, finished)
	require.Equal(t, "方便提供一下订单号吗？", state.FinalAnswer, "guard calls=%d", len(model.calls))
}

func TestAskUserPlanPreservesIndependentDeliverable(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Read the independent reference and preserve its summary","Identify the user's missing target document"]}`),
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: "read", Function: types.FunctionCall{Name: "read_reference", Arguments: `{}`}}}}}},
		plannerResponse(`{"status":"completed","result":"Reference summary: configure region before deployment."}`),
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: "ask", Function: types.FunctionCall{Name: agenttools.ToolAskUser, Arguments: `{"question":"请提供需要修改的文档。"}`}}}}}},
		plannerResponse(`{"status":"blocked","result":"The target document is missing."}`),
		plannerResponse("Reference summary: configure region before deployment. 请提供需要修改的文档。"),
	}}
	e := newTestEngine(t, model, withPlanningEnabled(true))
	e.toolRegistry = agenttools.NewToolRegistry()
	e.toolRegistry.RegisterTool(newCountingTool("read_reference"))
	e.toolRegistry.RegisterTool(agenttools.NewAskUserTool(nil))
	state, err := e.Execute(t.Context(), "session", "message", "Summarize the reference and also revise my document", nil)
	require.NoError(t, err)
	t.Logf("calls=%d plan=%+v final=%q", model.callCount, state.Plan, state.FinalAnswer)
	require.Contains(t, state.FinalAnswer, "Reference summary:", "completed independent deliverable must survive final publication")
	require.Equal(t, len(state.Plan.Steps), state.Plan.Current)
}

func TestAskUserContinuesWhenProjectResolvesEveryBlocker(t *testing.T) {
	e := registeredMissingFields(t, "渠道")
	e.EnableRetrievalGuard()
	e.appInfo = `{"channel":"微信小游戏"}`
	raw, err := json.Marshal(retrievalAdmission{Decision: "continue", Reason: "channel supplied by app context", UserAnswers: []retrievalConditionAnswer{{GapID: "gap-1", retrievalUserAnswer: retrievalUserAnswer{Field: "渠道", InputID: e.projectInputID(), Quote: "微信小游戏"}}}})
	require.NoError(t, err)
	model := &projectContextChat{mockChat: &mockChat{}, reply: string(raw)}
	e.chatModel = model
	state := &types.AgentState{}
	messages := []chat.Message{}
	step := types.AgentStep{ToolCalls: []types.ToolCall{{Name: agenttools.ToolAskUser, Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请问是哪个渠道？"})}}}
	finished, err := e.finishUserQuestion(t.Context(), state, step, &messages, "session", "message")
	require.NoError(t, err)
	require.False(t, finished)
	require.False(t, state.IsComplete)
	require.Empty(t, state.FinalAnswer)
	require.Empty(t, (&retrievalControl{state: e.retrieval.snapshot()}).missingUserFields())
	require.Len(t, model.calls, 1)
	require.Contains(t, messages[len(messages)-1].Content, "without asking")
}
