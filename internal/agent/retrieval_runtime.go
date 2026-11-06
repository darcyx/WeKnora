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
		if (e.retrievalSearchGapID == "" || gap.ID == e.retrievalSearchGapID) && (&retrievalControl{state: state}).searchCount(&gap) >= questionSearchGrace {
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
	e.recordRetrievalUserMessage()
	state := c.snapshot()
	quote, inputID := strings.TrimSpace(state.Goal), ""
	c.mu.Lock()
	for i := len(c.state.UserInputIDs) - 1; i >= 0; i-- {
		id := c.state.UserInputIDs[i]
		if !strings.HasPrefix(id, "app_info:") && strings.TrimSpace(c.userInputs[id]) != "" {
			inputID, quote = id, strings.TrimSpace(c.userInputs[id])
			break
		}
	}
	c.mu.Unlock()
	if e.retrievalSearchGapID == "" || e.retrievalSearchInputID != inputID {
		id, err := e.admitRetrievalFact(ctx, retrievalFactProposal{GoalQuote: quote, GoalInputID: inputID, Fact: quote})
		if err != nil {
			return "", err
		}
		e.retrievalSearchGapID, e.retrievalSearchInputID = id, inputID
	}
	gapID := e.retrievalSearchGapID
	executionArgs := retrievalToolArgs(args)
	if tc.Function.Name == "read_document" {
		id, _ := executionArgs["id"].(string)
		id = strings.TrimSpace(id)
		if id != "" && !e.runtimeDocumentObserved(state, id) {
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
		if previous.Tool == tc.Function.Name && (previous.Outcome != retrievalSuccess &&
			(previous.Arguments == string(encoded) || previous.Outcome == retrievalInput) ||
			len(previous.Failures) > 0 && validateScopedRecovery(previous, string(encoded)) == nil) {
			action, recoveryID = retrievalRecovery, previous.ID
		}
	}
	// A useful document read can sit between a partial search failure and its recovery.
	for i := len(c.state.Attempts) - 1; i >= 0; i-- {
		attempt := &c.state.Attempts[i]
		if attempt.GapID == gapID && attempt.Tool == tc.Function.Name &&
			(attempt.Outcome != retrievalSuccess && attempt.Arguments == string(encoded) ||
				len(attempt.Failures) > 0 && validateScopedRecovery(attempt, string(encoded)) == nil) {
			action, recoveryID = retrievalRecovery, attempt.ID
			break
		}
		// A newer success supersedes the old failure even when its content was redacted.
		if attempt.GapID == gapID && attempt.Tool == tc.Function.Name &&
			attempt.Arguments == string(encoded) && attempt.Outcome == retrievalSuccess {
			break
		}
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
