package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// Ordinary QA uses recorded execution facts; explicit plans and blockers retain their protocol.
func (e *AgentEngine) EnableRuntimeRetrieval() {
	e.EnableRetrievalGuard()
	e.runtimeRetrieval = true
}

func (e *AgentEngine) usesRuntimeRetrieval() bool {
	if e.retrieval == nil {
		return false
	}
	state := e.retrieval.snapshot()
	if !e.runtimeRetrievalFor(state) {
		return false
	}
	for _, gap := range state.Gaps {
		if (e.retrievalSearchGapID == "" || gap.ID == e.retrievalSearchGapID) && (&retrievalControl{state: state}).countSearches(&gap, state.questionID(), retrievalTurnPrefix(e.retrievalTurnID)) >= questionSearchGrace {
			return false
		}
	}
	return true
}

func (e *AgentEngine) usesRuntimeRetrievalTool(name string) bool {
	return e.usesRuntimeRetrieval() || !isBudgetedSearchTool(name) && e.retrieval != nil && e.runtimeRetrievalFor(e.retrieval.snapshot())
}

func (e *AgentEngine) runtimeRetrievalFor(state retrievalState) bool {
	if !e.runtimeRetrieval {
		return false
	}
	for _, gap := range state.Gaps {
		if e.retrievalSearchGapID != "" && gap.ID != e.retrievalSearchGapID {
			continue
		}
		if gap.Derived || len(gap.DependsOn) > 0 || gap.ScopeFrom != "" ||
			gap.Status != retrievalPending && gap.Status != retrievalInFlight && gap.Status != retrievalUnavailable || gap.LastReview != "" || len(gap.MissingUserFields) > 0 {
			return false
		}
	}
	return true
}

// Called under retrievalGateMu, on the same dispatch path as structured retrieval.
func (e *AgentEngine) prepareRuntimeRetrieval(ctx context.Context, tc types.LLMToolCall, args map[string]any) (string, error) {
	c := e.retrieval
	gapID, quote, err := e.bindCurrentRetrievalFact(ctx)
	if err != nil {
		return "", err
	}
	executionArgs := retrievalToolArgs(args)
	if tc.Function.Name == "read_document" {
		id, _ := executionArgs["id"].(string)
		id = strings.TrimSpace(id)
		if id != "" && !e.runtimeDocumentObserved(c.snapshot(), id) {
			return "", fmt.Errorf("read_document requires a document reference from the user, runtime context or an available result")
		}
	}
	encoded, err := json.Marshal(executionArgs)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	g := c.gap(gapID)
	g.SearchGrace, c.state.Version = questionSearchGrace, 4
	previous := c.attempt(g.LastCallID)
	action, recoveryID := retrievalInitial, ""
	if previous != nil {
		action = retrievalRead
		if isBudgetedSearchTool(tc.Function.Name) {
			action = retrievalSearch
		}
		if !previous.FAQOnly && previous.Tool == tc.Function.Name && previous.Outcome == retrievalInput {
			action, recoveryID = retrievalRecovery, previous.ID
		}
	}
	// A useful document read can sit between a partial search failure and its recovery.
	for i := len(c.state.Attempts) - 1; i >= 0; i-- {
		attempt := &c.state.Attempts[i]
		if attempt.GapID != gapID || attempt.Tool != tc.Function.Name {
			continue
		}
		if len(attempt.Failures) > 0 && validateScopedRecovery(attempt, string(encoded)) == nil {
			action, recoveryID = retrievalRecovery, attempt.ID
			break
		}
		// FAQ and document searches share a tool name. Only scoped stage failures above can replay FAQ work.
		if attempt.FAQOnly || attempt.Arguments != string(encoded) {
			continue
		}
		if attempt.Outcome != retrievalSuccess {
			action, recoveryID = retrievalRecovery, attempt.ID
		}
		break
	}
	c.mu.Unlock()
	if err := c.acquireWithAdmission(gapID, e.retrievalCallReference(tc.ID), action, quote,
		tc.Function.Name, string(encoded), retrievalExecutionAdmission{RuntimeManaged: true}, recoveryID); err != nil {
		return "", fmt.Errorf("retrieval execution limit: %w; use available evidence or explain the remaining limitation", err)
	}
	return string(encoded), nil
}

func (e *AgentEngine) runtimeDocumentObserved(state retrievalState, id string) bool {
	e.retrieval.mu.Lock()
	fromUser := e.retrieval.hasUserQuote(id)
	e.retrieval.mu.Unlock()
	if fromUser {
		return true
	}
	for _, doc := range e.selectedDocs {
		if doc != nil && doc.KnowledgeID == id {
			return true
		}
	}
	for _, kb := range e.knowledgeBasesInfo {
		if kb == nil {
			continue
		}
		// Match the recent-document references published by formatKnowledgeBaseList.
		for _, doc := range kb.RecentDocs[:min(2, len(kb.RecentDocs))] {
			if doc.KnowledgeID == id || doc.ChunkID == id {
				return true
			}
		}
	}
	for _, observation := range state.Attempts {
		if observation.Completed && observation.Outcome == retrievalSuccess &&
			retrievalScopeCovers(state.Scope, observation.Scope) {
			var args struct {
				ID string `json:"id"`
			}
			if observation.Tool == "read_document" && json.Unmarshal([]byte(observation.Arguments), &args) == nil && args.ID == id {
				return true // Current tool authorization still applies when reloading.
			}
			if !observedDocumentReference(observation.Content, id) {
				continue
			}
			return true
		}
	}
	return false
}
