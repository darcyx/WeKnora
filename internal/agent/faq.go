package agent

import (
	"maps"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

func mergeFAQSearchResult(faq, documents *types.ToolResult) *types.ToolResult {
	if documents == nil {
		return faq
	}
	combined := *faq
	combined.Success = faq.Success || documents.Success
	combined.Output = faq.Output + "\n\n" + documents.Output
	combined.Error = strings.TrimSpace(faq.Error + " " + documents.Error)
	combined.RetrievalFailures = append(slices.Clone(faq.RetrievalFailures), documents.RetrievalFailures...)
	combined.PublishedSources = nil
	sourceIndexes := make(map[string]int)
	for _, stage := range []*types.ToolResult{faq, documents} {
		structured, observed, citable := extractToolPublishedSources(stage)
		if !structured {
			continue
		}
		if combined.PublishedSources == nil {
			combined.PublishedSources = []types.PublishedSourceRef{}
		}
		for _, id := range observed {
			isCitable := slices.Contains(citable, id)
			if index, ok := sourceIndexes[id]; ok {
				combined.PublishedSources[index].Citable = combined.PublishedSources[index].Citable || isCitable
				continue
			}
			sourceIndexes[id] = len(combined.PublishedSources)
			combined.PublishedSources = append(combined.PublishedSources, types.PublishedSourceRef{ID: id, Citable: isCitable})
		}
	}
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
