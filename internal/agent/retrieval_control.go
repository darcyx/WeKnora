package agent

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
)

type retrievalStatus string
type retrievalAction string
type retrievalOutcome string

const (
	retrievalPending     retrievalStatus = "pending"
	retrievalInFlight    retrievalStatus = "in_flight"
	retrievalSatisfied   retrievalStatus = "satisfied"
	retrievalNeedsUser   retrievalStatus = "needs_user"
	retrievalExhausted   retrievalStatus = "exhausted_no_progress"
	retrievalUnavailable retrievalStatus = "tool_unavailable"
	// Progress and no_progress are observations, not terminal gap states.
	retrievalProgress   retrievalStatus = "progress"
	retrievalNoProgress retrievalStatus = "no_progress"

	retrievalInitial    retrievalAction = "initial"
	retrievalRead       retrievalAction = "read"
	retrievalSupplement retrievalAction = "supplement"
	retrievalRecovery   retrievalAction = "recovery"
	retrievalSearch     retrievalAction = "search"

	retrievalSuccess   retrievalOutcome = "success"
	retrievalTransient retrievalOutcome = "transient"
	retrievalInput     retrievalOutcome = "invalid_input"
	retrievalUnknown   retrievalOutcome = "unavailable"
)

type retrievalGap struct {
	BlocksWholeGoal   bool                   `json:"blocks_whole_goal,omitempty"`
	ID                string                 `json:"id"`
	Scope             string                 `json:"scope"`
	ScopeFrom         string                 `json:"scope_from,omitempty"`
	Fact              string                 `json:"fact"`
	GoalQuote         string                 `json:"goal_quote"`
	GoalInputID       string                 `json:"goal_input_id,omitempty"`
	Conditions        string                 `json:"conditions,omitempty"`
	ConditionQuote    string                 `json:"condition_quote,omitempty"`
	ConditionInputID  string                 `json:"condition_input_id,omitempty"`
	DistinctFrom      []retrievalDistinction `json:"distinct_from,omitempty"`
	Origins           []retrievalFactSource  `json:"origins,omitempty"`
	DependsOn         []string               `json:"depends_on,omitempty"`
	Derived           bool                   `json:"derived,omitempty"`
	Status            retrievalStatus        `json:"status"`
	LastCallID        string                 `json:"last_call_id,omitempty"`
	LastReview        retrievalStatus        `json:"last_review,omitempty"`
	Evidence          []string               `json:"evidence,omitempty"`
	EvidenceCallID    string                 `json:"evidence_call_id,omitempty"`
	EvidenceSources   []retrievalEvidence    `json:"evidence_sources,omitempty"`
	MissingUserFields []string               `json:"missing_user_fields,omitempty"`
	UserAnswers       []retrievalUserAnswer  `json:"user_answers,omitempty"`
	SupplementUsed    bool                   `json:"supplement_used"`
	RecoveryUsed      bool                   `json:"recovery_used"`
	SearchGrace       int                    `json:"search_grace,omitempty"`
}

type retrievalAttempt struct {
	QuestionID      string              `json:"question_id,omitempty"`
	FAQOnly         bool                `json:"faq_only,omitempty"`
	RuntimeManaged  bool                `json:"runtime_managed,omitempty"`
	ID              string              `json:"id"`
	Scope           string              `json:"scope"`
	GapID           string              `json:"gap_id"`
	Action          retrievalAction     `json:"action"`
	Tool            string              `json:"tool"`
	Arguments       string              `json:"arguments"`
	Completed       bool                `json:"completed"`
	Outcome         retrievalOutcome    `json:"outcome,omitempty"`
	Failures        []retrievalFailure  `json:"failures,omitempty"`
	RecoveryCallID  string              `json:"recovery_call_id,omitempty"`
	RecoveryTargets types.SearchTargets `json:"recovery_targets,omitempty"`
	RecoveryStages  []string            `json:"recovery_stages,omitempty"`
	FAQ             *retrievalFAQState  `json:"faq,omitempty"`
	// Raw results must be restored through the authorized history path.
	Content string `json:"-"`
}

type retrievalState struct {
	Version                int                `json:"version"`
	Goal                   string             `json:"goal"`
	Scope                  string             `json:"scope"`
	Scopes                 []string           `json:"scopes"`
	Gaps                   []retrievalGap     `json:"gaps"`
	Attempts               []retrievalAttempt `json:"attempts"`
	DecisionCorrectionUsed bool               `json:"decision_correction_used"`
	UserInputIDs           []string           `json:"user_input_ids,omitempty"`
}

type retrievalControl struct {
	mu                sync.Mutex
	state             retrievalState
	userInputs        map[string]string // Rehydrate from actual user messages or accepted ask_user results.
	persist           func(retrievalState) error
	persistenceFailed bool
}

func newRetrievalControl(goal, scope string) *retrievalControl {
	return &retrievalControl{state: retrievalState{Version: 3, Goal: goal, Scope: scope, Scopes: []string{scope}}}
}

func (c *retrievalControl) gap(id string) *retrievalGap {
	for i := range c.state.Gaps {
		if c.state.Gaps[i].ID == id {
			return &c.state.Gaps[i]
		}
	}
	return nil
}

func (c *retrievalControl) attempt(id string) *retrievalAttempt {
	for i := range c.state.Attempts {
		if c.state.Attempts[i].ID == id {
			return &c.state.Attempts[i]
		}
	}
	return nil
}

func (c *retrievalControl) acquire(gapID, callID string, action retrievalAction, lead, tool, arguments string, recoveryCallID ...string) error {
	return c.acquireWithAdmission(gapID, callID, action, lead, tool, arguments, retrievalExecutionAdmission{}, recoveryCallID...)
}

type retrievalExecutionAdmission struct {
	ObservedRead   bool
	RuntimeManaged bool
}

func (c *retrievalControl) acquireWithAdmission(gapID, callID string, action retrievalAction, lead, tool, arguments string, admission retrievalExecutionAdmission, recoveryCallID ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.persistenceFailed {
		return errRetrievalStateSave
	}
	g := c.gap(gapID)
	if g == nil || g.Status != retrievalPending && !(admission.RuntimeManaged && g.Status == retrievalUnavailable) {
		return fmt.Errorf("gap %s is unknown, blocked or already in flight", gapID)
	}
	executionScope := retrievalExecutionScope(c.state.Scope, tool, arguments)
	if err := c.checkDependencies(g); err != nil {
		return err
	}
	if callID == "" || tool == "" || c.attempt(callID) != nil {
		return fmt.Errorf("call must have a new identity; completed calls cannot be replayed")
	}
	previous := c.attempt(g.LastCallID)
	failures := outstandingRetrievalFailures(c.state.Attempts, gapID)
	if len(recoveryCallID) > 0 && recoveryCallID[0] != "" {
		if action != retrievalRecovery {
			return fmt.Errorf("recovery_call_id is only valid for a recovery")
		}
		previous = c.attempt(recoveryCallID[0])
		if previous == nil || previous.GapID != gapID {
			return fmt.Errorf("recovery must refer to an observed call of this gap")
		}
	}
	groundedLead := admission.ObservedRead && tool == "read_document" || lead != "" && (c.hasUserQuote(lead) ||
		previous != nil && evidenceContains(previous.Content, lead))
	var recoveryTargets types.SearchTargets
	var recoveredCallID string
	switch action {
	case retrievalSearch:
		if g.SearchGrace == questionSearchGrace && c.countSearches(g, c.state.questionID(), callID) >= g.SearchGrace {
			return fmt.Errorf("ordinary search allowance exhausted for gap %s; use the allowances in the retrieval state or answer from available evidence", gapID)
		}
		if g.SearchGrace != questionSearchGrace || !isBudgetedSearchTool(tool) || previous == nil || !previous.Completed || !admission.RuntimeManaged && previous.Outcome != retrievalSuccess {
			return fmt.Errorf("ordinary follow-up search requires an eligible search tool, a completed preceding call and an active search allowance")
		}
		if len(failures) > 0 && !independentRetrievalScope(failures, arguments) {
			return fmt.Errorf("a search cannot retry a failed scope; use the original recovery allowance")
		}
	case retrievalInitial:
		if previous != nil {
			return fmt.Errorf("gap %s has already been searched", gapID)
		}
		if id := c.state.questionID(); id != "" && isBudgetedSearchTool(tool) && c.countSearches(g, id, callID) >= questionSearchGrace {
			return fmt.Errorf("this user message has spent its ordinary search allowance")
		}
	case retrievalRead:
		for _, attempt := range c.state.Attempts {
			if attempt.GapID == gapID && attempt.Tool == tool && attempt.Arguments == arguments && attempt.Completed && attempt.Outcome == retrievalSuccess && attempt.Content != "" {
				return fmt.Errorf("this source range has already been read for the same fact")
			}
		}
		observedRead := (admission.RuntimeManaged || admission.ObservedRead && tool == "read_document" && g.LastReview == "") &&
			previous != nil && previous.Completed && (admission.RuntimeManaged || previous.Outcome == retrievalSuccess)
		if (g.LastReview != retrievalProgress && !observedRead) || !groundedLead {
			return fmt.Errorf("necessary reading requires observed progress and a grounded lead")
		}
		if tool != "read_document" && tool != "wiki_read_page" && tool != "list_knowledge_chunks" &&
			tool != "get_document_info" && tool != "wiki_read_source_doc" && !(admission.RuntimeManaged && tool == "list_documents") {
			return fmt.Errorf("searching or listing sources cannot be relabeled as necessary reading")
		}
	case retrievalSupplement:
		if g.LastReview != retrievalNoProgress || g.SupplementUsed || !groundedLead {
			return fmt.Errorf("no grounded supplementary retrieval remains for gap %s", gapID)
		}
		if len(failures) > 0 && !independentRetrievalScope(failures, arguments) {
			return fmt.Errorf("a supplement cannot retry a failed scope; use the original recovery allowance")
		}
	case retrievalRecovery:
		if previous == nil || !previous.Completed || !admission.RuntimeManaged && g.RecoveryUsed || previous.Tool != tool {
			return fmt.Errorf("no safe recovery remains for gap %s", gapID)
		}
		if admission.RuntimeManaged && c.recoverySpent(previous, "") {
			return fmt.Errorf("recovery already used for this operation")
		}
		if len(previous.Failures) > 0 {
			if previous.Outcome == retrievalSuccess && !groundedLead {
				return fmt.Errorf("partial-result recovery requires a grounded lead for the missing fact")
			}
			if err := validateScopedRecovery(previous, arguments); err != nil {
				return err
			}
			var err error
			recoveryTargets, err = scopedRecoveryTargets(previous, arguments)
			if err != nil {
				return err
			}
			if len(recoveryTargets) > 0 {
				executionScope = retrievalScopeForTargets(recoveryTargets)
			}
		} else {
			if previous.Outcome != retrievalTransient && previous.Outcome != retrievalInput {
				return fmt.Errorf("the previous outcome is not a recoverable tool failure")
			}
			if previous.Outcome == retrievalTransient && previous.Arguments != arguments {
				return fmt.Errorf("a transient recovery must retry the original operation")
			}
			if previous.Outcome == retrievalInput && (!groundedLead || previous.Arguments == arguments) {
				return fmt.Errorf("argument correction requires a grounded change")
			}
		}
		recoveredCallID = previous.ID
	default:
		return fmt.Errorf("unknown retrieval action %q", action)
	}
	if !retrievalScopeCovers(c.state.Scope, executionScope) || !retrievalScopeCovers(g.Scope, executionScope) {
		return fmt.Errorf("this call would change the fact's source scope; reuse its covered scope or explicitly re-evaluate the affected fact")
	}
	if g.ScopeFrom != "" {
		var previousScopes []string
		for i := range c.state.Gaps {
			previous := &c.state.Gaps[i]
			if previous.ID == g.ID {
				break
			}
			if sameRetrievalFact(previous, g.Fact, g.Conditions) {
				previousScopes = append(previousScopes, previous.Scope)
			}
		}
		if retrievalScopesOverlap(previousScopes, executionScope) {
			return fmt.Errorf("this call includes an earlier source range; select only newly available sources and use the original fact's state and allowances for earlier sources")
		}
	}
	if action == retrievalSupplement {
		g.SupplementUsed = true
	}
	if action == retrievalRecovery {
		g.RecoveryUsed = true
	}
	if admission.RuntimeManaged {
		g.Evidence, g.EvidenceCallID = nil, ""
	}
	g.Status, g.LastCallID, g.LastReview = retrievalInFlight, callID, ""
	var stages []string
	if action == retrievalRecovery && previous != nil {
		for _, f := range previous.Failures {
			if (f.Retry != nil || f.Stage == "search" || f.Stage == "embedding") && retrievalScopeCovers(retrievalScopeForTargets(recoveryTargets), retrievalScopeForTargets(f.Targets)) {
				stages = append(stages, f.Stage)
			}
		}
	}
	c.state.Attempts = append(c.state.Attempts, retrievalAttempt{ID: callID, QuestionID: c.state.questionID(), GapID: gapID, RuntimeManaged: admission.RuntimeManaged,
		Scope: executionScope, Action: action, Tool: tool, Arguments: arguments,
		RecoveryCallID: recoveredCallID, RecoveryTargets: recoveryTargets, RecoveryStages: stages})
	return c.persistLocked()
}

func (c *retrievalControl) observe(callID, content string, outcome retrievalOutcome, failures ...retrievalFailure) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempt(callID)
	if a == nil || a.Completed {
		return fmt.Errorf("call %s is unknown or already observed", callID)
	}
	if outcome != retrievalSuccess && outcome != retrievalTransient && outcome != retrievalInput && outcome != retrievalUnknown {
		return fmt.Errorf("unknown retrieval outcome %q", outcome)
	}
	a.Completed, a.Content, a.Outcome = true, content, outcome
	a.Failures = cloneRetrievalFailures(failures)
	for i := range a.Failures {
		a.Failures[i].CallID = callID
	}
	g := c.gap(a.GapID)
	g.Status = retrievalPending
	if outcome != retrievalSuccess && (g.RecoveryUsed ||
		outcome == retrievalUnknown && !hasRecoverableSource(failures)) {
		g.Status = retrievalUnavailable
	}
	return c.persistLocked()
}

func (c *retrievalControl) reviewable(g *retrievalGap, a *retrievalAttempt) bool {
	return g != nil && a != nil && a.GapID == g.ID && g.LastCallID == a.ID &&
		g.Status == retrievalPending && g.LastReview == "" && a.Completed &&
		a.Outcome == retrievalSuccess && retrievalScopeCovers(c.state.Scope, a.Scope)
}

func (c *retrievalControl) review(gapID, callID string, verdict retrievalStatus, quotes []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, a := c.gap(gapID), c.attempt(callID)
	if !c.reviewable(g, a) {
		return fmt.Errorf("review must refer to the gap's unreviewed successful observation")
	}
	newEvidence := false
	if len(quotes) > 0 {
		if err := validateEvidenceQuotes(a, quotes, false); err != nil {
			return err
		}
	}
	for _, quote := range quotes {
		seen := slices.ContainsFunc(g.Evidence, func(prior string) bool { return evidenceContains(prior, quote) })
		for _, previous := range c.state.Attempts {
			if previous.ID != callID && previous.GapID == gapID && previous.Outcome == retrievalSuccess &&
				evidenceContains(previous.Content, quote) {
				seen = true
			}
		}
		if !seen {
			newEvidence = true
		}
	}
	switch verdict {
	case retrievalProgress:
		if !newEvidence {
			return fmt.Errorf("progress requires newly cited evidence for the remaining fact")
		}
	case retrievalSatisfied:
		if len(quotes) == 0 {
			return fmt.Errorf("satisfied requires actual evidence")
		}
		if err := c.checkDependencies(g); err != nil {
			return err
		}
		g.Status = retrievalSatisfied
	case retrievalNeedsUser, retrievalExhausted:
		g.Status = verdict
	case retrievalNoProgress:
		if (g.SearchGrace == 0 || c.countSearches(g, c.state.questionID(), callID) >= g.SearchGrace) && g.SupplementUsed && (g.RecoveryUsed || !hasRecoverableSource(outstandingRetrievalFailures(c.state.Attempts, gapID))) {
			g.Status = retrievalExhausted
		}
	default:
		return fmt.Errorf("invalid evidence verdict %q", verdict)
	}
	g.LastReview = verdict
	if verdict == retrievalSatisfied {
		g.Evidence = slices.Clone(quotes)
		g.EvidenceCallID = callID
		return nil
	}
	for _, quote := range quotes {
		if !slices.Contains(g.Evidence, quote) {
			g.Evidence = append(g.Evidence, quote)
		}
	}
	return nil
}

func (c *retrievalControl) snapshot() retrievalState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *retrievalControl) snapshotLocked() retrievalState {
	s := c.state
	s.Gaps, s.Attempts = slices.Clone(s.Gaps), slices.Clone(s.Attempts)
	s.UserInputIDs = slices.Clone(s.UserInputIDs)
	s.Scopes = slices.Clone(s.Scopes)
	for i := range s.Gaps {
		s.Gaps[i].Evidence = slices.Clone(s.Gaps[i].Evidence)
		s.Gaps[i].EvidenceSources = cloneRetrievalEvidence(s.Gaps[i].EvidenceSources)
		s.Gaps[i].MissingUserFields = slices.Clone(s.Gaps[i].MissingUserFields)
		s.Gaps[i].UserAnswers = slices.Clone(s.Gaps[i].UserAnswers)
		s.Gaps[i].DistinctFrom = slices.Clone(s.Gaps[i].DistinctFrom)
		s.Gaps[i].Origins = slices.Clone(s.Gaps[i].Origins)
		s.Gaps[i].DependsOn = slices.Clone(s.Gaps[i].DependsOn)
	}
	for i := range s.Attempts {
		s.Attempts[i].Failures = cloneRetrievalFailures(s.Attempts[i].Failures)
		s.Attempts[i].RecoveryTargets = s.Attempts[i].RecoveryTargets.Clone()
		s.Attempts[i].RecoveryStages = slices.Clone(s.Attempts[i].RecoveryStages)
		if faq := s.Attempts[i].FAQ; faq != nil {
			copyFAQ := *faq
			copyFAQ.Observations = slices.Clone(faq.Observations)
			copyFAQ.DocumentTargets = faq.DocumentTargets.Clone()
			s.Attempts[i].FAQ = &copyFAQ
		}
	}
	return s
}

func (c *retrievalControl) takeDecisionCorrection() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.DecisionCorrectionUsed {
		return false
	}
	c.state.DecisionCorrectionUsed = true
	return c.persistLocked() == nil
}

func (c *retrievalControl) restoreObservation(callID, content string, tool ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.attempt(callID); a != nil && a.Completed && a.Content == "" &&
		retrievalScopeCovers(c.state.Scope, a.Scope) && (len(tool) == 0 || a.Tool == tool[0]) {
		a.Content = content
	}
}

// A v1/v2 ledger predates scope records; the upgrade path is gone, so callers
// that own a fresh ledger keep it instead (see restoreSavedRetrieval).
var errRetrievalSnapshotOutdated = errors.New("retrieval snapshot predates the supported format")

func restoreRetrievalControl(state retrievalState) (*retrievalControl, error) {
	if state.Version == 1 || state.Version == 2 {
		return nil, errRetrievalSnapshotOutdated
	}
	if state.Version != 3 && state.Version != 4 {
		return nil, fmt.Errorf("unsupported retrieval snapshot version %d", state.Version)
	}
	c := &retrievalControl{state: state}
	// Copy caller-owned slices before reconciling interrupted dispatches.
	c.state = c.snapshot()
	if err := c.restoreScopes(); err != nil {
		return nil, err
	}
	if err := c.validateUserInputs(); err != nil {
		return nil, err
	}
	gaps := make(map[string]*retrievalGap, len(c.state.Gaps))
	for i := range c.state.Gaps {
		g := &c.state.Gaps[i]
		if g.SearchGrace != 0 && ((g.SearchGrace != 2 && g.SearchGrace != questionSearchGrace) || state.Version < 4) {
			return nil, fmt.Errorf("invalid saved question search limit")
		}
		if g.SearchGrace == 2 {
			g.SearchGrace = questionSearchGrace // Preserve attempts and spent recovery/supplement allowances.
		}
		if g.ID != fmt.Sprintf("gap-%d", i+1) || gaps[g.ID] != nil || strings.TrimSpace(g.Fact) == "" ||
			!c.factGoalGrounded(g.GoalInputID, g.GoalQuote, true) {
			return nil, fmt.Errorf("inconsistent retrieval gap identity")
		}
		switch g.Status {
		case retrievalPending, retrievalInFlight, retrievalSatisfied, retrievalNeedsUser, retrievalExhausted, retrievalUnavailable:
		default:
			return nil, fmt.Errorf("invalid retrieval gap state %q", g.Status)
		}
		switch g.LastReview {
		case "", retrievalProgress, retrievalNoProgress, retrievalSatisfied, retrievalNeedsUser, retrievalExhausted:
		default:
			return nil, fmt.Errorf("invalid retrieval review %q", g.LastReview)
		}
		gaps[g.ID] = g
	}
	seen := make(map[string]bool)
	last := make(map[string]*retrievalAttempt)
	supplements, recoveries := make(map[string]int), make(map[string]int)
	strictRecoveries := make(map[string]int)
	for i := range c.state.Attempts {
		a := &c.state.Attempts[i]
		g := gaps[a.GapID]
		if g == nil || a.ID == "" || seen[a.ID] || a.Tool == "" ||
			!retrievalScopeCovers(g.Scope, a.Scope) || !retrievalScopesCover(c.state.Scopes, a.Scope) {
			return nil, fmt.Errorf("inconsistent retrieval snapshot")
		}
		seen[a.ID] = true
		if a.QuestionID != "" && (!slices.Contains(c.state.UserInputIDs, a.QuestionID) || strings.HasPrefix(a.QuestionID, "app_info:")) {
			return nil, fmt.Errorf("unknown saved search question")
		}
		if err := validateRetrievalFAQ(a, g.Scope); err != nil {
			return nil, err
		}
		for _, failure := range a.Failures {
			if !a.Completed || failure.CallID != a.ID || len(failure.KnowledgeBaseIDs) == 0 ||
				(failure.Outcome != retrievalTransient && failure.Outcome != retrievalUnknown) {
				return nil, fmt.Errorf("invalid retrieval failure observation")
			}
			ids := make(map[string]bool)
			for _, id := range failure.KnowledgeBaseIDs {
				if strings.TrimSpace(id) == "" || ids[id] {
					return nil, fmt.Errorf("invalid retrieval failure scope")
				}
				ids[id] = true
			}
			if err := validateRetrievalFailureTargets(a, failure); err != nil {
				return nil, err
			}
		}
		previous := last[g.ID]
		if err := c.validateRecoveryReservation(a); err != nil {
			return nil, err
		}
		if (previous == nil) != (a.Action == retrievalInitial) || previous != nil && !previous.Completed {
			return nil, fmt.Errorf("inconsistent retrieval attempt order")
		}
		switch a.Action {
		case retrievalSearch:
			prefix := &retrievalControl{state: c.state}
			prefix.state.Attempts = prefix.state.Attempts[:i]
			ref := a.ID
			if a.QuestionID == "" {
				ref = "" // Older snapshots validated search grace per fact.
			}
			if g.SearchGrace != questionSearchGrace || !a.FAQOnly && prefix.countSearches(g, a.QuestionID, ref) >= g.SearchGrace || !isBudgetedSearchTool(a.Tool) || !a.RuntimeManaged && previous.Outcome != retrievalSuccess {
				return nil, fmt.Errorf("invalid saved budgeted search")
			}
			if failures := outstandingRetrievalFailures(prefix.state.Attempts, g.ID); len(failures) > 0 && !independentRetrievalScope(failures, a.Arguments) {
				return nil, fmt.Errorf("saved search retries a failed scope without recovery")
			}
		case retrievalInitial, retrievalRead:
		case retrievalSupplement:
			supplements[g.ID]++
		case retrievalRecovery:
			recoveries[g.ID]++
			if !a.RuntimeManaged {
				strictRecoveries[g.ID]++
			}
		default:
			return nil, fmt.Errorf("invalid retrieval action %q", a.Action)
		}
		if a.Completed {
			switch a.Outcome {
			case retrievalSuccess, retrievalTransient, retrievalInput, retrievalUnknown:
			default:
				return nil, fmt.Errorf("invalid retrieval outcome %q", a.Outcome)
			}
		} else if a.Outcome != "" {
			return nil, fmt.Errorf("unfinished retrieval has a completed outcome")
		}
		last[g.ID] = a
	}
	for id, g := range gaps {
		if err := c.validateGapScope(g); err != nil {
			return nil, err
		}
		if err := c.validateFactLinks(g); err != nil {
			return nil, err
		}
		if err := c.validateReviewState(g); err != nil {
			return nil, err
		}
		if supplements[id] > 1 || strictRecoveries[id] > 1 ||
			g.SupplementUsed != (supplements[id] == 1) || g.RecoveryUsed != (recoveries[id] > 0) {
			return nil, fmt.Errorf("inconsistent retrieval allowances")
		}
		a := last[id]
		if a == nil {
			if g.LastCallID != "" || !((g.Status == retrievalPending && g.LastReview == "") ||
				(g.Status == retrievalNeedsUser && g.LastReview == retrievalNeedsUser && len(g.MissingUserFields) > 0) ||
				(g.Status == retrievalSatisfied && g.LastReview == retrievalSatisfied && (g.EvidenceCallID != "" || len(g.EvidenceSources) > 0))) {
				return nil, fmt.Errorf("retrieval state has no supporting call")
			}
			continue
		}
		if g.LastCallID != a.ID || (g.Status == retrievalInFlight) == a.Completed {
			return nil, fmt.Errorf("retrieval last call does not match its state")
		}
		if !a.Completed {
			// The result is unknown: never turn a possibly executed call into a new initial lookup.
			a.Completed, a.Outcome = true, retrievalUnknown
			g.Status = retrievalUnavailable
		}
	}
	return c, nil
}
