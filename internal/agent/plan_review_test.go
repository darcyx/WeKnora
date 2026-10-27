package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func planToolCall(name string) mockResponse {
	return mockResponse{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls",
		ToolCalls: []types.LLMToolCall{{ID: "call-1", Type: "function", Function: types.FunctionCall{Name: name, Arguments: `{}`}}},
	}}}
}

func TestPlanRunsWithIdleCollaborationBinding(t *testing.T) {
	_, binding := engineCollaboration(t)
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Check A","Check B"]}`),
		plannerResponse(`{"status":"completed","result":"A"}`),
		plannerResponse(`{"status":"completed","result":"B"}`),
		plannerResponse("Final"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.SetCollaboration(binding)
	state, err := engine.Execute(context.Background(), "s", "m", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.NotNil(t, state.Plan)
	require.Equal(t, "Final", state.FinalAnswer)
	require.Len(t, model.calls, 4)
}

func TestPlanWaitsForChildBeforeCompletingStep(t *testing.T) {
	runtime, binding := engineCollaboration(t)
	_, err := runtime.Spawn(context.Background(), binding.Scope, "spawn", "research", "a", "{}", "q")
	require.NoError(t, err)
	first := plannerResponse(`{"status":"completed","result":"premature"}`)
	first.chunks = append([]types.StreamResponse{{ResponseType: types.ResponseTypeThinking, Content: "checking child"}}, first.chunks...)
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Check A","Check B"]}`), first,
		plannerResponse(`{"status":"completed","result":"child reviewed"}`),
		plannerResponse(`{"status":"completed","result":"B"}`), plannerResponse("Final"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.SetCollaboration(binding)
	engine.eventBus.On(event.EventAgentThought, func(ctx context.Context, evt event.Event) error {
		if data, ok := evt.Data.(event.AgentThoughtData); ok && data.Content == "checking child" {
			return runtime.Interrupt(ctx, binding.Scope, "research", "")
		}
		return nil
	})
	state, err := engine.Execute(context.Background(), "s", "m", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.Equal(t, "child reviewed", state.Plan.Steps[0].Result)
	require.Equal(t, "Final", state.FinalAnswer)
	require.Len(t, model.calls, 5)
	var evidence string
	for _, msg := range model.calls[2] {
		evidence += msg.Content
	}
	require.Contains(t, evidence, "research")
}

func TestPlanCollaborationWaitCheckpointClearedOnChildResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime, binding := engineCollaboration(t)
	owned, err := runtime.AcquireParent(ctx, binding.Scope)
	require.NoError(t, err)
	_, err = runtime.Spawn(ctx, binding.Scope, "spawn", "research", "a", "{}", "q")
	require.NoError(t, err)
	engine := newTestEngine(t, nil)
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.SetCollaboration(owned)
	engine.EnableSuspension(json.RawMessage(`{}`))
	state := &types.AgentState{Plan: &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{Description: "Research", Status: "in_progress"}}}}
	engine.beginTurn("q", nil, state)
	var messages []chat.Message
	type outcome struct {
		waiting bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		waiting, waitErr := engine.waitPlanCollaboration(ctx, state, &messages)
		done <- outcome{waiting, waitErr}
	}()
	require.Eventually(t, func() bool {
		group, readErr := runtime.Store.Read(ctx, binding.Scope)
		return readErr == nil && group.ParentWait != nil
	}, 3*time.Second, time.Millisecond)
	group, err := runtime.Store.Read(ctx, binding.Scope)
	require.NoError(t, err)
	snapshot, err := DecodeRunSnapshot(group.ParentWait.Snapshot)
	require.NoError(t, err)
	require.Equal(t, "in_progress", snapshot.Plan.Steps[0].Status)
	require.NoError(t, runtime.Interrupt(ctx, binding.Scope, "research", ""))
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.True(t, result.waiting)
	case <-time.After(3 * time.Second):
		t.Fatal("plan did not resume after the child result")
	}
	group, err = runtime.Store.Read(ctx, binding.Scope)
	require.NoError(t, err)
	require.Nil(t, group.ParentWait)
}

func TestPlanSteerDoesNotRepeatCompletedWrite(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Write A","Check B"]}`),
		planToolCall("write_test"),
		plannerResponse(`{"status":"completed","result":"A saved"}`),
		plannerResponse(`{"status":"completed","result":"B checked"}`),
		plannerResponse("Final"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	write := newCountingTool("write_test")
	engine.toolRegistry.RegisterTool(write)
	engine.SetSteerSink(&sequencedSteerSink{byPoll: map[int][]map[string]interface{}{
		3: {steerEntry("correction", "Also mention the owner")},
	}})
	state, err := engine.Execute(context.Background(), "s", "m", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.Equal(t, 1, write.calls)
	require.Equal(t, "completed", state.Plan.Steps[0].Status)
	require.Contains(t, model.calls[3][len(model.calls[3])-1].Content, `"current":1`)
}

func TestPlanRepairsMalformedReportWithoutRepeatingTools(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Write A","Check B"]}`),
		planToolCall("write_test"),
		plannerResponse("A was saved."),
		plannerResponse(`{"status":"completed","result":"A saved"}`),
		plannerResponse(`{"status":"completed","result":"B checked"}`),
		plannerResponse("Final"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	write := newCountingTool("write_test")
	engine.toolRegistry.RegisterTool(write)
	state, err := engine.Execute(context.Background(), "s", "m", "Write A and check B", nil)
	require.NoError(t, err)
	require.Equal(t, 1, write.calls)
	require.Empty(t, model.opts[3].Tools)
	require.Equal(t, "completed", state.Plan.Steps[0].Status)
	require.Equal(t, "Final", state.FinalAnswer)
}

func TestPlanStopsAfterSecondMalformedReport(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Check A","Check B"]}`),
		plannerResponse("A"), plannerResponse("Still not JSON"), plannerResponse("Could not complete A."),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	state, err := engine.Execute(context.Background(), "s", "m", "Check A and B", nil)
	require.NoError(t, err)
	require.Equal(t, "blocked", state.Plan.Steps[0].Status)
	require.Equal(t, "pending", state.Plan.Steps[1].Status)
	require.Len(t, model.calls, 4)
}

func TestPlanReportRepairRejectsUnadvertisedToolCalls(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Write A","Check B"]}`), planToolCall("write_test"),
		plannerResponse("A was saved."), planToolCall("write_test"), plannerResponse("Could not verify A."),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	write := newCountingTool("write_test")
	engine.toolRegistry.RegisterTool(write)
	state, err := engine.Execute(context.Background(), "s", "m", "Write A and check B", nil)
	require.NoError(t, err)
	require.Equal(t, 1, write.calls)
	require.Equal(t, "blocked", state.Plan.Steps[0].Status)
	require.Contains(t, state.RoundSteps[2].ToolCalls[0].Result.Error, "not allowed")
}

func TestPlanSteerAtBudgetGetsOneMoreRound(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Check A","Check B"]}`),
		plannerResponse(`{"status":"completed","result":"A"}`),
		plannerResponse(`{"status":"completed","result":"B"}`),
		plannerResponse("Final"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true), withMaxIterations(1))
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.SetSteerSink(&sequencedSteerSink{byPoll: map[int][]map[string]interface{}{
		2: {steerEntry("correction", "Include B")},
	}})
	state, err := engine.Execute(context.Background(), "s", "m", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.Equal(t, "unverified", state.Plan.Steps[1].Status)
	require.Len(t, model.calls, 4)
}

func TestPageButtonDoesNotEndAnActivePlan(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Open page","Check details"]}`),
		pageToolResponse("Opening the page", agenttools.ToolOpenPage),
		plannerResponse(`{"status":"completed","result":"Page offered"}`),
		plannerResponse(`{"status":"completed","result":"Details checked"}`),
		plannerResponse("Final"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.toolRegistry.RegisterTool(newCountingTool(agenttools.ToolOpenPage))
	var visible strings.Builder
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		visible.WriteString(evt.Data.(event.AgentFinalAnswerData).Content)
		return nil
	})
	state, err := engine.Execute(context.Background(), "s", "m", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.Equal(t, "unverified", state.Plan.Steps[1].Status)
	require.Equal(t, "Final", visible.String())
}

func TestFailedToolCannotBeReportedAsCompleted(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["Save A","Check B"]}`),
		planToolCall("write_test"),
		plannerResponse(`{"status":"completed","result":"A saved"}`),
		plannerResponse("Could not save A."),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.toolRegistry.RegisterTool(&failingTool{BaseTool: agenttools.NewBaseTool("write_test", "", json.RawMessage(`{"type":"object"}`))})
	state, err := engine.Execute(context.Background(), "s", "m", "对比 A 和 B", nil)
	require.NoError(t, err)
	require.Equal(t, "blocked", state.Plan.Steps[0].Status)
	require.Equal(t, "pending", state.Plan.Steps[1].Status)
	require.Contains(t, model.calls[3][len(model.calls[3])-1].Content, "tool failed")
}

func TestPlanToolFailuresResolvedOnlyBySameCall(t *testing.T) {
	state := &types.AgentState{Plan: &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{}}}, RoundSteps: []types.AgentStep{{ToolCalls: []types.ToolCall{
		{Name: "write", Args: map[string]interface{}{"path": "A"}, Result: &types.ToolResult{Success: false}},
		{Name: "write", Args: map[string]interface{}{"path": "B"}, Result: &types.ToolResult{Success: true}},
	}}}}
	response := `{"status":"completed","result":"saved"}`
	require.Contains(t, checkPlanToolResults(state, response), `"blocked"`)
	state.RoundSteps = append(state.RoundSteps, types.AgentStep{ToolCalls: []types.ToolCall{{
		Name: "write", Args: map[string]interface{}{"path": "A"}, Result: &types.ToolResult{Success: true},
	}}})
	require.Equal(t, response, checkPlanToolResults(state, response))
}

func TestPlanParallelSuccessDoesNotResolveFailure(t *testing.T) {
	state := &types.AgentState{Plan: &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{}}}, RoundSteps: []types.AgentStep{{ToolCalls: []types.ToolCall{
		{Name: "write", Result: &types.ToolResult{Success: false}},
		{Name: "write", Result: &types.ToolResult{Success: true}},
	}}}}
	require.Contains(t, checkPlanToolResults(state, `{"status":"completed","result":"saved"}`), `"blocked"`)
}

func TestPlanContinuesWithUnverifiedReadFallback(t *testing.T) {
	state := &types.AgentState{Plan: &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{}}}, RoundSteps: []types.AgentStep{{ToolCalls: []types.ToolCall{
		{Name: agenttools.ToolWebSearch, Args: map[string]interface{}{"query": "A"}, Result: &types.ToolResult{Success: false}},
		{Name: agenttools.ToolSearchKnowledge, Args: map[string]interface{}{"query": "A"}, Result: &types.ToolResult{Success: true}},
	}}}}
	result := checkPlanToolResults(state, `{"status":"completed","result":"found A"}`)
	completePlanStep(state.Plan, result)
	require.Equal(t, "unverified", state.Plan.Steps[0].Status)
	require.Equal(t, 1, state.Plan.Current)
}

func TestPlanReadFailureCanUseEarlierStepEvidence(t *testing.T) {
	state := &types.AgentState{Plan: &types.ExecutionPlan{Current: 1, Steps: []types.ExecutionPlanStep{{}, {StartStep: 1}}}, RoundSteps: []types.AgentStep{
		{ToolCalls: []types.ToolCall{{Name: agenttools.ToolSearchKnowledge, Result: &types.ToolResult{Success: true}}}},
		{ToolCalls: []types.ToolCall{{Name: agenttools.ToolReadDocument, Result: &types.ToolResult{Success: false}}}},
	}}
	result := checkPlanToolResults(state, `{"status":"completed","result":"using earlier search"}`)
	completePlanStep(state.Plan, result)
	require.Equal(t, "unverified", state.Plan.Steps[1].Status)
}

func TestPlanContinuesAfterSearchFallback(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		plannerResponse(`{"steps":["查退款规则","撰写申诉邮件"]}`),
		planToolCall(agenttools.ToolWebSearch),
		planToolCall(agenttools.ToolSearchKnowledge),
		plannerResponse(`{"status":"completed","result":"找到了退款规则"}`),
		plannerResponse(`{"status":"completed","result":"已整理邮件草稿"}`),
		plannerResponse("退款规则及申诉邮件草稿"),
	}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.toolRegistry.RegisterTool(&failingTool{BaseTool: agenttools.NewBaseTool(agenttools.ToolWebSearch, "", json.RawMessage(`{"type":"object"}`))})
	search := newCountingTool(agenttools.ToolSearchKnowledge)
	engine.toolRegistry.RegisterTool(search)
	state, err := engine.Execute(context.Background(), "s", "m", "先查退款规则，再写申诉邮件", nil)
	require.NoError(t, err)
	require.Equal(t, "unverified", state.Plan.Steps[0].Status)
	require.Equal(t, "unverified", state.Plan.Steps[1].Status)
	require.Equal(t, "退款规则及申诉邮件草稿", state.FinalAnswer)
	require.Equal(t, 1, search.calls)
	last := model.calls[len(model.calls)-1]
	require.Contains(t, last[len(last)-1].Content, "failed searches")
}

func TestPlanCompletionWithoutToolEvidenceIsUnverified(t *testing.T) {
	state := &types.AgentState{Plan: &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{}}}}
	result := checkPlanToolResults(state, `{"status":"completed","result":"saved"}`)
	completePlanStep(state.Plan, result)
	require.Equal(t, "unverified", state.Plan.Steps[0].Status)
	require.Equal(t, "saved", state.Plan.Steps[0].Result)
}

func TestPlanDoesNotTreatFailedShellCommandAsSuccess(t *testing.T) {
	state := &types.AgentState{Plan: &types.ExecutionPlan{Steps: []types.ExecutionPlanStep{{}}}, RoundSteps: []types.AgentStep{{ToolCalls: []types.ToolCall{{
		Name: "shell_exec", Result: &types.ToolResult{Success: true, Data: map[string]interface{}{"exit_code": 1}},
	}}}}}
	result := checkPlanToolResults(state, `{"status":"completed","result":"command succeeded"}`)
	completePlanStep(state.Plan, result)
	require.Equal(t, "blocked", state.Plan.Steps[0].Status)
}

func TestPlanAcceptsFencedJSON(t *testing.T) {
	model := &mockChat{responses: []mockResponse{plannerResponse("```json\n{\"steps\":[\"A\",\"B\"]}\n```")}}
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	plan, ok := engine.maybePlan(context.Background(), "对比 A 和 B", nil, nil)
	require.True(t, ok)
	completePlanStep(plan, "```json\n{\"status\":\"completed\",\"result\":\"A\"}\n```")
	require.Equal(t, "completed", plan.Steps[0].Status)
}
