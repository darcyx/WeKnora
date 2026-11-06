package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// FAQResolver evaluates the existing matcher after an initial search was admitted.
// A nil resolution leaves the ordinary search path unchanged.
type FAQResolver func(context.Context, types.SearchTargets) (*FAQResolution, error)

type FAQResolution struct {
	Result          *types.ToolResult
	Observations    json.RawMessage
	DocumentTargets types.SearchTargets
}

type retrievalFAQState struct {
	Observations    json.RawMessage     `json:"observations"`
	DocumentTargets types.SearchTargets `json:"document_targets,omitempty"`
}

func (e *AgentEngine) SetFAQResolver(resolver FAQResolver) { e.faqResolver = resolver }

func validateRetrievalFAQ(a *retrievalAttempt) error {
	if a.FAQ == nil {
		return nil
	}
	if a.Action != retrievalInitial || a.Tool != agenttools.ToolSearchKnowledge || !json.Valid(a.FAQ.Observations) ||
		!retrievalScopeCovers(a.Scope, retrievalScopeForTargets(a.FAQ.DocumentTargets)) {
		return fmt.Errorf("invalid FAQ retrieval handoff")
	}
	return nil
}

func (c *retrievalControl) recordFAQ(callID string, resolution *FAQResolution) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempt(callID)
	if a == nil || a.Completed || a.FAQ != nil {
		return fmt.Errorf("FAQ requires an uncompleted initial retrieval")
	}
	next := *a
	next.FAQ = &retrievalFAQState{Observations: slices.Clone(resolution.Observations), DocumentTargets: resolution.DocumentTargets.Clone()}
	if err := validateRetrievalFAQ(&next); err != nil {
		return err
	}
	a.FAQ = next.FAQ
	return nil
}

func (e *AgentEngine) executeWithFAQ(ctx context.Context, tc types.LLMToolCall, arguments json.RawMessage) (*types.ToolResult, error) {
	execute := func(ctx context.Context) (*types.ToolResult, error) {
		tool, err := e.toolRegistry.GetTool(tc.Function.Name)
		if err != nil {
			return nil, err
		}
		if meta, ok := agenttools.ToolExecFromContext(ctx); ok {
			copyMeta := *meta
			copyMeta.KnowledgeSearch = nil
			ctx = agenttools.WithToolExecContext(ctx, &copyMeta)
		}
		return tool.Execute(ctx, arguments)
	}
	if e.faqResolver == nil || e.retrieval == nil || tc.Function.Name != agenttools.ToolSearchKnowledge {
		return execute(ctx)
	}
	var input agenttools.SearchKnowledgeInput
	if json.Unmarshal(arguments, &input) != nil || strings.TrimSpace(input.Query) == "" || len(input.Metadata) > 0 {
		return execute(ctx)
	}
	snapshot := e.retrieval.snapshot()
	var attempt *retrievalAttempt
	for i := range snapshot.Attempts {
		if snapshot.Attempts[i].ID == e.retrievalCallReference(tc.ID) {
			attempt = &snapshot.Attempts[i]
		}
	}
	if attempt == nil || attempt.Action != retrievalInitial || attempt.Completed || attempt.FAQ != nil {
		return execute(ctx)
	}
	targets := e.config.SearchTargets.Clone()
	if len(input.KnowledgeBaseIDs) > 0 {
		for _, id := range input.KnowledgeBaseIDs {
			if !slices.Contains(targets.GetAllKnowledgeBaseIDs(), id) {
				return execute(ctx)
			}
		}
		targets = slices.DeleteFunc(targets, func(t *types.SearchTarget) bool {
			return t == nil || !slices.Contains(input.KnowledgeBaseIDs, t.KnowledgeBaseID)
		})
	}
	if len(targets) == 0 || !retrievalScopeCovers(attempt.Scope, retrievalScopeForTargets(targets)) {
		return execute(ctx)
	}
	resolution, err := e.faqResolver(ctx, targets)
	if resolution == nil {
		if err != nil {
			return nil, err
		}
		return execute(ctx)
	}
	if resolution.Result == nil {
		return nil, fmt.Errorf("FAQ resolution has no observation")
	}
	if recordErr := e.retrieval.recordFAQ(attempt.ID, resolution); recordErr != nil {
		return resolution.Result, errors.Join(err, recordErr)
	}
	result := resolution.Result
	if err != nil {
		return result, err
	}
	if result.FAQ != nil && result.FAQ.Reply != "" || len(resolution.DocumentTargets) == 0 {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	meta, ok := agenttools.ToolExecFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("FAQ document handoff requires the original execution context")
	}
	copyMeta := *meta
	copyMeta.RetrievalTargets = resolution.DocumentTargets.Clone()
	input.KnowledgeBaseIDs = resolution.DocumentTargets.GetAllKnowledgeBaseIDs()
	arguments, err = json.Marshal(input)
	if err != nil {
		return result, err
	}
	documentCtx := agenttools.WithOutputBudget(ctx, max(1, agenttools.OutputBudget(ctx)/2))
	documents, err := execute(agenttools.WithToolExecContext(documentCtx, &copyMeta))
	if err != nil {
		// Retain FAQ evidence and the actual document failure together.
		if documents == nil {
			documents = &types.ToolResult{Error: err.Error(), Output: "Document retrieval did not complete: " + err.Error()}
		}
		documents.RetrievalFailures = append(documents.RetrievalFailures, types.RetrievalFailure{
			KnowledgeBaseIDs: resolution.DocumentTargets.GetAllKnowledgeBaseIDs(), Targets: resolution.DocumentTargets.Clone(), Stage: "search", Cause: err,
		})
	}
	return mergeFAQSearchResult(result, documents), nil
}

func mergeFAQSearchResult(faq, documents *types.ToolResult) *types.ToolResult {
	if documents == nil {
		return faq
	}
	combined := *faq
	combined.Success = faq.Success || documents.Success
	combined.Output = faq.Output + "\n\n" + documents.Output
	combined.Error = strings.TrimSpace(faq.Error + " " + documents.Error)
	combined.RetrievalFailures = append(slices.Clone(faq.RetrievalFailures), documents.RetrievalFailures...)
	displayType := "search_stages"
	if combined.Success {
		displayType = "search_results"
	}
	results := make([]map[string]any, 0)
	kbCounts := make(map[string]int)
	seen := make(map[string]bool)
	for _, stage := range []*types.ToolResult{faq, documents} {
		if !stage.Success {
			continue
		}
		if stage.Data["display_type"] != "search_results" {
			displayType = "search_stages"
		}
		var rows []map[string]any
		switch values := stage.Data["results"].(type) {
		case []map[string]any:
			rows = values
		case []any:
			for _, value := range values {
				if row, ok := value.(map[string]any); ok {
					rows = append(rows, row)
				}
			}
		}
		for _, row := range rows {
			if row == nil {
				continue
			}
			id, _ := row["chunk_id"].(string)
			if id == "" {
				id, _ = row["faq_id"].(string)
			}
			kb, _ := row["knowledge_base_id"].(string)
			document, _ := row["knowledge_id"].(string)
			key := kb + "\x00" + document + "\x00" + id
			if id != "" && seen[key] {
				continue
			}
			seen[key] = true
			copyRow := maps.Clone(row)
			if id != "" {
				copyRow["chunk_id"] = id
			}
			results = append(results, copyRow)
			if kb != "" {
				kbCounts[kb]++
			}
		}
	}
	// Each stage keeps its own coverage and diagnostics through history replay.
	combined.Data = map[string]any{"display_type": displayType, "results": results, "count": len(results), "kb_counts": kbCounts, "stages": []map[string]any{
		{"success": faq.Success, "output": faq.Output, "error": faq.Error, "data": faq.Data},
		{"success": documents.Success, "output": documents.Output, "error": documents.Error, "data": documents.Data},
	}}
	combined.Images = append(slices.Clone(faq.Images), documents.Images...)
	combined.ImageDescriptions = append(slices.Clone(faq.ImageDescriptions), documents.ImageDescriptions...)
	return &combined
}

func (e *AgentEngine) recordFAQDecision(callID, content string, faq *types.FAQToolResult) error {
	s := e.retrieval.snapshot()
	for _, attempt := range s.Attempts {
		if attempt.ID != callID {
			continue
		}
		review := retrievalReview{GapID: attempt.GapID, CallID: callID, Status: retrievalSatisfied, Quotes: []string{content}}
		if len(faq.MissingUserFields) > 0 {
			review.Status, review.MissingUserFields = retrievalNeedsUser, faq.MissingUserFields
		}
		return e.retrieval.applyReview(review)
	}
	return fmt.Errorf("FAQ decision has no observed retrieval")
}

func (e *AgentEngine) finishFAQReply(ctx context.Context, state *types.AgentState, step types.AgentStep, messages *[]chat.Message, sessionID, messageID string) (bool, error) {
	for _, call := range step.ToolCalls {
		if call.Result != nil && call.Result.FAQ != nil {
			state.AddUsage(call.Result.FAQ.Usage)
			state.TurnUsage.Accumulate(call.Result.FAQ.Usage)
		}
	}
	if len(step.ToolCalls) != 1 || step.ToolCalls[0].Result == nil || !step.ToolCalls[0].Result.Success {
		return false, nil
	}
	faq := step.ToolCalls[0].Result.FAQ
	if faq == nil || strings.TrimSpace(faq.Reply) == "" {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, publicText := agenttools.SplitThinkContent(faq.Reply)
	if publicText != faq.Reply || agenttools.IsProtocolArtifact(faq.Reply) {
		return false, nil
	}
	current := &retrievalControl{state: e.retrieval.snapshot()}
	attempt := current.attempt(e.retrievalCallReference(step.ToolCalls[0].ID))
	if attempt == nil {
		return false, nil
	}
	answered := current.gap(attempt.GapID)
	if answered == nil || answered.LastCallID != attempt.ID {
		return false, nil
	}
	for _, gap := range current.state.Gaps {
		if gap.ID == answered.ID && len(faq.MissingUserFields) > 0 {
			if gap.Status != retrievalNeedsUser || !slices.Equal(gap.MissingUserFields, faq.MissingUserFields) {
				return false, nil
			}
			continue
		}
		if !current.evidenceAvailable(&gap) {
			return false, nil
		}
	}
	if e.drainSteerMessages(ctx, state, messages, sessionID, messageID) > 0 {
		return false, nil
	}
	if len(faq.Clarification) > 0 {
		state.RoundSteps = append(state.RoundSteps, types.AgentStep{ToolCalls: []types.ToolCall{{ID: types.FAQClarificationToolCallID,
			Name: "faq_clarification", Args: map[string]any{"state": string(faq.Clarification)}}}})
	}
	state.KnowledgeRefs = append(state.KnowledgeRefs, faq.References...)
	if len(faq.References) > 0 {
		if err := e.eventBus.Emit(ctx, event.Event{ID: messageID + "-faq-references", Type: event.EventAgentReferences, SessionID: sessionID,
			Data: event.AgentReferencesData{References: faq.References}}); err != nil {
			return false, err
		}
	}
	if err := e.eventBus.Emit(ctx, event.Event{ID: messageID + "-faq-answer", Type: event.EventAgentFinalAnswer, SessionID: sessionID,
		Data: event.AgentFinalAnswerData{Content: faq.Reply, Done: true}}); err != nil {
		return false, err
	}
	state.FinalAnswer, state.IsComplete = faq.Reply, true
	return true, nil
}
