package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Tencent/WeKnora/internal/logger"
)

func (e *AgentEngine) projectInputID() string {
	if e.appInfo == "" {
		return ""
	}
	return fmt.Sprintf("app_info:%x", sha256.Sum256([]byte(e.appInfo)))
}

// Preserve project provenance instead of presenting metadata as a user reply.
func (e *AgentEngine) resolveProjectConditions(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !e.retrievalGuard || e.retrieval == nil || e.appInfo == "" {
		return nil
	}
	e.retrieval.mu.Lock()
	failed := e.retrieval.persistenceFailed
	e.retrieval.mu.Unlock()
	if failed {
		return errRetrievalStateSave
	}
	state := e.retrieval.snapshot()
	var missing []retrievalGap
	allowed := make(map[string]bool)
	for _, g := range retrievalGuardState(state).Gaps {
		if g.Status == retrievalNeedsUser {
			missing = append(missing, g)
			allowed[g.ID] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	raw, _ := json.Marshal(missing)
	key := e.projectInputID() + string(raw)
	if e.retrievalProjectReviewed == key {
		return nil
	}
	e.retrievalProjectReviewed = key
	e.retrieval.recordUserInput(e.projectInputID(), e.appInfo)
	e.retrieval.mu.Lock()
	failed = e.retrieval.persistenceFailed
	e.retrieval.mu.Unlock()
	if failed {
		return errRetrievalStateSave
	}
	verdict, err := e.retrievalAdmission(ctx, "project_conditions", map[string]any{"missing_facts": missing})
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, errRetrievalGuardUnavailable) {
		// Project metadata is an enrichment: an unavailable check leaves the
		// blocker in place and lets the turn continue.
		logger.Warnf(ctx, "Retrieval boundary check unavailable for project conditions; leaving the blocker unresolved: %v", err)
		return nil
	}
	if err != nil {
		return err
	}
	if verdict.Decision != "continue" || len(verdict.UserAnswers) == 0 {
		return nil
	}
	c := e.retrieval
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.persistenceFailed {
		return errRetrievalStateSave
	}
	// Answers are model output: apply each one that validates and skip the rest,
	// as continuation does. A single-answer resolve fails before it mutates.
	next := &retrievalControl{state: c.snapshotLocked(), userInputs: c.userInputs}
	applied := false
	for _, answer := range verdict.UserAnswers {
		if !allowed[answer.GapID] || answer.InputID != e.projectInputID() {
			logger.Warnf(ctx, "Ignoring a project condition with an invalid gap or input identity")
			continue
		}
		if err := next.resolveUserConditions(retrievalReview{GapID: answer.GapID, Status: retrievalUserConditions, UserAnswers: []retrievalUserAnswer{answer.retrievalUserAnswer}}); err != nil {
			logger.Warnf(ctx, "Ignoring invalid project condition for gap %s: %v", answer.GapID, err)
			continue
		}
		applied = true
	}
	if !applied {
		return nil
	}
	c.state = next.state
	if err := c.persistLocked(); err != nil {
		return err
	}
	// Cache the resulting blockers so later publication does not recheck the same metadata.
	missing = missing[:0]
	for _, g := range retrievalGuardState(c.snapshotLocked()).Gaps {
		if g.Status == retrievalNeedsUser {
			missing = append(missing, g)
		}
	}
	raw, _ = json.Marshal(missing)
	e.retrievalProjectReviewed = e.projectInputID() + string(raw)
	return nil
}
