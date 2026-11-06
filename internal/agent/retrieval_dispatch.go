package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

const reviewRetrievalTool = "review_retrieval"
const retrievalDispatchBlocked = "retrieval_dispatch_blocked"

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

type retrievalReview struct {
	GapID             string                 `json:"gap_id,omitempty"`
	CallID            string                 `json:"call_id"`
	Status            retrievalStatus        `json:"status"`
	Quotes            []string               `json:"quotes,omitempty"`
	Evidence          []retrievalEvidence    `json:"evidence,omitempty"`
	NewFact           *retrievalFactProposal `json:"new_fact,omitempty"`
	MissingUserFields []string               `json:"missing_user_fields,omitempty"`
	UserAnswers       []retrievalUserAnswer  `json:"user_answers,omitempty"`
}

type retrievalEvidence struct {
	CallID string   `json:"call_id"`
	Quotes []string `json:"quotes"`
}

type retrievalDecision struct {
	GapID string `json:"gap_id,omitempty"`
	retrievalFactProposal
	Action         retrievalAction  `json:"action"`
	Lead           string           `json:"lead,omitempty"`
	RecoveryCallID string           `json:"recovery_call_id,omitempty"`
	Review         *retrievalReview `json:"review,omitempty"`
}

const retrievalReviewSchema = `{
 "type":"object","additionalProperties":false,
 "properties":{
  "gap_id":{"type":"string"},
  "call_id":{"type":"string","description":"Observed successful call; satisfied may reuse any currently available applicable result without executing another search"},
	"status":{"type":"string","enum":["progress","no_progress","satisfied","needs_user","exhausted_no_progress","user_conditions"]},
  "quotes":{"type":"array","items":{"type":"string"},"description":"Short supporting excerpts from that call, required for progress or satisfied; XML escaping and Markdown emphasis/code formatting may differ, but preserve wording and conditions. Cite facts relevant to the remaining question, not just new text"},
  "evidence":{"type":"array","minItems":1,"description":"For satisfied supported by multiple observations, use this instead of call_id and quotes. Each excerpt must belong to its own call.","items":{"type":"object","additionalProperties":false,"required":["call_id","quotes"],"properties":{"call_id":{"type":"string"},"quotes":{"type":"array","minItems":1,"items":{"type":"string"}}}}},
	"missing_user_fields":{"type":"array","minItems":1,"items":{"type":"string"},"description":"Required for needs_user: only necessary unknown conditions that the user must supply. A pending document prerequisite is not a user condition"},
	"user_answers":{"type":"array","minItems":1,"description":"Only with user_conditions: map genuinely supplied answers to existing missing fields. Cite a harness-listed input_id and its verbatim value. Declining, rephrasing the question or saying the value is unavailable does not satisfy a required condition. No call_id or evidence quotes","items":{
	 "type":"object","additionalProperties":false,"required":["field","input_id","quote"],
	 "properties":{"field":{"type":"string"},"input_id":{"type":"string"},"quote":{"type":"string"}}
	}}
 },"required":["status"]
}`

func (e *AgentEngine) retrievalTools(tools []chat.Tool) []chat.Tool {
	if e.retrieval == nil || e.usesRuntimeRetrieval() {
		return tools
	}
	var reviewSchema map[string]any
	_ = json.Unmarshal([]byte(retrievalReviewSchema), &reviewSchema)
	factProperties := retrievalFactProperties()
	for i := range tools {
		def := &tools[i].Function
		if !kbToolNames[def.Name] {
			continue
		}
		var schema map[string]any
		if json.Unmarshal(def.Parameters, &schema) != nil || schema == nil {
			continue
		}
		properties, _ := schema["properties"].(map[string]any)
		if properties == nil {
			properties = make(map[string]any)
			schema["properties"] = properties
		}
		properties["retrieval"] = map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"action"},
			"properties": map[string]any{
				"gap_id":           map[string]any{"type": "string", "description": "Reuse the harness-issued gap ID for the same fact and scope, across all search modes and readers"},
				"action":           map[string]any{"type": "string", "enum": []string{"initial", "read", "supplement", "recovery"}},
				"recovery_call_id": map[string]any{"type": "string", "description": "For recovery of an earlier still-needed failed source after reading useful evidence, name its original call ID. The gap's single recovery allowance still applies"},
				"lead":             map[string]any{"type": "string", "description": "Verbatim lead from the user goal or an available observed result for necessary reading, supplementary retrieval or argument correction"},
				"review":           reviewSchema,
			},
		}
		decisionProperties := properties["retrieval"].(map[string]any)["properties"].(map[string]any)
		maps.Copy(decisionProperties, factProperties)
		if e.retrievalGuard {
			distinction := maps.Clone(decisionProperties["distinct_from"].(map[string]any))
			distinction["description"] = "Optional hints distinguishing independently required answers from existing gaps. The runtime checks identity even when omitted; wording or search strategy is not a new fact"
			decisionProperties["distinct_from"] = distinction
		}
		var factKeys []string
		for name := range factProperties {
			factKeys = append(factKeys, name)
		}
		slices.Sort(factKeys)
		var factSelectors []any
		for _, name := range factKeys {
			factSelectors = append(factSelectors, map[string]any{"required": []string{name}})
		}
		properties["retrieval"].(map[string]any)["oneOf"] = []any{
			map[string]any{
				"required":   []string{"goal_quote", "fact"},
				"properties": map[string]any{"action": map[string]any{"enum": []string{"initial"}}},
				"not": map[string]any{"anyOf": []any{
					map[string]any{"required": []string{"gap_id"}},
					map[string]any{"required": []string{"review"}},
				}},
			},
			map[string]any{"required": []string{"gap_id"}, "not": map[string]any{"anyOf": factSelectors}},
		}
		if e.retrievalGuard && isBudgetedSearchTool(def.Name) {
			metadata := properties["retrieval"].(map[string]any)
			delete(metadata, "required")
			delete(metadata, "oneOf")
			def.Parameters, _ = json.Marshal(schema)
			def.Description += "\n" + questionSearchGuidance()
			continue
		}
		required, _ := schema["required"].([]any)
		schema["required"] = append(required, "retrieval")
		def.Parameters, _ = json.Marshal(schema)
		readRule := "Necessary reading requires progress and a grounded lead. "
		if e.retrievalGuard && def.Name == "read_document" {
			readRule = "For read_document, reuse gap_id with action=read. If its id was returned in the latest successful observation, the runtime may admit necessary reading without a separate progress review or lead; an explicit no_progress decision still blocks reading. "
		}
		def.Description += "\nFor a new fact, search directly with retrieval={action:initial,goal_quote:<user quote>,fact:<needed fact>}; omit gap_id so the runtime issues it. " +
			"For an existing fact, use only its harness-issued gap_id and action; omit new-fact fields. Never invent a gap_id. " +
			"Review the latest successful observation before a follow-up search. " + readRule +
			"Only new evidence relevant to the remaining fact is progress. After no progress, " +
			"at most one grounded supplement is available. Tool failures share one recovery per gap. " +
			"For partial results, answer from sufficient evidence; recover only needed failed knowledge_base_ids, preserving the query and other arguments. The runtime restricts recovery to the recorded failed targets; do not invent document or tag selectors. " +
			"Use action=recovery with a grounded lead for that missing fact; a supplement must select a scope independent of the failure. " +
			"Reuse gap_id; changing query, mode, library or reader does not reset allowances. " +
			"Use review_retrieval for a necessary blocker or stop decision without executing another retrieval; satisfied is optional when answering."
	}
	terminalSchema := maps.Clone(reviewSchema)
	terminalSchema["required"] = []string{"status"}
	terminalProperties := maps.Clone(reviewSchema["properties"].(map[string]any))
	terminalProperties["status"] = map[string]any{"type": "string", "enum": []string{
		"progress", "no_progress", "satisfied", "needs_user", "exhausted_no_progress", "user_conditions",
	}}
	terminalProperties["new_fact"] = map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"goal_quote", "fact"},
		"properties":  factProperties,
		"description": "Register a necessary fact without searching: only with needs_user or satisfied. Omit gap_id; existing facts must reuse their ID",
	}
	terminalSchema["properties"] = terminalProperties
	terminalSchema["oneOf"] = []any{
		map[string]any{"required": []string{"gap_id"}, "not": map[string]any{"required": []string{"new_fact"}}},
		map[string]any{
			"required":   []string{"new_fact"},
			"properties": map[string]any{"status": map[string]any{"enum": []string{"needs_user", "satisfied"}}},
			"not":        map[string]any{"required": []string{"gap_id"}},
		},
	}
	parameters, _ := json.Marshal(terminalSchema)
	return append(tools, chat.Tool{Type: "function", Function: chat.FunctionDef{
		Name: reviewRetrievalTool, Parameters: parameters,
		Description: "Record a retrieval decision without searching. Use gap_id for an existing fact. new_fact is only for a justified needs_user or an already evidenced satisfied decision, never to register a pending search. " +
			"For needs_user, provide missing_user_fields and ask directly; no call_id is needed when business rules already establish the missing conditions. " +
			"After an actual user answer, use user_conditions with user_answers to resolve only the answered missing fields. " +
			"Unanswered fields stay blocked. Completing the fields permits reassessment; it does not satisfy the fact or reset any allowance. " +
			"For satisfied, cite call_id and quotes from one applicable successful observation, or evidence entries with separate call_id and quotes for multiple observations; evidence from another fact is allowed when applicable. " +
			"Do not search merely to register a clarification or duplicate evidence. Waiting for another document fact is not needs_user. " +
			"Use exhausted_no_progress when there is no grounded next lead. This updates state only; it does not verify semantic relevance for you. " +
			"Do not narrate these internal decisions in the answer.",
	}})
}

func retrievalFactProperties() map[string]any {
	return map[string]any{
		"goal_quote":         map[string]any{"type": "string", "description": "Exact quote linking the fact to the user's task; independent facts may share a quote"},
		"goal_input_id":      map[string]any{"type": "string", "description": "Available user input ID when goal_quote comes from steering or an accepted answer during this execution"},
		"fact":               map[string]any{"type": "string", "description": "The fact needed to answer that goal; renaming an existing fact does not create a new gap"},
		"conditions":         map[string]any{"type": "string", "description": "Known applicability conditions that change this fact's answer, such as a platform or version. Query, reader and library selection are not new conditions"},
		"condition_quote":    map[string]any{"type": "string", "description": "Verbatim supporting user text or text within the cited prerequisite source. May be omitted when conditions itself is verbatim supported text; the runtime verifies and records it"},
		"condition_input_id": map[string]any{"type": "string", "description": "Available user input ID when condition_quote comes from a later message or answer"},
		"scope_from":         map[string]any{"type": "string", "description": "Only when the runtime source scope actually changed and this same fact needs the newly available range: original gap ID. Preserve fact and conditions, explain the scope difference in distinct_from. Narrowing, returning, query changes and already covered sources never grant new allowances"},
		"distinct_from": map[string]any{"type": "array", "description": "For every new fact, distinguish it from each existing gap by its independently required answer, not wording or search strategy", "items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"gap_id", "reason"},
			"properties": map[string]any{"gap_id": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string"}},
		}},
		"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Existing prerequisite gap IDs; this fact cannot execute until their evidence is satisfied and available"},
		"source": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"call_id", "quote"},
			"description": "For a prerequisite discovered in a tool result: cite an available successful observation of the parent gap. The parent waits for this fact; closed gaps cannot be reopened",
			"properties":  map[string]any{"call_id": map[string]any{"type": "string"}, "quote": map[string]any{"type": "string"}},
		},
	}
}

func (e *AgentEngine) prepareRetrievalCall(ctx context.Context, tc types.LLMToolCall, args map[string]any) (string, error) {
	e.retrievalGateMu.Lock()
	defer e.retrievalGateMu.Unlock()
	if err := e.resolveProjectConditions(ctx); err != nil {
		return "", err
	}
	if e.usesRuntimeRetrievalTool(tc.Function.Name) {
		if raw, ok := args["retrieval"]; ok {
			encoded, _ := json.Marshal(raw)
			var decision retrievalDecision
			if json.Unmarshal(encoded, &decision) == nil && (decision.Review != nil || decision.Source != nil || decision.ScopeFrom != "" || len(decision.DependsOn) > 0) {
				return e.prepareStructuredRetrievalCall(ctx, tc, args)
			}
		}
		return e.prepareRuntimeRetrieval(ctx, tc, args)
	}
	if e.retrievalGuard && isBudgetedSearchTool(tc.Function.Name) {
		return e.prepareBudgetedSearch(ctx, tc, args)
	}
	return e.prepareStructuredRetrievalCall(ctx, tc, args)
}

func (e *AgentEngine) prepareStructuredRetrievalCall(ctx context.Context, tc types.LLMToolCall, args map[string]any) (string, error) {
	raw, ok := args["retrieval"]
	if !ok {
		// Tolerate omitted metadata: bind to the current question's fact; acquire still enforces budgets.
		defaulted, err := e.defaultRetrievalDecision(ctx)
		if err != nil {
			return "", err
		}
		if tc.Function.Name == "read_document" && defaulted.Action == retrievalSearch {
			defaulted.Action = retrievalRead
			defaulted.Lead, _ = args["id"].(string)
		}
		raw = defaulted
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	var decision retrievalDecision
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return "", fmt.Errorf("invalid retrieval metadata: %w", err)
	}
	decision.RecoveryCallID = e.resolveRetrievalReference(decision.RecoveryCallID)
	if decision.Source != nil {
		decision.Source.CallID = e.resolveRetrievalReference(decision.Source.CallID)
	}
	if review := decision.Review; review != nil && review.NewFact != nil {
		return "", fmt.Errorf("inline review cannot register another fact")
	}
	if decision.GapID == "" {
		if decision.Review != nil {
			return "", fmt.Errorf("inline review requires an existing gap_id; for a new initial search omit review and gap_id")
		}
		if decision.Action != retrievalInitial {
			return "", fmt.Errorf("follow-up retrieval must reuse a registered gap_id")
		}
		decision.GapID, err = e.admitRetrievalFact(ctx, decision.retrievalFactProposal)
		if err != nil {
			return "", err
		}
	} else {
		e.retrieval.mu.Lock()
		known := e.retrieval.gap(decision.GapID) != nil
		e.retrieval.mu.Unlock()
		if !known {
			return "", fmt.Errorf("unknown gap_id %q; for a new fact, correct this search once with action=initial, goal_quote and fact, omitting gap_id; the runtime issues the ID. review_retrieval cannot register a pending search", decision.GapID)
		}
		if decision.GoalQuote != "" || decision.GoalInputID != "" || decision.Fact != "" || decision.Conditions != "" || decision.ConditionQuote != "" || decision.ConditionInputID != "" || len(decision.DistinctFrom) != 0 || len(decision.DependsOn) != 0 || decision.Source != nil || decision.ScopeFrom != "" {
			return "", fmt.Errorf("an existing gap cannot be renamed through call metadata; reuse its gap_id and omit new-fact fields")
		}
	}
	if review := decision.Review; review != nil {
		if review.GapID != "" && review.GapID != decision.GapID {
			return "", fmt.Errorf("review must refer to the current gap")
		}
		decisionReview := *review
		decisionReview.GapID = decision.GapID
		e.scopeRetrievalReview(&decisionReview)
		if err := e.applyRetrievalReview(ctx, decisionReview); err != nil {
			return "", err
		}
	}
	readAdmitted := false
	if decision.Action == retrievalRead {
		readAdmitted, err = e.validateRetrievalRead(ctx, &decision, tc.Function.Name, retrievalToolArgs(args))
		if err != nil {
			return "", err
		}
	}
	executionArgs := retrievalToolArgs(args)
	encoded, err = json.Marshal(executionArgs)
	if err != nil {
		return "", err
	}
	if err := e.retrieval.acquireWithAdmission(decision.GapID, e.retrievalCallReference(tc.ID), decision.Action, strings.TrimSpace(decision.Lead), tc.Function.Name, string(encoded), retrievalExecutionAdmission{ObservedRead: readAdmitted}, decision.RecoveryCallID); err != nil {
		return "", err
	}
	return string(encoded), nil
}

func retrievalToolArgs(args map[string]any) map[string]any {
	clean := maps.Clone(args)
	delete(clean, "retrieval")
	return clean
}

func (e *AgentEngine) reviewRetrieval(ctx context.Context, raw string) *types.ToolResult {
	e.retrievalGateMu.Lock()
	defer e.retrievalGateMu.Unlock()
	var review retrievalReview
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&review)
	if err == nil {
		e.scopeRetrievalReview(&review)
		err = e.applyRetrievalReview(ctx, review)
	}
	if err != nil {
		return &types.ToolResult{Success: false, Error: "Retrieval decision rejected: " + err.Error()}
	}
	return &types.ToolResult{Success: true, Output: "Retrieval state updated; reassess the remaining fact using the preserved state and allowances. Use available evidence or ask only the necessary unanswered fields."}
}

func retrievalCallOutcome(ctx context.Context, result *types.ToolResult, err error) retrievalOutcome {
	if ctx.Err() != nil {
		return retrievalUnknown
	}
	if err == nil && result != nil && result.Success {
		return retrievalSuccess
	}
	if result != nil && result.InvalidArguments {
		return retrievalInput
	}
	return retrievalFailureOutcome(err)
}

func retrievalFailureOutcome(err error) retrievalOutcome {
	// An aggregate timeout plus an unknown/permission failure is not safely retryable.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return retrievalUnknown
		}
		for _, cause := range causes {
			if retrievalFailureOutcome(cause) != retrievalTransient {
				return retrievalUnknown
			}
		}
		return retrievalTransient
	}
	if cause := errors.Unwrap(err); cause != nil {
		return retrievalFailureOutcome(cause)
	}
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
		return retrievalTransient
	}
	return retrievalUnknown
}

func retrievalCallFailures(ctx context.Context, result *types.ToolResult) []retrievalFailure {
	if result == nil {
		return nil
	}
	failures := make([]retrievalFailure, 0, len(result.RetrievalFailures))
	for _, failure := range result.RetrievalFailures {
		outcome := retrievalUnknown
		if ctx.Err() == nil {
			outcome = retrievalFailureOutcome(failure.Cause)
		}
		ids := slices.Clone(failure.KnowledgeBaseIDs)
		slices.Sort(ids)
		failures = append(failures, retrievalFailure{KnowledgeBaseIDs: slices.Compact(ids), Outcome: outcome,
			Targets: failure.Targets.Clone(), Stage: failure.Stage, Retry: failure.Retry, RetryAvailable: failure.Retry != nil})
	}
	return failures
}

func (e *AgentEngine) retrievalStatePrompt() string {
	if e.retrieval == nil || e.usesRuntimeRetrieval() {
		return ""
	}
	s := e.retrieval.snapshot()
	type gapView struct {
		retrievalGap
		SearchUsed        int                `json:"search_used,omitempty"`
		Failures          []retrievalFailure `json:"failed_sources,omitempty"`
		EvidenceAvailable bool               `json:"evidence_available"`
	}
	views := make([]gapView, 0, len(s.Gaps))
	viewState := &retrievalControl{state: s}
	for _, gap := range s.Gaps {
		available := viewState.evidenceAvailable(&gap)
		if !retrievalScopeCovers(s.Scope, gap.Scope) {
			gap.Fact = "Previous source scope; details unavailable"
			gap.Conditions = ""
			gap.GoalQuote = ""
			gap.DistinctFrom = nil
		}
		gap.Scope = fmt.Sprintf("scope-%d", slices.Index(s.Scopes, gap.Scope)+1)
		gap.Evidence = nil
		gap.EvidenceSources = nil
		gap.Origins = nil
		gap.ConditionQuote = ""
		view := gapView{retrievalGap: gap, EvidenceAvailable: available, SearchUsed: viewState.searchCount(&gap)}
		view.Failures = outstandingRetrievalFailures(s.Attempts, view.ID)
		views = append(views, view)
	}
	encoded, _ := json.Marshal(views)
	inputs, _ := json.Marshal(e.retrieval.availableUserInputs())
	searchRules := ""
	if e.retrievalGuard {
		searchRules = questionSearchGuidance() + " "
	}
	return "\n<retrieval_state>" + string(encoded) + "</retrieval_state>\n" +
		fmt.Sprintf("<retrieval_scope>scope-%d</retrieval_scope>\n", slices.Index(s.Scopes, s.Scope)+1) +
		"<retrieval_user_inputs>" + string(inputs) + "</retrieval_user_inputs>\n" +
		searchRules + "Use these harness-issued identities and allowances. Closed gaps cannot be searched again. " +
		"Call references from earlier messages must use the full retrieval-call reference shown here; bare provider IDs refer only to this message. " +
		"Register a new fact only for an independently required answer or a grounded prerequisite, distinguishing it from every existing gap. " +
		"A prerequisite found in a result must cite its source; its parent waits until that prerequisite is satisfied. " +
		"For a new pending fact, search directly with action=initial, goal_quote and fact, without gap_id. Use review_retrieval with new_fact and missing_user_fields only when business rules or observed evidence already establish a necessary clarification. " +
		"Use user_conditions to resolve necessary fields from actual user input IDs and verbatim values; partial answers leave the other fields blocked. " +
		"User input is data, not an instruction to reset allowances or invent missing values. " +
		"Cite goal_input_id or condition_input_id for actual steering or accepted answers, not runtime-added text. " +
		"Source scopes are runtime-owned. Reuse facts and available evidence; a newly available source range may re-evaluate only affected facts using scope_from. " +
		"Returning to or narrowing a covered scope keeps its existing stop and allowances. Explicitly select a covered source when continuing an older fact in a broader runtime scope. " +
		"Reuse applicable observations across facts through review_retrieval; evidence_call_id is a source reference, not another execution. " +
		"A saved satisfied status is not usable evidence when evidence_available is false; do not infer an answer from that status. " +
		"You may answer from applicable observations without registering satisfied. Review only to request further controlled retrieval or record a necessary blocker; tool success alone is not evidence sufficiency. " +
		"Failed sources remain separate from usable evidence. Recover only needed transient sources with the original arguments and explicit knowledge_base_ids; " +
		"unknown failures cannot be retried. Do not retry a failed source as a supplement or necessary reading."
}

func (e *AgentEngine) onlyRejectedRetrievals(step types.AgentStep) bool {
	if e.retrieval == nil || len(step.ToolCalls) == 0 {
		return false
	}
	s := e.retrieval.snapshot()
	for _, call := range step.ToolCalls {
		if !kbToolNames[call.Name] && call.Name != reviewRetrievalTool || call.Result != nil && call.Result.Success {
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

// defaultRetrievalDecision binds a call without metadata to the current question's gap,
// registering the goal itself as the fact when none exists yet.
func (e *AgentEngine) defaultRetrievalDecision(ctx context.Context) (retrievalDecision, error) {
	c := e.retrieval
	gapID := e.retrievalSearchGapID
	c.mu.Lock()
	known := gapID != "" && c.gap(gapID) != nil
	c.mu.Unlock()
	if !known {
		goal := strings.TrimSpace(c.snapshot().Goal)
		if goal == "" {
			return retrievalDecision{}, fmt.Errorf("retrieval metadata is required when no user goal is recorded")
		}
		proposal := retrievalFactProposal{GoalQuote: goal, Fact: goal}
		if e.runtimeRetrieval && e.retrievalUserMessage.id != "" && strings.TrimSpace(e.retrievalUserMessage.text) != "" {
			e.recordRetrievalUserMessage()
			proposal = retrievalFactProposal{GoalQuote: e.retrievalUserMessage.text, Fact: e.retrievalUserMessage.text,
				GoalInputID: "message:" + e.retrievalUserMessage.id}
		}
		id, err := e.admitRetrievalFact(ctx, proposal)
		if err != nil {
			return retrievalDecision{}, err
		}
		gapID = id
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	action := retrievalInitial
	if previous := c.attempt(c.gap(gapID).LastCallID); previous != nil {
		action = retrievalSearch
		if previous.Outcome != retrievalSuccess {
			action = retrievalRecovery
		}
	}
	return retrievalDecision{GapID: gapID, Action: action}, nil
}

// A rejected decision is not a failed search and must not trigger source recovery.
func (e *AgentEngine) retrievalCorrectionGuidance(step types.AgentStep) string {
	reviewOnly := true
	readOnly := true
	for _, call := range step.ToolCalls {
		reviewOnly = reviewOnly && call.Name == reviewRetrievalTool
		readOnly = readOnly && call.Name == "read_document"
	}
	const retained = " Existing successful observations remain usable. A rejected call does not establish missing user information and does not justify another search. Answer the supported question now if possible. Otherwise correct only the reported decision once; preserve all stop and recovery limits."
	if reviewOnly {
		return "The evidence review was rejected; no retrieval was attempted. Associate each quote with the observation that actually contains it; use separate evidence entries for multiple sources. Do not search again to repair a citation. A satisfied review is not required just to answer." + retained
	}
	if readOnly {
		return "The requested reading was not dispatched. Check whether its full answer or source range is already present; reuse that evidence. Only an observed, still-needed unread source/range warrants a corrected read." + retained
	}
	return "The retrieval request was not dispatched. Follow its specific argument, necessary-user-information or execution-limit error; do not invent a new gap or reset the question budget." + retained
}
