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
			published := ""
			e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
				data := ev.Data.(event.AgentFinalAnswerData)
				if data.Content != "" {
					questions++
					require.Equal(t, "请提供订单号。", data.Content)
					published = data.PublishedQuestion
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
			require.NotEmpty(t, published)
			steps := agenttools.SanitizeAgentStepsForStorage(state.RoundSteps)
			require.Equal(t, published, steps[len(steps)-1].ToolCalls[0].Result.Data["published_question"])
			encoded, err := json.Marshal(steps)
			require.NoError(t, err)
			var restored []types.AgentStep
			require.NoError(t, json.Unmarshal(encoded, &restored))
			require.Equal(t, published, restored[len(restored)-1].ToolCalls[0].Result.Data["published_question"])
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

func TestAskUserDoesNotMarkQuestionWhenPublicationFails(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	e.eventBus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error { return errors.New("stream failed") })
	step := types.AgentStep{ToolCalls: []types.ToolCall{{Name: agenttools.ToolAskUser, Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请选择平台？", Options: []string{"Android", "iOS"}})}}}
	state := &types.AgentState{RoundSteps: []types.AgentStep{step}}
	messages := []chat.Message{}
	finished, err := e.finishUserQuestion(t.Context(), state, step, &messages, "session", "message")
	require.Error(t, err)
	require.False(t, finished)
	require.NotContains(t, state.RoundSteps[0].ToolCalls[0].Result.Data, "published_question")
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

func TestAskUserPreservesAssistantAnswerPrelude(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	state := &types.AgentState{}
	messages := []chat.Message{}
	step := types.AgentStep{
		Thought: "<think>internal reasoning</think>\n1. 先在平台订单查询核对订单状态。\n2. 确认发货通知是否送达游戏服务端。",
		ToolCalls: []types.ToolCall{{
			Name:   agenttools.ToolAskUser,
			Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请补充订单号和渠道。"}),
		}},
	}
	finished, err := e.finishUserQuestion(t.Context(), state, step, &messages, "session", "message")
	require.NoError(t, err)
	require.True(t, finished)
	require.True(t, state.IsComplete)
	require.NotContains(t, state.FinalAnswer, "internal reasoning")
	require.Contains(t, state.FinalAnswer, "先在平台订单查询核对订单状态")
	require.Contains(t, state.FinalAnswer, "请补充订单号和渠道。")
}

func TestAskUserStripsFollowUpButtonsFromPrelude(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	state := &types.AgentState{}
	messages := []chat.Message{}
	step := types.AgentStep{
		Thought: "先核对渠道配置。\n\n## 相关问题\n1. <button-send>Android 怎么接</button-send>\n2. <button-send>iOS 怎么接</button-send>",
		ToolCalls: []types.ToolCall{{
			Name:   agenttools.ToolAskUser,
			Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请问您接入的是哪个平台？"}),
		}},
	}
	finished, err := e.finishUserQuestion(t.Context(), state, step, &messages, "session", "message")
	require.NoError(t, err)
	require.True(t, finished)
	require.Equal(t, "先核对渠道配置。\n\n请问您接入的是哪个平台？", state.FinalAnswer)
	require.NotContains(t, state.FinalAnswer, "<button-send>")
	require.NotContains(t, state.FinalAnswer, "相关问题")
	rawPublished, ok := step.ToolCalls[0].Result.Data["published_question"].(string)
	require.True(t, ok)
	require.NotContains(t, rawPublished, "<button-send>")
}
