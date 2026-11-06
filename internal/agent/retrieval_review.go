package agent

import (
	"fmt"
	"slices"
	"strings"
)

// Registering and reviewing are one decision: a rejected review must not leave
// a new fact or prerequisite edge behind for a subsequent search to use.
func (c *retrievalControl) applyReview(review retrievalReview) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.persistenceFailed {
		return errRetrievalStateSave
	}
	if len(review.Evidence) > 0 && (review.Status != retrievalSatisfied || review.CallID != "" || len(review.Quotes) > 0) {
		return fmt.Errorf("evidence is only valid for satisfied and cannot be combined with call_id or quotes")
	}
	next := &retrievalControl{state: c.snapshotLocked(), userInputs: c.userInputs}
	if review.Status == retrievalUserConditions {
		if err := next.resolveUserConditions(review); err != nil {
			return err
		}
		c.state = next.state
		return c.persistLocked()
	}
	if len(review.UserAnswers) > 0 {
		return fmt.Errorf("user_answers is only valid for user_conditions")
	}
	if review.NewFact != nil {
		if review.GapID != "" || (review.Status != retrievalNeedsUser && review.Status != retrievalSatisfied) {
			return fmt.Errorf("new_fact requires either a necessary clarification or sufficient existing evidence, without gap_id")
		}
		var err error
		review.GapID, err = next.register(*review.NewFact)
		if err != nil {
			return err
		}
	}
	g := next.gap(review.GapID)
	if g == nil || g.Status != retrievalPending {
		return fmt.Errorf("review requires a pending registered fact")
	}
	if review.Status != retrievalNeedsUser && len(review.MissingUserFields) > 0 {
		return fmt.Errorf("missing_user_fields is only valid for needs_user")
	}
	switch review.Status {
	case retrievalNeedsUser:
		fields, err := clarificationFields(review.MissingUserFields)
		if err != nil {
			return err
		}
		if err := next.checkDependencies(g); err != nil {
			return err
		}
		for _, answer := range g.UserAnswers {
			if slices.ContainsFunc(fields, func(field string) bool { return strings.EqualFold(field, answer.Field) }) {
				return fmt.Errorf("an already supplied user condition cannot be requested again")
			}
		}
		if review.CallID != "" {
			observation := next.attempt(review.CallID)
			if observation == nil || !retrievalScopeCovers(next.state.Scope, observation.Scope) || observation.GapID != g.ID || g.LastCallID != review.CallID {
				return fmt.Errorf("clarification evidence must refer to this fact's latest observation")
			}
			if err := validateEvidenceQuotes(observation, review.Quotes, false); err != nil {
				return err
			}
		} else if len(review.Quotes) > 0 {
			return fmt.Errorf("evidence quotes require an observed call")
		}
		g.Status, g.LastReview, g.MissingUserFields = retrievalNeedsUser, retrievalNeedsUser, fields
	case retrievalSatisfied:
		sources := review.Evidence
		if len(sources) == 0 {
			sources = []retrievalEvidence{{CallID: review.CallID, Quotes: review.Quotes}}
		}
		if err := next.validateEvidenceSources(g, sources, true); err != nil {
			return err
		}
		if err := next.checkDependencies(g); err != nil {
			return err
		}
		g.Status, g.LastReview = retrievalSatisfied, retrievalSatisfied
		g.EvidenceCallID, g.Evidence = review.CallID, slices.Clone(review.Quotes)
		g.EvidenceSources = cloneRetrievalEvidence(review.Evidence)
	default:
		if err := next.review(g.ID, review.CallID, review.Status, review.Quotes); err != nil {
			return err
		}
	}
	c.state = next.state
	return c.persistLocked()
}

func clarificationFields(fields []string) ([]string, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("needs_user requires the missing necessary user fields; waiting for document evidence is not a user condition")
	}
	clean := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || slices.ContainsFunc(clean, func(existing string) bool { return strings.EqualFold(existing, field) }) {
			return nil, fmt.Errorf("missing user fields must be nonempty and distinct")
		}
		clean = append(clean, field)
	}
	return clean, nil
}

func validateEvidenceQuotes(observation *retrievalAttempt, quotes []string, required bool) error {
	if observation == nil || !observation.Completed || observation.Outcome != retrievalSuccess || observation.Content == "" {
		return fmt.Errorf("evidence requires an available successful observation")
	}
	if required && len(quotes) == 0 {
		return fmt.Errorf("satisfied requires actual evidence")
	}
	for i, quote := range quotes {
		if !evidenceContains(observation.Content, quote) {
			return fmt.Errorf("quotes[%d] was not present in the observed result; copy that excerpt without changing words or conditions", i)
		}
	}
	return nil
}

func (c *retrievalControl) evidenceObservation(g *retrievalGap) *retrievalAttempt {
	if g.EvidenceCallID != "" {
		return c.attempt(g.EvidenceCallID)
	}
	// Older snapshots tied sufficient evidence to the fact's last execution.
	return c.attempt(g.LastCallID)
}

func (c *retrievalControl) evidenceAvailable(g *retrievalGap) bool {
	if len(g.EvidenceSources) > 0 {
		return g.Status == retrievalSatisfied && c.validateEvidenceSources(g, g.EvidenceSources, true) == nil
	}
	a := c.evidenceObservation(g)
	return g.Status == retrievalSatisfied && a != nil && retrievalScopeCovers(c.state.Scope, a.Scope) &&
		retrievalScopeCovers(g.Scope, a.Scope) && validateEvidenceQuotes(a, g.Evidence, true) == nil
}

func (c *retrievalControl) validateReviewState(g *retrievalGap) error {
	if err := c.validateUserAnswers(g); err != nil {
		return err
	}
	if len(g.MissingUserFields) > 0 {
		if g.Status != retrievalNeedsUser || g.LastReview != retrievalNeedsUser {
			return fmt.Errorf("saved user conditions do not match the fact state")
		}
		if _, err := clarificationFields(g.MissingUserFields); err != nil {
			return err
		}
	}
	if (g.EvidenceCallID != "" || len(g.EvidenceSources) > 0) && g.Status != retrievalSatisfied {
		return fmt.Errorf("saved evidence reference does not match the fact state")
	}
	if g.Status != retrievalSatisfied {
		return nil
	}
	if len(g.EvidenceSources) > 0 {
		if g.LastReview != retrievalSatisfied || g.EvidenceCallID != "" || len(g.Evidence) != 0 {
			return fmt.Errorf("saved evidence has conflicting source representations")
		}
		return c.validateEvidenceSources(g, g.EvidenceSources, false)
	}
	observation := c.evidenceObservation(g)
	if g.LastReview != retrievalSatisfied || observation == nil || !observation.Completed ||
		observation.Outcome != retrievalSuccess || len(g.Evidence) == 0 || !retrievalScopeCovers(g.Scope, observation.Scope) {
		return fmt.Errorf("saved satisfied fact has no supporting observation")
	}
	for _, quote := range g.Evidence {
		if strings.TrimSpace(quote) == "" || observation.Content != "" && !evidenceContains(observation.Content, quote) {
			return fmt.Errorf("saved satisfied fact has invalid evidence")
		}
	}
	return nil
}

func cloneRetrievalEvidence(sources []retrievalEvidence) []retrievalEvidence {
	copy := slices.Clone(sources)
	for i := range copy {
		copy[i].Quotes = slices.Clone(copy[i].Quotes)
	}
	return copy
}

func (c *retrievalControl) validateEvidenceSources(g *retrievalGap, sources []retrievalEvidence, requireContent bool) error {
	seen := make(map[string]bool, len(sources))
	for i, source := range sources {
		a := c.attempt(source.CallID)
		if seen[source.CallID] || a == nil || !a.Completed || a.Outcome != retrievalSuccess || !retrievalScopeCovers(g.Scope, a.Scope) ||
			requireContent && !retrievalScopeCovers(c.state.Scope, a.Scope) || len(source.Quotes) == 0 {
			return fmt.Errorf("evidence[%d] requires distinct, successful, applicable sources and quotes", i)
		}
		seen[source.CallID] = true
		for _, quote := range source.Quotes {
			if strings.TrimSpace(quote) == "" {
				return fmt.Errorf("evidence[%d] contains an empty quote", i)
			}
		}
		if requireContent || a.Content != "" {
			if err := validateEvidenceQuotes(a, source.Quotes, true); err != nil {
				return fmt.Errorf("evidence[%d] (%s): %w", i, source.CallID, err)
			}
		}
	}
	return nil
}
