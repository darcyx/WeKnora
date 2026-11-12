package agent

import (
	"context"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// finishUserQuestion publishes a tool question without another model round when no independent work remains.
func (e *AgentEngine) finishUserQuestion(ctx context.Context, state *types.AgentState, step types.AgentStep, messages *[]chat.Message, sessionID, messageID string) (bool, error) {
	for _, call := range step.ToolCalls {
		if call.Name != agenttools.ToolAskUser || call.Result == nil || !call.Result.Success || call.Result.UserQuestion == nil {
			continue
		}
		hadBlockers := e.retrieval != nil && len((&retrievalControl{state: e.retrieval.snapshot()}).missingUserFields()) > 0
		e.retrievalGateMu.Lock()
		err := e.resolveProjectConditions(ctx)
		e.retrievalGateMu.Unlock()
		if err != nil {
			return false, err
		}
		question := call.Result.Output
		if e.retrieval != nil {
			if fields := (&retrievalControl{state: e.retrieval.snapshot()}).missingUserFields(); len(fields) > 0 {
				question = missingConditionsQuestion(fields)
			} else if hadBlockers {
				*messages = append(*messages, chat.Message{Role: "system", Content: "Project context supplies the missing fields. Continue using the established context without asking for them again."})
				return false, nil
			}
		}
		// Collaboration owns delivery and finish; its ordinary loop must collect child results first.
		if state.Plan != nil || e.collaboration != nil || e.clarificationLeavesIndependentWork(question) {
			*messages = append(*messages, chat.Message{Role: "system", Content: "ask_user returned without waiting. Complete remaining independent work and collect delegated results before including this question in the final answer. Follow the existing plan reporting protocol and preserve completed deliverables for the final answer. Keep dependent work stopped; do not ask the same question again."})
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		nextRound := state.CurrentRound + 1
		if e.withinIterationBudget(nextRound) || e.steerOverruns < maxSteerOverruns {
			injected := e.drainSteerMessages(ctx, state, messages, sessionID, messageID)
			injected += e.injectBackground(ctx, state, messages, true)
			if injected > 0 {
				if !e.withinIterationBudget(nextRound) {
					e.steerOverruns++
					e.allowSteerOverrun = true
				}
				return false, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := e.eventBus.Emit(ctx, event.Event{ID: generateEventID("clarification"), Type: event.EventAgentFinalAnswer, SessionID: sessionID, Data: event.AgentFinalAnswerData{Content: question, Done: true}}); err != nil {
			return false, err
		}
		state.FinalAnswer, state.IsComplete = question, true
		return true, nil
	}
	return false, nil
}
