package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
		previous, err := restoreRetrievalForScope(saved, e.retrieval.snapshot().Scope)
		if err != nil {
			return err
		}
		if e.usesRuntimeRetrieval() && e.runtimeRetrievalFor(previous.snapshot()) && saved.Goal == e.retrieval.snapshot().Goal {
			e.retrieval = previous
			e.recordRetrievalUserMessage()
			if len(saved.Gaps) == 1 {
				e.retrievalSearchGapID = saved.Gaps[0].ID
				e.retrievalSearchInputID = "message:" + e.retrievalUserMessage.id
			}
			return e.bindRetrievalPersistence(ctx)
		}
		decision, err := e.retrievalAdmission(ctx, "continuation", map[string]any{"previous": retrievalGuardState(previous.snapshot()), "current_input": e.retrievalUserMessage.text, "input_id": "message:" + e.retrievalUserMessage.id})
		if ctx.Err() != nil {
			return ctx.Err()
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
			if e.runtimeRetrieval && len(decision.UserAnswers) == 0 && len(saved.Gaps) == 1 && !saved.Gaps[0].Derived {
				e.retrievalSearchGapID = saved.Gaps[0].ID
				e.retrievalSearchInputID = "message:" + e.retrievalUserMessage.id
			}
			for _, g := range previous.snapshot().Gaps {
				var answers []retrievalUserAnswer
				for _, answer := range decision.UserAnswers {
					if answer.GapID == g.ID {
						answers = append(answers, answer.retrievalUserAnswer)
					}
				}
				if len(answers) > 0 {
					if err := previous.applyReview(retrievalReview{GapID: g.ID, Status: retrievalUserConditions, UserAnswers: answers}); err != nil {
						return err
					}
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
