package agent

import (
	"context"
	"errors"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestStreamRetryReplacesFailedPartialAnswer(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "partial draft"},
			{ResponseType: types.ResponseTypeError, Content: "upstream 503", Done: true},
		}},
		{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "final answer"},
			{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "stop"},
		}},
	}}
	engine := newTestEngine(t, model)
	recorder := &truncationRecorder{}
	recorder.attach(engine.eventBus)

	state := &types.AgentState{}
	messages := emptyMessages()
	_, err := engine.callLLMWithRetry(
		context.Background(), &messages, emptyTools(), state, "question", 0, "session",
	)
	require.NoError(t, err)

	var rendered string
	replaces := 0
	for _, data := range recorder.snapshot() {
		if data.Replace {
			rendered = ""
			replaces++
		}
		rendered += data.Content
	}
	require.Equal(t, "final answer", rendered)
	require.Equal(t, 1, replaces)
}

func TestStreamFailureWithoutAnswerDoesNotEmitReplace(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeError, Content: "upstream 503", Done: true},
	}}}}
	engine := newTestEngine(t, model)
	recorder := &truncationRecorder{}
	recorder.attach(engine.eventBus)

	messages := emptyMessages()
	_, err := engine.callLLMWithRetry(
		context.Background(), &messages, emptyTools(), &types.AgentState{}, "question", 0, "session",
	)
	require.Error(t, err)
	for _, data := range recorder.snapshot() {
		require.False(t, data.Replace, "a failed attempt with no visible answer has nothing to replace")
	}
}

func TestStreamFailureClosesOpenThoughtAndPendingTool(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeThinking, Content: "reasoning"},
		{ResponseType: types.ResponseTypeToolCall, Data: map[string]any{"tool_call_id": "partial", "tool_name": "search"}},
		{ResponseType: types.ResponseTypeError, Content: "upstream 503", Done: true},
	}}}}
	engine := newTestEngine(t, model)
	var failedCalls []string
	engine.eventBus.On(event.EventAgentToolResult, func(_ context.Context, evt event.Event) error {
		result := evt.Data.(event.AgentToolResultData)
		require.False(t, result.Success)
		failedCalls = append(failedCalls, result.ToolCallID)
		return nil
	})
	done := map[string]bool{}
	engine.eventBus.On(event.EventAgentThought, func(_ context.Context, evt event.Event) error {
		done[evt.ID] = evt.Data.(event.AgentThoughtData).Done
		return nil
	})

	_, err := engine.streamThinkingToEventBus(context.Background(), emptyMessages(), emptyTools(), 0, "session")
	require.Error(t, err)
	require.Equal(t, []string{"partial"}, failedCalls)
	require.Len(t, done, 1)
	for _, closed := range done {
		require.True(t, closed, "a failed attempt must not leave its thinking card open")
	}
}

func TestStreamCancellationKeepsPartialAnswer(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "partial draft"},
		{ResponseType: types.ResponseTypeError, Content: "context canceled", Done: true},
	}}}}
	engine := newTestEngine(t, model)
	recorder := &truncationRecorder{}
	recorder.attach(engine.eventBus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		if evt.Data.(event.AgentFinalAnswerData).Content != "" {
			cancel()
		}
		return nil
	})
	state, err := engine.executeLoop(
		ctx, &types.AgentState{}, "question", emptyMessages(), emptyTools(), "session", "message",
	)
	require.NoError(t, err)
	require.Equal(t, "partial draft", state.FinalAnswer)
	require.False(t, state.IsComplete, "stopping a partial answer does not complete the run")
	require.Len(t, model.calls, 1, "cancellation must not retry or synthesize")
	for _, data := range recorder.snapshot() {
		require.False(t, data.Replace, "cancellation has no retry; keep the partial answer")
	}
}

func TestLeaseLossDuringAnswerPreservesPartialAndReportsFailure(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "partial"},
		{ResponseType: types.ResponseTypeError, Content: "context canceled", Done: true},
	}}}}
	engine := newTestEngine(t, model)
	engine.toolRegistry = agenttools.NewToolRegistry()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	lost := errors.New("lease lost")
	var failure, partial string
	engine.eventBus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error { cancel(lost); return nil })
	engine.eventBus.On(event.EventError, func(_ context.Context, evt event.Event) error {
		failure = evt.Data.(event.ErrorData).Error
		return nil
	})
	engine.eventBus.On(event.EventAgentComplete, func(_ context.Context, evt event.Event) error {
		partial = evt.Data.(event.AgentCompleteData).FinalAnswer
		return nil
	})
	_, err := engine.Execute(ctx, "session", "message", "question", nil)
	require.ErrorIs(t, err, lost)
	require.Equal(t, lost.Error(), failure)
	require.Equal(t, "partial", partial)
}

func TestStreamCancellationKeepsAnswerWithPendingPageButton(t *testing.T) {
	for _, tool := range []string{agenttools.ToolOpenPage, agenttools.ToolViewPage} {
		t.Run(tool, func(t *testing.T) {
			model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
				{ResponseType: types.ResponseTypeAnswer, Content: "the report"},
				{ResponseType: types.ResponseTypeToolCall, Data: map[string]any{"tool_call_id": "page", "tool_name": tool}},
				{ResponseType: types.ResponseTypeError, Content: "context canceled", Done: true},
			}}}}
			engine := newTestEngine(t, model)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			engine.eventBus.On(event.EventAgentToolCall, func(context.Context, event.Event) error {
				cancel()
				return nil
			})
			state, err := engine.executeLoop(ctx, &types.AgentState{}, "question", emptyMessages(), emptyTools(), "session", "message")
			require.NoError(t, err)
			require.False(t, state.IsComplete)
			if tool == agenttools.ToolOpenPage {
				require.Equal(t, "the report", state.FinalAnswer, "a page button does not make the written answer a tool preamble")
			} else {
				require.Empty(t, state.FinalAnswer)
				require.Len(t, state.RoundSteps, 1)
				require.Equal(t, "the report", state.RoundSteps[0].Thought, "a real tool's preamble belongs in the steps")
			}
		})
	}
}

func TestStreamCancellationKeepsAnswerWithAssembledPageButton(t *testing.T) {
	for _, tool := range []string{agenttools.ToolOpenPage, agenttools.ToolViewPage} {
		t.Run(tool, func(t *testing.T) {
			model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
				{ResponseType: types.ResponseTypeAnswer, Content: "the report"},
				{ResponseType: types.ResponseTypeError, Content: "context canceled", Done: true, ToolCalls: []types.LLMToolCall{{
					ID: "page", Type: "function", Function: types.FunctionCall{Name: tool, Arguments: `{"page":"agents"}`},
				}}},
			}}}}
			engine := newTestEngine(t, model)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			engine.eventBus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error {
				cancel()
				return nil
			})
			state, err := engine.executeLoop(ctx, &types.AgentState{}, "question", emptyMessages(), emptyTools(), "session", "message")
			require.NoError(t, err)
			require.False(t, state.IsComplete)
			if tool == agenttools.ToolOpenPage {
				require.Equal(t, "the report", state.FinalAnswer)
				require.Empty(t, state.RoundSteps, "the answer must not be duplicated as a thought")
			} else {
				require.Empty(t, state.FinalAnswer)
				require.Len(t, state.RoundSteps, 1)
				require.Equal(t, "the report", state.RoundSteps[0].Thought)
			}
		})
	}
}

func TestStreamSuccessDoesNotEmitReplace(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "final answer"},
		{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "stop"},
	}}}}
	engine := newTestEngine(t, model)
	recorder := &truncationRecorder{}
	recorder.attach(engine.eventBus)

	messages := emptyMessages()
	response, err := engine.callLLMWithRetry(
		context.Background(), &messages, emptyTools(), &types.AgentState{}, "question", 0, "session",
	)
	require.NoError(t, err)
	require.Equal(t, "final answer", response.Content)
	for _, data := range recorder.snapshot() {
		require.False(t, data.Replace)
	}
}

// answerEvents replays final-answer events per answer ID the way the client does.
func answerEvents(e *AgentEngine) (rendered map[string]string, replaces map[string]int) {
	rendered, replaces = map[string]string{}, map[string]int{}
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		data := evt.Data.(event.AgentFinalAnswerData)
		if data.Replace {
			rendered[evt.ID] = ""
			replaces[evt.ID]++
		}
		rendered[evt.ID] += data.Content
		return nil
	})
	return rendered, replaces
}

func TestPublicAnswerFailureRetractsStreamedDraft(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	e.appInfo = `{"channel":"微信小游戏"}`
	_, err := e.retrieval.register(retrievalFactProposal{GoalQuote: "TapTap sandbox configuration", Fact: "sandbox configuration"})
	require.NoError(t, err)
	e.chatModel = &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "streamed draft"},
		{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "stop"},
	}}}}
	rendered, replaces := answerEvents(e)
	// The state store fails while the draft is already on screen.
	e.eventBus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error {
		e.retrieval.mu.Lock()
		e.retrieval.persistenceFailed = true
		e.retrieval.mu.Unlock()
		return nil
	})

	_, err = e.streamThinkingToEventBus(context.Background(), emptyMessages(), emptyTools(), 0, "session")
	require.ErrorIs(t, err, errRetrievalStateSave)
	require.Len(t, rendered, 1)
	for id, text := range rendered {
		require.Empty(t, text, "a draft the gate refused must not stay visible")
		require.Equal(t, 1, replaces[id])
	}
}

func TestIndependentWorkRetryRetractsStreamedDraft(t *testing.T) {
	e, _ := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	blocked, err := e.retrieval.register(retrievalFactProposal{GoalQuote: "TapTap sandbox configuration", Fact: "sandbox configuration"})
	require.NoError(t, err)
	_, err = e.retrieval.register(retrievalFactProposal{GoalQuote: "TapTap sandbox configuration", Fact: "whitelist purpose", DistinctFrom: []retrievalDistinction{{GapID: blocked, Reason: "independent supported explanation"}}})
	require.NoError(t, err)
	stop := types.StreamResponse{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "stop"}
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "streamed draft"}, stop}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "second round"}, stop}},
	}}
	e.chatModel = model
	rendered, replaces := answerEvents(e)
	var firstID string
	// A blocker lands mid-stream, after the round was allowed to stream live.
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		if firstID == "" {
			firstID = evt.ID
			return e.retrieval.applyReview(retrievalReview{GapID: blocked, Status: retrievalNeedsUser, MissingUserFields: []string{"渠道"}})
		}
		return nil
	})

	tools := []chat.Tool{{Type: "function", Function: chat.FunctionDef{Name: "search_knowledge"}}}
	_, _ = e.executeLoop(context.Background(), &types.AgentState{}, "question", emptyMessages(), tools, "session", "message")
	require.GreaterOrEqual(t, len(model.calls), 2, "the independent-work retry must run")
	require.Empty(t, rendered[firstID], "the retried round's draft must not stay visible")
	require.Equal(t, 1, replaces[firstID])
}

func TestContextWindowRetryRetractsAnswerAndCountsBothCalls(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "discarded", Done: true, FinishReason: "length",
			Usage: &types.TokenUsage{PromptTokens: 100, CompletionTokens: 1, TotalTokens: 101}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "recovered", Done: true, FinishReason: "stop",
			Usage: &types.TokenUsage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42}}}},
	}}
	e := newTestEngine(t, model, withMaxCompletionTokens(1024))
	var visible string
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		d := evt.Data.(event.AgentFinalAnswerData)
		if d.Replace {
			visible = ""
		}
		visible += d.Content
		return nil
	})
	state, err := e.executeLoop(t.Context(), &types.AgentState{}, "question", emptyMessages(), nil, "s", "m")
	require.NoError(t, err)
	require.Equal(t, "recovered", state.FinalAnswer)
	require.Equal(t, state.FinalAnswer, visible)
	require.Equal(t, 143, state.TurnUsage.TotalTokens)
}
