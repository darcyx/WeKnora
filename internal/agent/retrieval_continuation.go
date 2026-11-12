package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/Tencent/WeKnora/internal/logger"
)

var errRetrievalStateSave = errors.New("retrieval state could not be saved; stop dependent retrieval, do not retry this as an empty search")

func (e *AgentEngine) SetRetrievalPersistence(previous json.RawMessage, save func(context.Context, json.RawMessage) error) {
	e.retrievalPrevious = append(json.RawMessage(nil), previous...)
	e.retrievalSave = save
}

func (e *AgentEngine) bindRetrievalPersistence(ctx context.Context) error {
	if e.retrieval == nil || e.retrievalSave == nil {
		return nil
	}
	e.retrieval.persist = func(s retrievalState) error {
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return e.retrievalSave(ctx, raw)
	}
	e.retrieval.mu.Lock()
	defer e.retrieval.mu.Unlock()
	return e.retrieval.persistLocked()
}

func (c *retrievalControl) persistLocked() error {
	if c.persistenceFailed {
		return errRetrievalStateSave
	}
	if c.persist != nil {
		if err := c.persist(c.snapshotLocked()); err != nil {
			c.persistenceFailed = true
			return errRetrievalStateSave
		}
	}
	return nil
}

func (e *AgentEngine) initializeRetrievalContinuation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.retrieval == nil {
		return nil
	}
	if len(e.retrievalPrevious) > 0 {
		var saved retrievalState
		if err := json.Unmarshal(e.retrievalPrevious, &saved); err != nil {
			return fmt.Errorf("invalid stored retrieval state: %w", err)
		}
		previous, err := e.restoreSavedRetrieval(ctx, saved)
		if err != nil {
			return err
		}
		if previous == nil {
			return e.bindRetrievalPersistence(ctx)
		}
		sameMessage := slices.Contains(saved.UserInputIDs, "message:"+e.retrievalUserMessage.id)
		sameGoal := saved.Goal == e.retrieval.snapshot().Goal
		if sameMessage || sameGoal && e.usesRuntimeRetrieval() && e.runtimeRetrievalFor(previous.snapshot()) {
			e.retrieval = previous
			e.recordRetrievalUserMessage()
			if sameGoal && len(saved.Gaps) == 1 && sameRetrievalFact(&saved.Gaps[0], saved.Goal, "") {
				e.retrievalSearchGapID = saved.Gaps[0].ID
				e.retrievalSearchInputID = "message:" + e.retrievalUserMessage.id
			}
			return e.bindRetrievalPersistence(ctx)
		}
		decision, err := e.retrievalAdmission(ctx, "continuation", map[string]any{"previous": retrievalGuardState(previous.snapshot()), "current_input": e.retrievalUserMessage.text, "input_id": "message:" + e.retrievalUserMessage.id})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errRetrievalGuardUnavailable) {
			// Fail open: keep the stored ledger and let this turn's fact admission
			// decide. Ending the turn here would make a guard outage fatal.
			logger.Warnf(ctx, "Retrieval continuation check unavailable; keeping the stored retrieval state: %v", err)
			e.retrieval = previous
			e.recordRetrievalUserMessage()
			e.retrievalSearchGapID, e.retrievalSearchInputID = "", ""
			return e.bindRetrievalPersistence(ctx)
		}
		if (err != nil || decision.Decision != "continue" && decision.Decision != "new_goal") &&
			e.runtimeRetrieval && !retrievalHasDependencies(saved) {
			logger.Warnf(ctx, "Retrieval continuation unavailable; preserving history for fact admission: %v", err)
			e.retrieval = previous
			e.recordRetrievalUserMessage()
			// Keep spent allowances without asserting that this input belongs to an old fact.
			e.retrievalSearchGapID, e.retrievalSearchInputID = "", ""
			return e.bindRetrievalPersistence(ctx)
		}
		if err != nil {
			return err
		}
		switch decision.Decision {
		case "continue":
			e.retrieval = previous
			e.recordRetrievalUserMessage()
			// Answers are model output: apply each one that validates and skip the
			// rest, as project_conditions does, instead of failing the whole turn.
			for _, answer := range decision.UserAnswers {
				err := previous.applyReview(retrievalReview{GapID: answer.GapID, Status: retrievalUserConditions, UserAnswers: []retrievalUserAnswer{answer.retrievalUserAnswer}})
				if errors.Is(err, errRetrievalStateSave) {
					return err
				}
				if err != nil {
					logger.Warnf(ctx, "Ignoring continuation answer for gap %s: %v", answer.GapID, err)
				}
			}
		case "new_goal":
		default:
			return fmt.Errorf("retrieval continuation could not establish the current goal")
		}
	}
	return e.bindRetrievalPersistence(ctx)
}

func retrievalHasDependencies(state retrievalState) bool {
	for _, gap := range state.Gaps {
		if gap.Status == retrievalNeedsUser || gap.Derived || len(gap.DependsOn) > 0 {
			return true
		}
	}
	return false
}
