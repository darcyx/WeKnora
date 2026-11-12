package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
)

type retrievalFAQState struct {
	Observations    json.RawMessage     `json:"observations"`
	DocumentTargets types.SearchTargets `json:"document_targets,omitempty"`
}

func validateRetrievalFAQ(a *retrievalAttempt, gapScope string) error {
	if a.FAQOnly && (a.Tool != agenttools.ToolSearchKnowledge || !a.RuntimeManaged || a.Action != retrievalInitial && a.Action != retrievalSearch) {
		return fmt.Errorf("invalid FAQ preflight reservation")
	}
	if a.FAQ == nil {
		return nil
	}
	handoffScope := a.Scope
	if a.FAQOnly {
		// A document handoff can be outside the FAQ reservation, but never outside this fact.
		handoffScope = gapScope
	}
	if (!a.FAQOnly && a.Action != retrievalInitial) || a.Tool != agenttools.ToolSearchKnowledge || !json.Valid(a.FAQ.Observations) ||
		!retrievalScopeCovers(handoffScope, retrievalScopeForTargets(a.FAQ.DocumentTargets)) {
		return fmt.Errorf("invalid FAQ retrieval handoff")
	}
	return nil
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
