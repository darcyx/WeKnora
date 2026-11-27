package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
)

type retrievalAction string
type retrievalOutcome string

const (
	retrievalInitial    retrievalAction = "initial"
	retrievalRead       retrievalAction = "read"
	retrievalSearch     retrievalAction = "search"
	retrievalSupplement retrievalAction = "supplement"
	retrievalRecovery   retrievalAction = "recovery"

	retrievalSuccess   retrievalOutcome = "success"
	retrievalTransient retrievalOutcome = "transient"
	retrievalInput     retrievalOutcome = "invalid_input"
	retrievalUnknown   retrievalOutcome = "unavailable"

	retrievalDispatchBlocked = "retrieval_dispatch_blocked"
	retrievalCallPrefix      = "retrieval-call/"
	retrievalStateVersion    = 4

	// maxSearchesPerQuestion caps ordinary KB searches per user question so a
	// turn cannot spin indefinitely without mutating tool schemas mid-turn.
	maxSearchesPerQuestion = 3
)

var (
	errRetrievalStateSave        = errors.New("retrieval state could not be saved; stop dependent retrieval, do not retry this as an empty search")
	errRetrievalSnapshotOutdated = errors.New("retrieval snapshot predates the supported format")
)

type retrievalAttempt struct {
	QuestionID        string              `json:"question_id,omitempty"`
	ID                string              `json:"id"`
	Scope             string              `json:"scope"`
	Action            retrievalAction     `json:"action"`
	Tool              string              `json:"tool"`
	Arguments         string              `json:"arguments"`
	Completed         bool                `json:"completed"`
	Outcome           retrievalOutcome    `json:"outcome,omitempty"`
	Failures          []retrievalFailure  `json:"failures,omitempty"`
	RecoveryCallID    string              `json:"recovery_call_id,omitempty"`
	RecoveryTargets   types.SearchTargets `json:"recovery_targets,omitempty"`
	RecoveryStages    []string            `json:"recovery_stages,omitempty"`
	StructuredSources bool                `json:"structured_sources,omitempty"`
	ObservedIDs       []string            `json:"observed_ids,omitempty"`
	CitableIDs        []string            `json:"citable_ids,omitempty"`
	// Raw results must be restored through the authorized history path.
	Content string `json:"-"`
}

type retrievalState struct {
	Version                int                `json:"version"`
	Goal                   string             `json:"goal"`
	Scope                  string             `json:"scope"`
	Scopes                 []string           `json:"scopes"`
	Attempts               []retrievalAttempt `json:"attempts"`
	DecisionCorrectionUsed bool               `json:"decision_correction_used"`
	UserInputIDs           []string           `json:"user_input_ids,omitempty"`
	AwaitingUserInput      bool               `json:"awaiting_user_input,omitempty"`
}

// retrievalControl is the single deterministic runtime retrieval ledger.
// It enforces:
//   - tool schema stability (zero mid-turn schema mutation, preserving prompt prefix cache),
//   - exact duplicate search blocking within the current question,
//   - bounded per-question search budget,
//   - read_document observed-ID validation against authorized runtime/tool sources,
//   - scoped one-shot recovery for partial/transient KB failures, and
//   - source-scope visibility enforcement across history/resume.
type retrievalControl struct {
	mu                sync.Mutex
	state             retrievalState
	userInputs        map[string]string
	persist           func(retrievalState) error
	persistenceFailed bool
}

func newRetrievalControl(goal, scope string) *retrievalControl {
	return &retrievalControl{
		state: retrievalState{
			Version: retrievalStateVersion,
			Goal:    goal,
			Scope:   scope,
			Scopes:  []string{scope},
		},
	}
}

func restoreRetrievalControl(saved retrievalState) (*retrievalControl, error) {
	if saved.Version == 1 || saved.Version == 2 {
		return nil, fmt.Errorf("%w: version %d", errRetrievalSnapshotOutdated, saved.Version)
	}
	if saved.Version != 3 && saved.Version != 4 {
		return nil, fmt.Errorf("unsupported retrieval snapshot version %d", saved.Version)
	}
	c := &retrievalControl{state: cloneRetrievalState(saved)}
	if err := c.restoreScopes(); err != nil {
		return nil, err
	}
	seenIDs := make(map[string]bool, len(c.state.Attempts))
	for i := range c.state.Attempts {
		a := &c.state.Attempts[i]
		if strings.TrimSpace(a.ID) == "" || seenIDs[a.ID] || strings.TrimSpace(a.Tool) == "" ||
			!retrievalScopesCover(c.state.Scopes, a.Scope) {
			return nil, fmt.Errorf("inconsistent retrieval snapshot")
		}
		seenIDs[a.ID] = true
		if a.QuestionID != "" && (!slices.Contains(c.state.UserInputIDs, a.QuestionID) || strings.HasPrefix(a.QuestionID, "app_info:")) {
			return nil, fmt.Errorf("unknown saved search question")
		}
		switch a.Action {
		case retrievalInitial, retrievalRead, retrievalSearch, retrievalSupplement, retrievalRecovery:
		default:
			return nil, fmt.Errorf("invalid retrieval action %q", a.Action)
		}
		for _, failure := range a.Failures {
			if !a.Completed || failure.CallID != a.ID || len(failure.KnowledgeBaseIDs) == 0 ||
				(failure.Outcome != retrievalTransient && failure.Outcome != retrievalUnknown) {
				return nil, fmt.Errorf("invalid retrieval failure observation")
			}
			kbSeen := make(map[string]bool, len(failure.KnowledgeBaseIDs))
			for _, id := range failure.KnowledgeBaseIDs {
				if strings.TrimSpace(id) == "" || kbSeen[id] {
					return nil, fmt.Errorf("invalid retrieval failure scope")
				}
				kbSeen[id] = true
			}
			if err := validateRetrievalFailureTargets(a, failure); err != nil {
				return nil, err
			}
		}
		if err := c.validateRecoveryReservation(a); err != nil {
			return nil, err
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
	}
	for i := range c.state.Attempts {
		if !c.state.Attempts[i].Completed {
			c.state.Attempts[i].Completed = true
			c.state.Attempts[i].Outcome = retrievalUnknown
		}
	}
	c.state.Version = retrievalStateVersion
	return c, nil
}

func cloneRetrievalState(s retrievalState) retrievalState {
	out := s
	out.Scopes = slices.Clone(s.Scopes)
	out.UserInputIDs = slices.Clone(s.UserInputIDs)
	out.Attempts = make([]retrievalAttempt, len(s.Attempts))
	for i, a := range s.Attempts {
		a.Failures = cloneRetrievalFailures(a.Failures)
		a.RecoveryTargets = a.RecoveryTargets.Clone()
		a.RecoveryStages = slices.Clone(a.RecoveryStages)
		a.ObservedIDs = slices.Clone(a.ObservedIDs)
		a.CitableIDs = slices.Clone(a.CitableIDs)
		out.Attempts[i] = a
	}
	return out
}

func (c *retrievalControl) snapshot() retrievalState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneRetrievalState(c.state)
}

func (c *retrievalControl) attempt(id string) *retrievalAttempt {
	if id == "" {
		return nil
	}
	for i := range c.state.Attempts {
		if c.state.Attempts[i].ID == id {
			return &c.state.Attempts[i]
		}
	}
	return nil
}

func (s retrievalState) questionID() string {
	for i := len(s.UserInputIDs) - 1; i >= 0; i-- {
		if id := s.UserInputIDs[i]; !strings.HasPrefix(id, "app_info:") {
			return id
		}
	}
	return ""
}

func (c *retrievalControl) recordUserInput(id, text string) {
	if c == nil || strings.TrimSpace(id) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.userInputs == nil {
		c.userInputs = make(map[string]string)
	}
	c.userInputs[id] = text
	if !slices.Contains(c.state.UserInputIDs, id) {
		c.state.UserInputIDs = append(c.state.UserInputIDs, id)
	}
}

func (c *retrievalControl) hasUserQuote(needle string) bool {
	if c == nil || strings.TrimSpace(needle) == "" {
		return false
	}
	if strings.Contains(c.state.Goal, needle) {
		return true
	}
	for _, text := range c.userInputs {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func (c *retrievalControl) saveLocked() error {
	if c.persistenceFailed {
		return errRetrievalStateSave
	}
	if c.persist == nil {
		return nil
	}
	if err := c.persist(cloneRetrievalState(c.state)); err != nil {
		c.persistenceFailed = true
		return errRetrievalStateSave
	}
	return nil
}

func (c *retrievalControl) hasPersistenceFailed() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persistenceFailed
}

func (c *retrievalControl) markAwaitingUserInput() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.state.AwaitingUserInput
	c.state.AwaitingUserInput = true
	if err := c.saveLocked(); err != nil {
		c.state.AwaitingUserInput = prev
		return err
	}
	return nil
}

func (c *retrievalControl) clearAwaitingUserInput() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.state.AwaitingUserInput {
		return
	}
	c.state.AwaitingUserInput = false
	_ = c.saveLocked()
}

func (c *retrievalControl) isAwaitingUserInput() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.AwaitingUserInput
}

func (c *retrievalControl) countSearchesLocked(questionID, turnPrefix string) int {
	turn, _, scoped := strings.Cut(turnPrefix, ".")
	scoped = scoped && strings.HasPrefix(turn, retrievalCallPrefix)
	count := 0
	for _, attempt := range c.state.Attempts {
		if !isBudgetedSearchTool(attempt.Tool) || attempt.Outcome == retrievalInput || attempt.Action == retrievalRecovery {
			continue
		}
		switch {
		case questionID != "" && attempt.QuestionID == questionID:
			count++
		case attempt.QuestionID == "" && scoped && strings.HasPrefix(attempt.ID, turn+"."):
			count++
		case questionID == "" && !scoped:
			count++
		}
	}
	return count
}

func (c *retrievalControl) observeWithSources(
	callID, content string,
	outcome retrievalOutcome,
	structured bool,
	observedIDs, citableIDs []string,
	failures ...retrievalFailure,
) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempt(callID)
	if a == nil {
		return nil
	}
	a.Completed = true
	a.Outcome = outcome
	a.Content = content
	a.StructuredSources = structured
	a.ObservedIDs = slices.Clone(observedIDs)
	a.CitableIDs = slices.Clone(citableIDs)
	cloned := cloneRetrievalFailures(failures)
	for i := range cloned {
		cloned[i].CallID = a.ID
	}
	a.Failures = cloned
	return c.saveLocked()
}

func (c *retrievalControl) restoreObservation(callID, content, tool string) {
	if c == nil || callID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.attempt(callID); a != nil && a.Tool == tool && retrievalScopeCovers(c.state.Scope, a.Scope) {
		a.Content = content
	}
}

func (c *retrievalControl) recordPublishedSources(callID string, structured bool, observedIDs, citableIDs []string) {
	if c == nil || callID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.attempt(callID); a != nil && structured {
		a.StructuredSources = true
		a.ObservedIDs = slices.Clone(observedIDs)
		a.CitableIDs = slices.Clone(citableIDs)
	}
}

func (c *retrievalControl) takeDecisionCorrection() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.DecisionCorrectionUsed {
		return false
	}
	c.state.DecisionCorrectionUsed = true
	return true
}

// --- Engine integration ---

// EnableRuntimeRetrieval enables deterministic runtime retrieval safeguards.
func (e *AgentEngine) EnableRuntimeRetrieval() {
	e.runtimeRetrieval = true
}

// EnableRetrievalGuard is retained for caller compatibility and delegates to
// deterministic runtime retrieval (synchronous LLM guard calls are removed).
func (e *AgentEngine) EnableRetrievalGuard() {
	e.EnableRuntimeRetrieval()
}

func (e *AgentEngine) SetRetrievalPersistence(previous json.RawMessage, save func(context.Context, json.RawMessage) error) {
	e.retrievalPrevious = slices.Clone(previous)
	e.retrievalSave = save
}

// SetRetrievalUserMessage binds the stored user message before Execute/Resume.
func (e *AgentEngine) SetRetrievalUserMessage(id, text string) {
	e.retrievalUserMessage.id = strings.TrimSpace(id)
	e.retrievalUserMessage.text = text
}

func (e *AgentEngine) recordRetrievalUserMessage() {
	if e.retrieval == nil {
		return
	}
	if e.retrievalUserMessage.id != "" {
		e.retrieval.recordUserInput("message:"+e.retrievalUserMessage.id, e.retrievalUserMessage.text)
	}
}

func (e *AgentEngine) recordRetrievalUserAnswer(call types.ToolCall) {
	if e.retrieval == nil || call.Name != agenttools.ToolAskUser || call.Result == nil || !call.Result.Success {
		return
	}
	if strings.TrimSpace(call.Result.Output) != "" {
		e.retrieval.recordUserInput("ask_user:"+e.retrievalCallReference(call.ID), call.Result.Output)
	}
}

func (e *AgentEngine) bindRetrievalPersistence(ctx context.Context) error {
	if e.retrieval == nil || e.retrievalSave == nil {
		return nil
	}
	e.retrieval.mu.Lock()
	e.retrieval.persist = func(state retrievalState) error {
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return e.retrievalSave(ctx, encoded)
	}
	err := e.retrieval.saveLocked()
	e.retrieval.mu.Unlock()
	return err
}

// initializeRetrievalContinuation restores the previous turn's persisted
// retrieval ledger (validated against the current scope) before binding and
// persisting this turn, so RetainRetrievalHistory=true keeps prior in-scope KB
// results visible across ordinary follow-up turns.
func (e *AgentEngine) initializeRetrievalContinuation(ctx context.Context) error {
	if e.retrieval == nil {
		return nil
	}
	if len(e.retrievalPrevious) > 0 {
		var saved retrievalState
		if err := json.Unmarshal(e.retrievalPrevious, &saved); err != nil {
			return err
		}
		restored, err := e.restoreSavedRetrieval(ctx, saved)
		if err != nil {
			return err
		}
		if restored != nil {
			restored.mu.Lock()
			restored.state.AwaitingUserInput = false
			if strings.TrimSpace(e.retrievalUserMessage.text) != "" {
				restored.state.Goal = e.retrievalUserMessage.text
			}
			restored.mu.Unlock()
			e.retrieval = restored
			e.recordRetrievalUserMessage()
		}
	}
	return e.bindRetrievalPersistence(ctx)
}

func (e *AgentEngine) initRetrieval(query string) {
	e.retrieval = nil
	if strings.TrimSpace(e.retrievalUserMessage.id) != "" && strings.TrimSpace(e.retrievalUserMessage.text) != "" {
		query = e.retrievalUserMessage.text
	}
	if e.toolRegistry == nil {
		return
	}
	for name := range kbToolNames {
		if _, err := e.toolRegistry.GetTool(name); err == nil {
			if len(e.config.SearchTargets) > 0 {
				e.retrieval = newRetrievalControl(query, retrievalScopeForTargets(e.config.SearchTargets))
				return
			}
			var scope []string
			for _, kb := range e.knowledgeBasesInfo {
				if kb != nil {
					scope = append(scope, "kb:"+kb.ID)
				}
			}
			for _, doc := range e.selectedDocs {
				if doc != nil {
					scope = append(scope, "doc:"+doc.KnowledgeID)
				}
			}
			slices.Sort(scope)
			encoded, _ := json.Marshal(scope)
			e.retrieval = newRetrievalControl(query, string(encoded))
			return
		}
	}
}

func retrievalTurnPrefix(turnID string) string {
	if turnID == "" {
		return ""
	}
	return retrievalCallPrefix + base64.RawURLEncoding.EncodeToString([]byte(turnID)) + "."
}

// Provider IDs are turn-local; never change the IDs replayed to the provider.
func retrievalCallReference(turnID, callID string) string {
	if turnID == "" || callID == "" {
		return callID
	}
	return retrievalTurnPrefix(turnID) + base64.RawURLEncoding.EncodeToString([]byte(callID))
}

func (e *AgentEngine) retrievalCallReference(callID string) string {
	return retrievalCallReference(e.retrievalTurnID, callID)
}

func retrievalToolArgs(args map[string]any) map[string]any {
	clean := maps.Clone(args)
	delete(clean, "retrieval")
	return clean
}

func isBudgetedSearchTool(name string) bool {
	return name == "search_knowledge" || name == "wiki_search" || name == "query_knowledge_graph"
}

const wikiParallelSearchError = "search_knowledge was not run: it was issued in the same turn as wiki_search without knowledge_base_ids or metadata. " +
	"Use the wiki_search results first (read the matching Wiki page). Search the whole library in a later turn only if the Wiki leaves the fact unsupported, " +
	"or restrict this search with knowledge_base_ids. No search allowance was used."

// wikiShadowsSearch reports whether a call is an unscoped library search that
// must not run alongside wiki_search in the same turn. A search restricted by
// knowledge base or metadata is a deliberate source choice and stays allowed.
func wikiShadowsSearch(name string, args map[string]any) bool {
	if name != agenttools.ToolSearchKnowledge && name != agenttools.LegacyToolKnowledgeSearch {
		return false
	}
	kbs, _ := args["knowledge_base_ids"].([]any)
	metadata, _ := args["metadata"].(map[string]any)
	return len(kbs) == 0 && len(metadata) == 0
}

// prepareRetrievalCall enforces deterministic runtime safeguards before any KB
// tool executes: domain KB expansion hook, observed-ID validation for
// read_document, duplicate-search blocking, per-question search budget, and
// scoped recovery reservation. Tool schemas are never mutated.
func (e *AgentEngine) prepareRetrievalCall(ctx context.Context, tc types.LLMToolCall, args map[string]any) (string, error) {
	e.retrievalGateMu.Lock()
	defer e.retrievalGateMu.Unlock()

	if tc.Function.Name == agenttools.ToolSearchKnowledge {
		e.extHooks().ExpandSearchKnowledgeBases(e, args)
	}

	c := e.retrieval
	executionArgs := retrievalToolArgs(args)
	if tc.Function.Name == agenttools.ToolReadDocument {
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
	argJSON := string(encoded)

	c.mu.Lock()
	defer c.mu.Unlock()

	qID := c.state.questionID()
	action := retrievalInitial
	if len(c.state.Attempts) > 0 {
		action = retrievalRead
		if isBudgetedSearchTool(tc.Function.Name) {
			action = retrievalSearch
		}
	}

	var recoveryCallID string
	var recoveryTargets types.SearchTargets
	var recoveryStages []string
	var previousFailed *retrievalAttempt

	for i := len(c.state.Attempts) - 1; i >= 0; i-- {
		attempt := &c.state.Attempts[i]
		if attempt.Tool != tc.Function.Name {
			continue
		}
		sameQuestionAndScope := attempt.QuestionID == qID && retrievalScopeCovers(c.state.Scope, attempt.Scope)
		// Partial search failure scoped recovery (only for the current question & scope).
		if sameQuestionAndScope && len(attempt.Failures) > 0 && validateScopedRecovery(attempt, argJSON) == nil {
			previousFailed = attempt
			break
		}
		if attempt.Arguments != argJSON {
			continue
		}
		if attempt.Outcome != retrievalSuccess {
			if sameQuestionAndScope && attempt.Outcome == retrievalTransient {
				previousFailed = attempt
			}
			break
		}
		if isBudgetedSearchTool(tc.Function.Name) && attempt.Completed && len(attempt.Failures) == 0 &&
			attempt.QuestionID == qID {
			return "", fmt.Errorf("this exact search already completed for the current user message; reuse its available evidence instead of repeating it")
		}
		break
	}

	if previousFailed != nil {
		if c.recoverySpent(previousFailed, "") {
			return "", fmt.Errorf("recovery already used for this failed retrieval; answer from available evidence or explain the limitation")
		}
		if len(previousFailed.Failures) > 0 {
			targets, err := scopedRecoveryTargets(previousFailed, argJSON)
			if err != nil {
				return "", err
			}
			recoveryTargets = targets
			recoveryScope := retrievalScopeForTargets(targets)
			for _, f := range previousFailed.Failures {
				if f.Stage == "" || f.Outcome != retrievalTransient {
					continue
				}
				if f.Stage != "search" && f.Stage != "embedding" && f.Retry == nil {
					continue
				}
				if len(f.Targets) > 0 && !retrievalScopesOverlap([]string{recoveryScope}, retrievalScopeForTargets(f.Targets)) {
					continue
				}
				if !slices.Contains(recoveryStages, f.Stage) {
					recoveryStages = append(recoveryStages, f.Stage)
				}
			}
		}
		action = retrievalRecovery
		recoveryCallID = previousFailed.ID
	} else if isBudgetedSearchTool(tc.Function.Name) {
		used := c.countSearchesLocked(qID, retrievalTurnPrefix(e.retrievalTurnID))
		if used >= maxSearchesPerQuestion {
			return "", fmt.Errorf("search budget exhausted (%d/%d searches for this question); answer from available evidence or state the limitation", used, maxSearchesPerQuestion)
		}
	}

	attemptScope := retrievalExecutionScope(c.state.Scope, tc.Function.Name, argJSON)
	if len(recoveryTargets) > 0 {
		attemptScope = retrievalScopeForTargets(recoveryTargets)
	}
	prevLen := len(c.state.Attempts)
	c.state.Attempts = append(c.state.Attempts, retrievalAttempt{
		QuestionID:      qID,
		ID:              e.retrievalCallReference(tc.ID),
		Scope:           attemptScope,
		Action:          action,
		Tool:            tc.Function.Name,
		Arguments:       argJSON,
		RecoveryCallID:  recoveryCallID,
		RecoveryTargets: recoveryTargets,
		RecoveryStages:  recoveryStages,
	})
	if err := c.saveLocked(); err != nil {
		c.state.Attempts = c.state.Attempts[:prevLen]
		return "", err
	}
	return argJSON, nil
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
	if e.questionOrigin != nil && e.questionOrigin.Document != nil && e.questionOrigin.Document.KnowledgeID == id {
		return true
	}
	if e.pageContext != nil {
		if e.pageContext.Document != nil && e.pageContext.Document.KnowledgeID == id {
			return true
		}
		if e.pageContext.Message != nil {
			for _, ref := range e.pageContext.Message.References {
				if ref.KnowledgeID == id {
					return true
				}
			}
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
			if observation.Tool == agenttools.ToolReadDocument && json.Unmarshal([]byte(observation.Arguments), &args) == nil && args.ID == id {
				return true
			}
			if attemptObservedDocument(observation, id) {
				return true
			}
		}
	}
	return false
}

func attemptObservedDocument(a retrievalAttempt, id string) bool {
	return id != "" && slices.Contains(a.ObservedIDs, id)
}

func (e *AgentEngine) retrievalStatePrompt() string {
	if e.retrieval == nil {
		return ""
	}
	e.retrieval.mu.Lock()
	used := e.retrieval.countSearchesLocked(e.retrieval.state.questionID(), retrievalTurnPrefix(e.retrievalTurnID))
	e.retrieval.mu.Unlock()
	if used < maxSearchesPerQuestion {
		return ""
	}
	return "The search budget for this question is spent. Answer from the available evidence, read an already-observed document if needed, or explain the remaining limitation." +
		e.configuredNoAnswerPrompt()
}

// configuredNoAnswerPrompt spells out a fixed no-answer response as quoted data.
func (e *AgentEngine) configuredNoAnswerPrompt() string {
	fixed := e.configuredFallback()
	if fixed == "" {
		return ""
	}
	return " The configured no-answer response is the following quoted text; when you use it, reply with exactly that text as the complete answer: " +
		strconv.Quote(fixed)
}

func (e *AgentEngine) onlyRejectedRetrievals(step types.AgentStep) bool {
	if e.retrieval == nil || len(step.ToolCalls) == 0 {
		return false
	}
	s := e.retrieval.snapshot()
	for _, call := range step.ToolCalls {
		if !kbToolNames[call.Name] || (call.Result != nil && call.Result.Success) {
			return false
		}
		if call.Result != nil && call.Result.Data[retrievalDispatchBlocked] == true {
			continue
		}
		for _, attempt := range s.Attempts {
			if attempt.ID == e.retrievalCallReference(call.ID) {
				return false
			}
		}
	}
	return true
}

func (e *AgentEngine) retrievalSnapshot() *retrievalState {
	if e.retrieval == nil {
		return nil
	}
	s := e.retrieval.snapshot()
	return &s
}

// RetrievalMissingUserFields inspects a persisted retrieval state JSON for
// unresolved user-input blockers (supporting both legacy v3/v4 needs_user gaps
// and v4 runtime AwaitingUserInput markers set by ask_user).
func RetrievalMissingUserFields(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var saved struct {
		AwaitingUserInput bool `json:"awaiting_user_input"`
		Gaps              []struct {
			Status            string   `json:"status"`
			MissingUserFields []string `json:"missing_user_fields"`
		} `json:"gaps"`
	}
	if json.Unmarshal(raw, &saved) != nil {
		return nil
	}
	var fields []string
	seen := make(map[string]bool)
	for _, g := range saved.Gaps {
		if g.Status != "needs_user" {
			continue
		}
		for _, f := range g.MissingUserFields {
			if f != "" && !seen[f] {
				seen[f] = true
				fields = append(fields, f)
			}
		}
	}
	if len(fields) == 0 && saved.AwaitingUserInput {
		return []string{"user_input"}
	}
	return fields
}

func (e *AgentEngine) retrievalCorrectionGuidance(step types.AgentStep) string {
	readOnly := true
	for _, call := range step.ToolCalls {
		readOnly = readOnly && call.Name == agenttools.ToolReadDocument
	}
	const retained = " Existing successful observations remain usable. Answer the supported question now if possible, or adjust only the rejected call once."
	if readOnly {
		return "The requested document read was not dispatched because the document ID was not observed in this turn's sources." + retained
	}
	return "The retrieval request was not dispatched. Follow its specific error (duplicate search, budget limit, or recovery rule)." + retained
}

func extractToolPublishedSources(result *types.ToolResult) (bool, []string, []string) {
	if result == nil || !result.Success {
		return false, nil, nil
	}
	var observed, citable []string
	seenObserved := make(map[string]struct{})
	seenCitable := make(map[string]struct{})
	add := func(id string, isCitable bool) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seenObserved[id]; !ok {
			seenObserved[id] = struct{}{}
			observed = append(observed, id)
		}
		if isCitable {
			if _, ok := seenCitable[id]; !ok {
				seenCitable[id] = struct{}{}
				citable = append(citable, id)
			}
		}
	}

	structured := result.PublishedSources != nil
	for _, ref := range result.PublishedSources {
		add(ref.ID, ref.Citable)
	}
	if !structured && result.Data != nil {
		if collectPublishedSourcesFromData(result.Data, add) {
			structured = true
		}
	}
	return structured, observed, citable
}

func collectPublishedSourcesFromData(data map[string]interface{}, add func(id string, citable bool)) bool {
	if len(data) == 0 {
		return false
	}
	structured := false
	addString := func(key string, citable bool) {
		if v, ok := data[key].(string); ok && strings.TrimSpace(v) != "" {
			structured = true
			add(v, citable)
		}
	}
	addStringSlice := func(key string, citable bool) {
		raw, exists := data[key]
		if !exists {
			return
		}
		structured = true
		switch vals := raw.(type) {
		case []string:
			for _, v := range vals {
				add(v, citable)
			}
		case []interface{}:
			for _, item := range vals {
				if v, ok := item.(string); ok {
					add(v, citable)
				}
			}
		}
	}
	forEachRow := func(key string, fn func(map[string]interface{})) {
		raw, exists := data[key]
		if !exists {
			return
		}
		structured = true
		switch rows := raw.(type) {
		case []map[string]interface{}:
			for _, row := range rows {
				fn(row)
			}
		case []interface{}:
			for _, item := range rows {
				if row, ok := item.(map[string]interface{}); ok {
					fn(row)
				}
			}
		}
	}

	displayType, _ := data["display_type"].(string)
	switch displayType {
	case "search_results", "grep_results", "graph_query_results", "knowledge_chunks_list", "document_info":
		structured = true
	}

	forEachRow("stages", func(stage map[string]interface{}) {
		if ok, _ := stage["success"].(bool); !ok {
			return
		}
		if stageData, ok := stage["data"].(map[string]interface{}); ok {
			collectPublishedSourcesFromData(stageData, add)
		}
	})
	forEachRow("results", func(row map[string]interface{}) {
		if id, _ := row["chunk_id"].(string); id != "" {
			add(id, true)
		}
		if id, _ := row["faq_id"].(string); id != "" {
			add(id, true)
		}
		if id, _ := row["knowledge_id"].(string); id != "" {
			add(id, true)
		}
	})
	forEachRow("chunk_results", func(row map[string]interface{}) {
		if id, _ := row["chunk_id"].(string); id != "" {
			add(id, true)
		}
		if id, _ := row["knowledge_id"].(string); id != "" {
			add(id, true)
		}
	})
	forEachRow("chunks", func(row map[string]interface{}) {
		if id, _ := row["chunk_id"].(string); id != "" {
			add(id, true)
		}
		if id, _ := row["id"].(string); id != "" {
			add(id, true)
		}
	})
	forEachRow("omitted_candidates", func(row map[string]interface{}) {
		if id, _ := row["chunk_id"].(string); id != "" {
			add(id, false)
			return
		}
		if id, _ := row["knowledge_id"].(string); id != "" {
			add(id, false)
		}
	})
	forEachRow("documents", func(row map[string]interface{}) {
		if id, _ := row["knowledge_id"].(string); id != "" {
			add(id, false)
		}
	})
	addString("knowledge_id", displayType == "knowledge_chunks_list")
	addStringSlice("source_knowledge_ids", false)
	return structured
}
