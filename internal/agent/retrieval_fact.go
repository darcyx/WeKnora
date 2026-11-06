package agent

import (
	"fmt"
	"slices"
	"strings"
)

type retrievalDistinction struct {
	GapID  string `json:"gap_id"`
	Reason string `json:"reason"`
}

type retrievalFactSource struct {
	CallID string `json:"call_id"`
	Quote  string `json:"quote"`
}

type retrievalFactProposal struct {
	GoalQuote        string                 `json:"goal_quote,omitempty"`
	GoalInputID      string                 `json:"goal_input_id,omitempty"`
	Fact             string                 `json:"fact,omitempty"`
	Conditions       string                 `json:"conditions,omitempty"`
	ConditionQuote   string                 `json:"condition_quote,omitempty"`
	ConditionInputID string                 `json:"condition_input_id,omitempty"`
	DistinctFrom     []retrievalDistinction `json:"distinct_from,omitempty"`
	DependsOn        []string               `json:"depends_on,omitempty"`
	Source           *retrievalFactSource   `json:"source,omitempty"`
	ScopeFrom        string                 `json:"scope_from,omitempty"`
}

// Saved provenance is trusted only when restoring this execution's snapshot.
func (c *retrievalControl) factGoalGrounded(inputID, quote string, restored bool) bool {
	if strings.TrimSpace(quote) == "" {
		return false
	}
	if inputID == "" {
		return strings.Contains(c.state.Goal, quote)
	}
	if !slices.Contains(c.state.UserInputIDs, inputID) {
		return false
	}
	input, available := c.userInputs[inputID]
	return restored && !available || strings.Contains(input, quote)
}

func sameRetrievalFact(g *retrievalGap, fact, conditions string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(g.Fact), " "), strings.Join(strings.Fields(fact), " ")) &&
		strings.EqualFold(strings.Join(strings.Fields(g.Conditions), " "), strings.Join(strings.Fields(conditions), " "))
}

func (c *retrievalControl) register(proposal retrievalFactProposal) (string, error) {
	return c.registerFact(proposal, true)
}

func (c *retrievalControl) registerFact(proposal retrievalFactProposal, requireDistinctions bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.persistenceFailed {
		return "", errRetrievalStateSave
	}
	proposal.GoalQuote, proposal.Fact = strings.TrimSpace(proposal.GoalQuote), strings.TrimSpace(proposal.Fact)
	proposal.Conditions, proposal.ConditionQuote = strings.TrimSpace(proposal.Conditions), strings.TrimSpace(proposal.ConditionQuote)
	if proposal.Conditions != "" && proposal.ConditionQuote == "" {
		// An omitted redundant quote is recoverable only from actual input or cited evidence.
		proposal.ConditionQuote = proposal.Conditions
	}
	if proposal.Fact == "" || strings.HasPrefix(proposal.GoalInputID, "app_info:") || !c.factGoalGrounded(proposal.GoalInputID, proposal.GoalQuote, false) {
		return "", fmt.Errorf("a fact requires its user goal quote and the specific fact needed to answer it")
	}
	conditionGrounded := c.factGoalGrounded(proposal.ConditionInputID, proposal.ConditionQuote, false)
	if proposal.ConditionInputID == "" && proposal.Source != nil {
		conditionGrounded = conditionGrounded || strings.Contains(proposal.Source.Quote, proposal.ConditionQuote)
	}
	if (proposal.Conditions == "") != (proposal.ConditionQuote == "") || proposal.ConditionInputID != "" && proposal.Conditions == "" ||
		proposal.Conditions != "" && !conditionGrounded {
		return "", fmt.Errorf("fact conditions require a quote from the user goal or cited prerequisite evidence")
	}
	id := fmt.Sprintf("gap-%d", len(c.state.Gaps)+1)
	var existing *retrievalGap
	var coveredScopes []string
	for i := range c.state.Gaps {
		g := &c.state.Gaps[i]
		if sameRetrievalFact(g, proposal.Fact, proposal.Conditions) {
			coveredScopes = append(coveredScopes, g.Scope)
			if g.Scope == c.state.Scope || existing == nil && retrievalScopeCovers(g.Scope, c.state.Scope) {
				id, existing = g.ID, g
			}
		}
	}
	if proposal.ScopeFrom != "" {
		previous := c.gap(proposal.ScopeFrom)
		if previous == nil || !sameRetrievalFact(previous, proposal.Fact, proposal.Conditions) ||
			retrievalScopesCover(coveredScopes, c.state.Scope) {
			return "", fmt.Errorf("scope_from requires the same fact and a verified source range not already covered; narrowing or returning does not reset allowances")
		}
		if _, ok := decodeRetrievalScope(c.state.Scope); !ok {
			return "", fmt.Errorf("a source scope revision requires verified current targets")
		}
	} else if existing == nil && len(coveredScopes) > 0 {
		return "", fmt.Errorf("re-evaluating this fact in a changed source range requires scope_from and a reason distinguishing the affected scope")
	}
	if existing == nil {
		seen := make(map[string]bool, len(proposal.DistinctFrom))
		for _, distinction := range proposal.DistinctFrom {
			if c.gap(distinction.GapID) == nil || seen[distinction.GapID] || strings.TrimSpace(distinction.Reason) == "" {
				return "", fmt.Errorf("new facts must explain their difference from known gaps exactly once")
			}
			seen[distinction.GapID] = true
		}
		for _, gap := range c.state.Gaps {
			if requireDistinctions && !seen[gap.ID] {
				return "", fmt.Errorf("reuse %s or state the independently needed fact that distinguishes it; a new wording is not a new gap", gap.ID)
			}
		}
	} else if len(proposal.DistinctFrom) != 0 || len(proposal.DependsOn) != 0 {
		return "", fmt.Errorf("fact already exists as %s; reuse its identity and dependencies", id)
	}
	seenDependencies := make(map[string]bool, len(proposal.DependsOn))
	for _, dependency := range proposal.DependsOn {
		if c.gap(dependency) == nil || dependency == id || seenDependencies[dependency] {
			return "", fmt.Errorf("fact dependencies must be distinct registered gaps")
		}
		seenDependencies[dependency] = true
	}
	var parent *retrievalGap
	if source := proposal.Source; source != nil {
		observation := c.attempt(source.CallID)
		if observation == nil || !retrievalScopeCovers(c.state.Scope, observation.Scope) || !observation.Completed || observation.Outcome != retrievalSuccess ||
			strings.TrimSpace(source.Quote) == "" || !strings.Contains(observation.Content, source.Quote) {
			return "", fmt.Errorf("a discovered prerequisite requires a quote from an actual successful observation")
		}
		parent = c.gap(observation.GapID)
		if parent.Status != retrievalPending {
			return "", fmt.Errorf("a prerequisite cannot reopen a closed fact")
		}
		dependencies := proposal.DependsOn
		if existing != nil {
			dependencies = existing.DependsOn
		}
		if parent.ID == id || c.dependsOn(id, parent.ID, make(map[string]bool)) {
			return "", fmt.Errorf("prerequisite would create a dependency cycle")
		}
		for _, dependency := range dependencies {
			if dependency == parent.ID || c.dependsOn(dependency, parent.ID, make(map[string]bool)) {
				return "", fmt.Errorf("prerequisite would create a dependency cycle")
			}
		}
	}
	if existing == nil {
		c.state.Gaps = append(c.state.Gaps, retrievalGap{ID: id, Scope: c.state.Scope, ScopeFrom: proposal.ScopeFrom, Fact: proposal.Fact, GoalQuote: proposal.GoalQuote,
			GoalInputID: proposal.GoalInputID, Conditions: proposal.Conditions, ConditionQuote: proposal.ConditionQuote, ConditionInputID: proposal.ConditionInputID,
			Status: retrievalPending, DistinctFrom: slices.Clone(proposal.DistinctFrom), DependsOn: slices.Clone(proposal.DependsOn), Derived: proposal.Source != nil})
	}
	if proposal.Source != nil {
		// Appending a gap can relocate the slice, so resolve both identities again.
		parent = c.gap(parent.ID)
		gap := c.gap(id)
		if !slices.Contains(parent.DependsOn, id) {
			parent.DependsOn = append(parent.DependsOn, id)
		}
		if !slices.Contains(gap.Origins, *proposal.Source) {
			gap.Origins = append(gap.Origins, *proposal.Source)
		}
	}
	return id, nil
}

func (c *retrievalControl) dependsOn(id, target string, seen map[string]bool) bool {
	if seen[id] {
		return false
	}
	seen[id] = true
	if gap := c.gap(id); gap != nil {
		for _, dependency := range gap.DependsOn {
			if dependency == target || c.dependsOn(dependency, target, seen) {
				return true
			}
		}
	}
	return false
}

func (c *retrievalControl) factNeeded(g *retrievalGap, seen map[string]bool) bool {
	if !g.Derived {
		return true
	}
	if seen[g.ID] {
		return false
	}
	seen[g.ID] = true
	for _, source := range g.Origins {
		if observation := c.attempt(source.CallID); observation != nil {
			parent := c.gap(observation.GapID)
			if parent != nil && (parent.Status == retrievalPending || parent.Status == retrievalInFlight) && c.factNeeded(parent, seen) {
				return true
			}
		}
	}
	return false
}

func (c *retrievalControl) checkDependencies(g *retrievalGap) error {
	if !c.factNeeded(g, make(map[string]bool)) {
		return fmt.Errorf("prerequisite %s has no executable parent fact", g.ID)
	}
	for _, id := range g.DependsOn {
		dependency := c.gap(id)
		if dependency == nil || dependency.Status != retrievalSatisfied {
			return fmt.Errorf("gap %s waits for prerequisite %s", g.ID, id)
		}
		if !c.evidenceAvailable(dependency) {
			return fmt.Errorf("prerequisite %s has no currently available evidence", id)
		}
	}
	return nil
}

func (c *retrievalControl) validateFactLinks(g *retrievalGap) error {
	conditionGrounded := c.factGoalGrounded(g.ConditionInputID, g.ConditionQuote, true)
	for _, source := range g.Origins {
		conditionGrounded = conditionGrounded || g.ConditionInputID == "" && g.ConditionQuote != "" && strings.Contains(source.Quote, g.ConditionQuote)
	}
	if (g.Conditions == "") != (g.ConditionQuote == "") || g.ConditionInputID != "" && g.Conditions == "" || g.Conditions != "" && !conditionGrounded {
		return fmt.Errorf("saved fact conditions have no source")
	}
	if g.Derived && len(g.Origins) == 0 {
		return fmt.Errorf("saved prerequisite has no source")
	}
	seen := make(map[string]bool)
	for _, distinction := range g.DistinctFrom {
		if distinction.GapID == g.ID || c.gap(distinction.GapID) == nil || seen[distinction.GapID] || strings.TrimSpace(distinction.Reason) == "" {
			return fmt.Errorf("invalid saved fact distinction")
		}
		seen[distinction.GapID] = true
	}
	seen = make(map[string]bool)
	for _, id := range g.DependsOn {
		if c.gap(id) == nil || seen[id] || id == g.ID || c.dependsOn(id, g.ID, make(map[string]bool)) {
			return fmt.Errorf("invalid saved prerequisite graph")
		}
		seen[id] = true
	}
	for _, source := range g.Origins {
		observation := c.attempt(source.CallID)
		if observation == nil || !observation.Completed || observation.Outcome != retrievalSuccess || strings.TrimSpace(source.Quote) == "" {
			return fmt.Errorf("invalid saved prerequisite source")
		}
		parent := c.gap(observation.GapID)
		if parent == nil || !slices.Contains(parent.DependsOn, g.ID) ||
			observation.Content != "" && !strings.Contains(observation.Content, source.Quote) {
			return fmt.Errorf("saved prerequisite has no matching parent observation")
		}
	}
	return nil
}
