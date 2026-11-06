package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const questionSearchGrace = 3

func questionSearchGuidance() string {
	return fmt.Sprintf("The first %d search calls per independent user question or genuinely new applicability branch may omit retrieval metadata and progress reviews. This is not a conversation-wide quota or a total search limit. Rephrasing or changing query/tool/library does not reset it. Afterwards the original progress, supplement, recovery and stopping rules apply. Stop when evidence is sufficient; no satisfied review is needed just to answer. Necessary user-information blockers, permissions and the single failure recovery always apply.", questionSearchGrace)
}

func isBudgetedSearchTool(name string) bool {
	return name == "search_knowledge" || name == "wiki_search" || name == "query_knowledge_graph"
}

// Called with the controller locked, or on an isolated snapshot.
func (c *retrievalControl) searchCount(g *retrievalGap) int {
	count := 0
	for _, attempt := range c.state.Attempts {
		other := c.gap(attempt.GapID)
		if isBudgetedSearchTool(attempt.Tool) && attempt.Outcome != retrievalInput && other != nil &&
			(attempt.GapID == g.ID || sameRetrievalFact(other, g.Fact, g.Conditions)) {
			count++
		}
	}
	return count
}

// Search identity comes from actual user input, not model-supplied gap metadata.
func (e *AgentEngine) prepareBudgetedSearch(ctx context.Context, tc types.LLMToolCall, args map[string]any) (string, error) {
	c := e.retrieval
	var hint retrievalDecision
	if raw, ok := args["retrieval"]; ok {
		encoded, _ := json.Marshal(raw)
		_ = json.Unmarshal(encoded, &hint)
	}
	c.mu.Lock()
	g := c.gap(hint.GapID)
	prerequisite := g != nil && g.Derived && c.factNeeded(g, make(map[string]bool))
	terminalReview := g != nil && hint.Review != nil && (hint.Review.Status == retrievalNeedsUser ||
		hint.Review.Status == retrievalSatisfied || hint.Review.Status == retrievalExhausted)
	c.mu.Unlock()
	// Explicit state changes retain atomic validation before any question binding.
	if hint.ScopeFrom != "" || hint.Source != nil || len(hint.DependsOn) > 0 || prerequisite || terminalReview {
		arguments, err := e.prepareStructuredRetrievalCall(ctx, tc, args)
		if err == nil && hint.ScopeFrom != "" {
			e.retrievalSearchGapID = ""
		}
		return arguments, err
	}
	e.recordRetrievalUserMessage()
	goal := c.snapshot().Goal
	quote, inputID := strings.TrimSpace(e.retrievalUserMessage.text), ""
	c.mu.Lock()
	for i := len(c.state.UserInputIDs) - 1; i >= 0; i-- {
		id := c.state.UserInputIDs[i]
		if !strings.HasPrefix(id, "app_info:") && strings.TrimSpace(c.userInputs[id]) != "" {
			inputID, quote = id, strings.TrimSpace(c.userInputs[id])
			break
		}
	}
	c.mu.Unlock()
	if quote == "" {
		quote = goal
	}
	if e.retrievalSearchGapID == "" || e.retrievalSearchInputID != inputID {
		proposal := retrievalFactProposal{GoalQuote: quote, GoalInputID: inputID, Fact: goal}
		if quote != goal {
			proposal.Fact = goal + "\n" + quote
			proposal.Conditions, proposal.ConditionQuote, proposal.ConditionInputID = quote, quote, inputID
		}
		id, err := e.admitRetrievalFact(ctx, proposal)
		if err != nil {
			return "", err
		}
		e.retrievalSearchGapID = id
		e.retrievalSearchInputID = inputID
	}
	gapID := e.retrievalSearchGapID
	c.mu.Lock()
	g = c.gap(gapID)
	if g == nil {
		c.mu.Unlock()
		return "", fmt.Errorf("current question has no registered retrieval state")
	}
	g.SearchGrace, c.state.Version = questionSearchGrace, 4
	if c.searchCount(g) >= questionSearchGrace {
		c.mu.Unlock()
		return e.prepareStructuredRetrievalCall(ctx, tc, args)
	}
	action := retrievalInitial
	previous := c.attempt(g.LastCallID)
	if previous != nil {
		action = retrievalSearch
		if previous.Outcome != retrievalSuccess {
			action = retrievalRecovery
		}
	}
	c.mu.Unlock()
	// Keep explicit partial-stage recovery compatible with existing callers.
	if review := hint.Review; review != nil {
		if review.NewFact != nil {
			return "", fmt.Errorf("inline review cannot register another fact")
		}
		if review.Status == retrievalNeedsUser || review.Status == retrievalSatisfied ||
			review.Status == retrievalExhausted || review.Status == retrievalUserConditions {
			reviewGapID := gapID
			if review.Status == retrievalUserConditions && hint.GapID != "" {
				// A supplied branch may first resolve the preceding fact's clarification.
				reviewGapID = hint.GapID
			}
			if review.GapID != "" && review.GapID != reviewGapID {
				return "", fmt.Errorf("review must refer to the current gap")
			}
			decisionReview := *review
			decisionReview.GapID = reviewGapID
			e.scopeRetrievalReview(&decisionReview)
			if err := e.applyRetrievalReview(ctx, decisionReview); err != nil {
				return "", err
			}
		}
	}
	recoveryCallID := ""
	if hint.Action == retrievalRecovery {
		c.mu.Lock()
		target := previous
		if hint.RecoveryCallID != "" {
			target = c.attempt(e.resolveRetrievalReference(hint.RecoveryCallID))
		}
		if target != nil && target.GapID == gapID && len(target.Failures) > 0 {
			action = retrievalRecovery
			recoveryCallID = e.resolveRetrievalReference(hint.RecoveryCallID)
		}
		c.mu.Unlock()
	}
	executionArgs := retrievalToolArgs(args)
	encoded, err := json.Marshal(executionArgs)
	if err != nil {
		return "", err
	}
	lead := quote
	if action == retrievalRecovery && hint.Lead != "" {
		lead = hint.Lead
	}
	if err := c.acquire(gapID, e.retrievalCallReference(tc.ID), action, lead, tc.Function.Name, string(encoded), recoveryCallID); err != nil {
		return "", err
	}
	return string(encoded), nil
}
