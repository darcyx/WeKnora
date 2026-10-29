package agent

import (
	"context"
	"encoding/json"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func plannerResponse(content string) mockResponse {
	return mockResponse{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer,
		Content:      content,
		Done:         true,
		FinishReason: "stop",
	}}}
}

// TestMaybePlanDisabledByDefault verifies the config switch is off unless the
// agent explicitly enables it, so existing agents pay nothing extra.
func TestMaybePlanDisabledByDefault(t *testing.T) {
	engine := newTestEngine(t, &mockChat{}) // PlanningEnabled defaults to false
	plan, ok := engine.maybePlan(context.Background(), "对比 A 和 B 的区别", nil, nil)

	require.False(t, ok)
	require.Empty(t, plan)
}

func TestMaybePlanSkipsSimpleQueryWhenEnabled(t *testing.T) {
	model := &mockChat{responses: []mockResponse{plannerResponse("SIMPLE")}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	plan, ok := engine.maybePlan(context.Background(), "What is WeKnora?", nil, nil)

	require.False(t, ok)
	require.Empty(t, plan)
	require.Len(t, model.calls, 1)
	require.Contains(t, model.calls[0][0].Content, "knowledge search")
	require.Contains(t, model.calls[0][0].Content, "A requested artifact or external operation")
}

func TestPlanInstructionRequestsConciseEvidence(t *testing.T) {
	plan := &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{Description: "Find evidence"}}}
	instruction := planInstruction(plan).Content
	require.Contains(t, instruction, "do not draft the final answer")
	require.Contains(t, instruction, "Preserve material qualifiers, failures, and identifiers")
}

func TestMaybePlanHandlesShortMultiStepTask(t *testing.T) {
	model := &mockChat{responses: []mockResponse{plannerResponse(`{"steps":["查退款规则","核实账号","撰写申诉邮件"]}`)}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	plan, ok := engine.maybePlan(context.Background(), "先查退款规则，再核实账号，最后写申诉邮件", nil, nil)
	require.True(t, ok)
	require.Len(t, plan.Steps, 3)
	require.NotNil(t, model.opts[0].Thinking)
	require.False(t, *model.opts[0].Thinking)
	require.Greater(t, model.opts[0].Temperature, 0.0)
	require.LessOrEqual(t, model.opts[0].Temperature, 0.05)
}

func TestSimpleQuestionFallsBackToNormalExecution(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse("SIMPLE"), plannerResponse("微信小游戏隐私协议配置步骤"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	state, err := engine.Execute(context.Background(), "session", "message", "微信小游戏的隐私协议怎么配置？", nil)
	require.NoError(t, err)
	require.Nil(t, state.Plan)
	require.Equal(t, "微信小游戏隐私协议配置步骤", state.FinalAnswer)
	require.Len(t, model.calls, 2)
}

func TestMaybePlanDecomposesComplexQuery(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Search KB for A","Search KB for B","Compare A and B"]}`),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))

	plan, ok := engine.maybePlan(context.Background(), "对比 A 和 B 的区别", nil, nil)

	require.True(t, ok)
	require.Equal(t, "Compare A and B", plan.Steps[2].Description)
}

func TestMaybePlanTreatsSimpleAsNoPlan(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse("SIMPLE"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))

	plan, ok := engine.maybePlan(context.Background(), "对比 A 和 B 的区别", nil, nil)

	require.False(t, ok)
	require.Empty(t, plan)
}

func TestMaybePlanUsesInstructionsAndRecentHistoryAndCountsUsage(t *testing.T) {
	response := plannerResponse(`{"steps":["Read WeChat","Read Douyin"]}`)
	response.chunks[0].Usage = &types.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	model := &mockChat{responses: []mockResponse{response}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	state := &types.AgentState{}
	messages := []chat.Message{
		{Role: "system", Content: "Stay within the requested channel."},
		{Role: "user", Content: "Compare WeChat and Douyin."},
		{Role: "assistant", Content: "Which feature?"},
		{Role: "user", Content: "Compare their privacy settings. Selected document: WeChat guide."},
	}
	plan, ok := engine.maybePlan(context.Background(), "Compare their privacy settings.", messages, state)
	require.True(t, ok)
	require.Equal(t, "Read WeChat", plan.Steps[0].Description)
	require.Len(t, model.calls, 1)
	require.Equal(t, "Stay within the requested channel.", model.calls[0][0].Content)
	require.Equal(t, "Compare WeChat and Douyin.", model.calls[0][1].Content)
	require.Contains(t, model.calls[0][3].Content, "Selected document: WeChat guide.")
	require.Contains(t, model.calls[0][4].Content, "Compare their privacy settings.")
	require.Equal(t, 15, state.TurnUsage.TotalTokens)
	require.Equal(t, 15, state.Usage.TotalTokens)
}

func TestMaybePlanDiscardsTruncatedPlan(t *testing.T) {
	response := plannerResponse("1. Search for")
	response.chunks[0].FinishReason = "length"
	engine := newTestEngine(t, &mockChat{responses: []mockResponse{response}}, withPlanningEnabled(true))
	plan, ok := engine.maybePlan(context.Background(), "对比 A 和 B", nil, nil)
	require.False(t, ok)
	require.Empty(t, plan)
}

func TestContinuingLoopDoesNotPlanAgain(t *testing.T) {
	model := &mockChat{responses: []mockResponse{plannerResponse("Done")}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	state := &types.AgentState{CurrentRound: 1}
	result, err := engine.executeLoop(context.Background(), state, "对比 A 和 B", emptyMessages(), emptyTools(), "session", "message")
	require.NoError(t, err)
	require.Equal(t, "Done", result.FinalAnswer)
	require.Len(t, model.calls, 1)
}

func TestExecuteRunsPlanBeforeFinalAnswer(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Check A","Check B"]}`),
		plannerResponse(`{"status":"completed","result":"A checked"}`),
		plannerResponse(`{"status":"blocked","result":"B unavailable"}`),
		plannerResponse("Done"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	var visible string
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		visible += evt.Data.(event.AgentFinalAnswerData).Content
		return nil
	})
	state, err := engine.Execute(context.Background(), "session", "message", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.Equal(t, "Done", state.FinalAnswer)
	require.Len(t, model.calls, 4)
	require.Equal(t, "unverified", state.Plan.Steps[0].Status)
	require.Equal(t, "blocked", state.Plan.Steps[1].Status)
	require.Equal(t, "Done", visible)
	require.True(t, state.RoundSteps[0].PlanReport)
	require.True(t, state.RoundSteps[1].PlanReport)
	last := model.calls[3][len(model.calls[3])-1]
	require.Contains(t, last.Content, "B unavailable")
}

func TestPlanSurvivesSuspensionAndResume(t *testing.T) {
	f := newSuspendFixture(t, "first_tool", false)
	f.engine.config.PlanningEnabled = true
	f.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))
	model := f.engine.chatModel.(*mockChat)
	model.responses = append([]mockResponse{plannerResponse(`{"steps":["Check A","Check B"]}`)}, model.responses...)
	state, err := f.engine.Execute(context.Background(), "sess-1", "msg-1", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.NotNil(t, state.Suspended)
	snap, err := DecodeRunSnapshot(f.blocked.snapshot)
	require.NoError(t, err)
	require.Equal(t, "in_progress", snap.Plan.Steps[0].Status)

	resumed := newSuspendFixture(t, "first_tool", false)
	resumed.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))
	resumedModel := resumed.engine.chatModel.(*mockChat)
	resumedModel.responses = []mockResponse{
		plannerResponse(`{"status":"completed","result":"A checked"}`),
		plannerResponse(`{"status":"completed","result":"B checked"}`),
		plannerResponse("Final answer"),
	}
	state, err = resumed.engine.Resume(context.Background(), "sess-1", "msg-1", nil, f.blocked.snapshot, answeredPrompt("call-b"))
	require.NoError(t, err)
	require.Equal(t, "Final answer", state.FinalAnswer)
	require.Equal(t, "completed", state.Plan.Steps[0].Status)
	require.Equal(t, "unverified", state.Plan.Steps[1].Status)
	require.Len(t, resumedModel.calls, 3, "resume must not generate another plan")
	require.Zero(t, resumed.first.calls, "completed tools must not be rerun")
}

func TestPlanStopsAfterBlockedStep(t *testing.T) {
	plan := &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{Description: "A"}, {Description: "B", Status: "pending"}}}
	completePlanStep(plan, `{"status":"blocked","result":"A unavailable"}`)
	require.Equal(t, len(plan.Steps), plan.Current)
	require.Equal(t, "pending", plan.Steps[1].Status)
	require.Contains(t, planInstruction(plan).Content, "unfinished work")
}

func TestPlanBudgetExhaustionReportsUnfinishedSteps(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Check A","Check B"]}`),
		plannerResponse(`{"status":"completed","result":"A checked"}`),
		plannerResponse("Partial answer"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true), withMaxIterations(1))
	engine.toolRegistry = agenttools.NewToolRegistry()
	state, err := engine.Execute(context.Background(), "session", "message", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.Equal(t, "Partial answer", state.FinalAnswer)
	require.Equal(t, "in_progress", state.Plan.Steps[1].Status)
	last := model.calls[2][len(model.calls[2])-1]
	require.Contains(t, last.Content, "unfinished work")
}

func TestTruncatedStepStopsWithoutStreamingItsPartialJSON(t *testing.T) {
	truncated := plannerResponse(`{"status":"completed"`)
	truncated.chunks[0].FinishReason = "length"
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Check A","Check B"]}`), truncated, plannerResponse("Could not complete A."),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	var visible string
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		visible += evt.Data.(event.AgentFinalAnswerData).Content
		return nil
	})
	state, err := engine.Execute(context.Background(), "session", "message", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.Equal(t, "blocked", state.Plan.Steps[0].Status)
	require.Equal(t, "pending", state.Plan.Steps[1].Status)
	require.Equal(t, "Could not complete A.", visible)
}

func TestCompletePlanStepReplansAndBoundsRetries(t *testing.T) {
	plan := &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{Description: "old"}, {Description: "later"}}}
	completePlanStep(plan, `{"status":"replan","result":"scope changed","steps":["new"]}`)
	require.Equal(t, "new", plan.Steps[0].Description)
	require.Equal(t, 1, plan.Replans)
	plan.Replans = 2
	completePlanStep(plan, `{"status":"replan","result":"still blocked","steps":["newer"]}`)
	require.Equal(t, "blocked", plan.Steps[0].Status)
}

func TestMaybePlanRejectsUnstructuredOutput(t *testing.T) {
	engine := newTestEngine(t, &mockChat{responses: []mockResponse{plannerResponse("Here is the answer.")}}, withPlanningEnabled(true))
	plan, ok := engine.maybePlan(context.Background(), "对比 A 和 B", nil, nil)
	require.False(t, ok)
	require.Nil(t, plan)
}
