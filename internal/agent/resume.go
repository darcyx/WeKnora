package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// Resume continues a run that was suspended on a prompt, now answered. It
// rebuilds the transcript from the snapshot, finishes the interrupted round
// with the answer delivered to the blocked call, then carries on with the
// ReAct loop. The run may suspend again; completion is emitted either way.
func (e *AgentEngine) Resume(
	ctx context.Context,
	sessionID, messageID string,
	llmContext []chat.Message,
	snapshot []byte,
	answered *types.AgentPrompt,
) (_ *types.AgentState, err error) {
	// The engine owns the error event for every failure path, like Execute,
	// so callers must not emit another one.
	defer func() {
		if err != nil {
			e.emitExecutionError(ctx, sessionID, err)
		}
	}()
	snap, err := DecodeRunSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	if snap.Plan != nil && (snap.Plan.Current < 0 || snap.Plan.Current > len(snap.Plan.Steps) || len(snap.Plan.Steps) == 0 || len(snap.Plan.Steps) > 6) {
		return nil, fmt.Errorf("snapshot plan is inconsistent")
	}
	e.bindCollaborationInput(sessionID, messageID)
	p := snap.PendingRound
	wait := answered == nil && snap.CollaborationWait && e.collaboration != nil && e.collaboration.Child == nil
	if answered == nil && !wait {
		return nil, fmt.Errorf("resume needs an answered prompt or owned collaboration wait")
	}
	if !snap.BetweenRounds {
		if p.BlockedIndex < 0 || p.BlockedIndex >= len(p.Calls) || len(p.Step.ToolCalls) != p.BlockedIndex {
			return nil, fmt.Errorf("snapshot round is inconsistent: blocked %d of %d calls, %d done",
				p.BlockedIndex, len(p.Calls), len(p.Step.ToolCalls))
		}
		if wait && p.Calls[p.BlockedIndex].Name != "wait_agent" {
			return nil, fmt.Errorf("unsafe collaboration checkpoint")
		}
		if answered != nil && p.Calls[p.BlockedIndex].ID != answered.ToolCallID {
			return nil, fmt.Errorf("prompt %s answers call %s, run is blocked on %s",
				answered.ID, answered.ToolCallID, p.Calls[p.BlockedIndex].ID)
		}
	} else if !wait {
		return nil, fmt.Errorf("between-round resume requires collaboration ownership")
	}
	if e.collaboration != nil {
		var stop func()
		ctx, stop = e.collaboration.Start(ctx)
		defer stop()
	}
	logger.Infof(ctx, "[Agent] Resuming: session=%s, message=%s, prompt=%s, round=%d",
		sessionID, messageID, func() string {
			if answered != nil {
				return answered.ID
			}
			return "collaboration"
		}(), snap.CurrentRound+1)
	defer e.toolRegistry.Cleanup(ctx)

	state := &types.AgentState{
		RoundSteps:    append([]types.AgentStep{}, snap.CompletedSteps...),
		KnowledgeRefs: append([]*types.SearchResult{}, snap.KnowledgeRefs...),
		CurrentRound:  snap.CurrentRound,
		TurnUsage:     snap.TurnUsage,
		Plan:          snap.Plan,
	}
	if snap.Retrieval != nil && snap.RetrievalTurnID != "" && snap.RetrievalTurnID != messageID {
		return nil, fmt.Errorf("retrieval snapshot belongs to a different assistant message")
	}
	e.beginTurn(snap.Query, snap.ImageURLs, state)
	e.setRetrievalHistory(llmContext)
	if e.retrieval != nil {
		e.retrievalTurnID = messageID
	}
	if snap.Retrieval != nil {
		if e.retrieval == nil {
			return nil, fmt.Errorf("retrieval capability changed while the run was suspended")
		}
		restored, err := e.restoreSavedRetrieval(ctx, *snap.Retrieval)
		if err != nil {
			return nil, fmt.Errorf("restore retrieval state: %w", err)
		}
		if restored != nil {
			e.retrieval = restored
		}
	}
	e.recordRetrievalUserMessage()
	if len(e.retrievalPrevious) > 0 && e.retrieval != nil {
		var saved retrievalState
		if err := json.Unmarshal(e.retrievalPrevious, &saved); err != nil {
			return nil, err
		}
		restored, err := e.restoreSavedRetrieval(ctx, saved)
		if err != nil {
			return nil, err
		}
		if restored != nil {
			e.retrieval = restored
			e.recordRetrievalUserMessage()
		}
	}
	if err := e.bindRetrievalPersistence(ctx); err != nil {
		return nil, err
	}
	if err := e.restoreCollaborationDeliveries(ctx, snap); err != nil {
		return nil, err
	}
	for id, content := range snap.UserMessages {
		e.recordSteerMessage(id, content)
	}
	messages := e.buildMessagesWithLLMContext(e.buildSystemPrompt(ctx), snap.Query, sessionID, llmContext, snap.ImageURLs)
	appendUsers := func(step types.AgentStep) error {
		for _, id := range step.UserMessagesBefore {
			content, ok := snap.UserMessages[id]
			if !ok {
				return fmt.Errorf("missing steered message %s in run snapshot", id)
			}
			messages = append(messages, chat.Message{Role: "user", Content: types.SteerMessageContent(content)})
		}
		return nil
	}
	for i, step := range state.RoundSteps {
		e.replayCollaborationDeliveries(&messages, i, true)
		if err := appendUsers(step); err != nil {
			return nil, err
		}
		e.replayCollaborationDeliveries(&messages, i, false)
		messages = e.appendToolResults(messages, step)
		messages = e.appendToolImages(ctx, messages, step)
	}
	e.replayCollaborationDeliveries(&messages, len(state.RoundSteps), true)
	if err := appendUsers(p.Step); err != nil {
		return nil, err
	}
	e.replayCollaborationDeliveries(&messages, len(state.RoundSteps), false)
	if e.toolRegistry != nil {
		e.toolRegistry.RestoreMCPExposure(snap.MCPToolRefs)
		e.toolRegistry.RememberMCPHistory(messages)
		// These calls were issued before suspension, including the first call
		// to a tool whose definition was only exposed by a describe round.
		e.toolRegistry.RememberMCPHistory(pendingCallHistory(p))
		e.toolRegistry.RefreshMCPTools(ctx)
	}
	tools := e.buildToolsForLLM()

	if wait && snap.BetweenRounds {
		if err := e.collaboration.ClearWait(ctx); err != nil {
			return nil, err
		}
	}
	if !snap.BetweenRounds {
		startTime := time.Now()
		step, suspended := e.finishPendingRound(ctx, p, answered, state.CurrentRound, sessionID, messageID)
		if suspended != nil {
			logger.Infof(ctx, "[Agent][Round-%d] Suspended again waiting on prompt %s",
				state.CurrentRound+1, suspended.PromptID)
			state.Suspended = suspended
			state.SuspendedStep = &step
			if e.collaboration != nil {
				e.collaboration.End(true)
			}
			e.emitCompletionEvent(context.WithoutCancel(ctx), state, sessionID, messageID, startTime)
			return state, nil
		}
		state.RoundSteps = append(state.RoundSteps, step)
		messages = e.appendToolResults(messages, step)
		messages = e.appendToolImages(ctx, messages, step)
		if finished, err := e.finishUserQuestion(ctx, state, step, &messages, sessionID, messageID); finished || err != nil {
			e.emitCompletionEvent(context.WithoutCancel(ctx), state, sessionID, messageID, startTime)
			return state, err
		}
		state.CurrentRound++
	}
	if state.Plan != nil {
		messages = append(messages, planInstruction(state.Plan))
		e.emitPlanProgress(ctx, state.Plan, sessionID, state.CurrentRound)
	}

	if _, err := e.executeLoop(ctx, state, snap.Query, messages, tools, sessionID, messageID); err != nil {
		return nil, err
	}
	return state, nil
}

func pendingCallHistory(p PendingRound) []chat.Message {
	calls := make([]chat.ToolCall, len(p.Calls))
	for i, c := range p.Calls {
		calls[i] = chat.ToolCall{ID: c.ID, Type: "function", Function: chat.FunctionCall{
			Name: c.Name, Arguments: c.Arguments,
		}}
	}
	return []chat.Message{{Role: "assistant", ToolCalls: calls}}
}

// finishPendingRound runs the interrupted round from its blocked call on,
// sequentially. The blocked call receives the answer; a later call may raise
// a prompt of its own and suspend the round again.
func (e *AgentEngine) finishPendingRound(
	ctx context.Context, p PendingRound, answered *types.AgentPrompt,
	iteration int, sessionID, messageID string,
) (types.AgentStep, *types.PromptSuspension) {
	step := p.Step
	step.ToolCalls = append([]types.ToolCall(nil), p.Step.ToolCalls...)
	calls := make([]types.LLMToolCall, len(p.Calls))
	for i, c := range p.Calls {
		calls[i] = types.LLMToolCall{
			ID:                c.ID,
			Type:              "function",
			Function:          types.FunctionCall{Name: c.Name, Arguments: c.Arguments},
			UnresolvedHandles: c.UnresolvedHandles,
			ProviderMetadata:  c.ProviderMetadata,
		}
	}

	round := iteration + 1
	e.beginRound(step, calls)
	defer e.endRound()
	for i, done := range step.ToolCalls {
		e.recordCall(i, done)
	}
	for i := p.BlockedIndex; i < len(calls); i++ {
		var answer *types.AgentPrompt
		if i == p.BlockedIndex {
			answer = answered
		}
		toolCall := e.runToolCallAnswered(ctx, calls[i], i, iteration, round, sessionID, messageID, answer)
		if s := suspensionOf(toolCall); s != nil {
			return step, s
		}
		e.recordCall(i, toolCall)
		step.ToolCalls = append(step.ToolCalls, toolCall)
		e.emitToolOutcome(ctx, toolCall, iteration, sessionID)
		if toolCall.Result != nil && toolCall.Result.Success && toolCall.Result.UserQuestion != nil {
			break
		}
	}
	return step, nil
}
