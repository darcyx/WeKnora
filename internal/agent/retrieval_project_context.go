package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

func (e *AgentEngine) projectInputID() string {
	if e.appInfo == "" {
		return ""
	}
	return fmt.Sprintf("app_info:%x", sha256.Sum256([]byte(e.appInfo)))
}

// Preserve project provenance instead of presenting metadata as a user reply.
func (e *AgentEngine) resolveProjectConditions(ctx context.Context) error {
	if !e.retrievalGuard || e.retrieval == nil || e.appInfo == "" {
		return nil
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
	failed := e.retrieval.persistenceFailed
	e.retrieval.mu.Unlock()
	if failed {
		return errRetrievalStateSave
	}
	verdict, err := e.retrievalAdmission(ctx, "project_conditions", map[string]any{"missing_facts": missing})
	if err != nil {
		return err
	}
	if verdict.Decision != "continue" || len(verdict.UserAnswers) == 0 {
		return nil
	}
	c := e.retrieval
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.persistenceFailed {
		return errRetrievalStateSave
	}
	next := &retrievalControl{state: c.snapshotLocked(), userInputs: c.userInputs}
	for _, answer := range verdict.UserAnswers {
		if !allowed[answer.GapID] || answer.InputID != e.projectInputID() {
			return fmt.Errorf("project conditions require the current project input identity")
		}
		if err := next.resolveUserConditions(retrievalReview{GapID: answer.GapID, Status: retrievalUserConditions, UserAnswers: []retrievalUserAnswer{answer.retrievalUserAnswer}}); err != nil {
			return err
		}
	}
	c.state = next.state
	return c.persistLocked()
}
