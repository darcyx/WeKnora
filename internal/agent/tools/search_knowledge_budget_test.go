package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestFormatWithinBudgetKeepsLargestRankedPrefix(t *testing.T) {
	tool := &SearchKnowledgeTool{}
	results := make([]*searchResultWithMeta, 3)
	for i, size := range []int{240, 140, 8000} {
		results[i] = &searchResultWithMeta{
			SearchResult: &types.SearchResult{
				ID: fmt.Sprintf("c%d", i), KnowledgeID: "doc", Content: strings.Repeat("文", size),
			},
			KnowledgeBaseID: "kb",
		}
	}
	ctx := WithOutputBudget(context.Background(), 2500)
	budget := OutputBudget(ctx) * 4 / 5
	firstTwo := tool.formatOutput(ctx, results[:2], []string{"kb"}, "q", SearchModeHybrid)
	if size := utf8.RuneCountInString(firstTwo.Output); size > budget {
		t.Fatalf("test fixture: the first two rows take %d runes, over budget %d", size, budget)
	}
	res, omitted := tool.formatWithinBudget(ctx, results, []string{"kb"}, "q", SearchModeHybrid)
	rows, _ := res.Data["results"].([]map[string]interface{})
	if len(rows) != 2 || omitted != 1 {
		t.Fatalf("shown=%d omitted=%d, want 2 shown and 1 omitted", len(rows), omitted)
	}
	if rows[0]["chunk_id"] != "c0" || rows[1]["chunk_id"] != "c1" {
		t.Fatalf("unexpected ranked rows: %v", rows)
	}
}

func TestFormatWithinBudgetListsOmittedDocumentsWithinBudget(t *testing.T) {
	tool := &SearchKnowledgeTool{}
	results := []*searchResultWithMeta{
		{SearchResult: &types.SearchResult{ID: "c1", KnowledgeID: "d1", KnowledgeTitle: "first", Content: strings.Repeat("x", 500)}, KnowledgeBaseID: "kb"},
		{SearchResult: &types.SearchResult{ID: "c2", KnowledgeID: "d2", KnowledgeTitle: "second", Content: strings.Repeat("secret", 2000)}, KnowledgeBaseID: "kb"},
	}
	ctx := WithOutputBudget(context.Background(), 2500)
	result, omitted := tool.formatWithinBudget(ctx, results, []string{"kb"}, "q", SearchModeHybrid)
	rows, ok := result.Data["omitted_candidates"].([]map[string]interface{})
	if omitted != 1 || !ok || len(rows) != 1 || rows[0]["knowledge_id"] != "d2" || !strings.Contains(result.Output, `title="second"`) || strings.Contains(result.Output, "secret") || utf8.RuneCountInString(result.Output) > OutputBudget(ctx)*4/5 {
		t.Fatalf("omitted=%d data=%v output=%s", omitted, rows, result.Output)
	}
}

func TestFormatWithinBudgetDoesNotDropEvidenceForLargeMetadata(t *testing.T) {
	metadata := strings.Repeat("文", 38560)
	results := []*searchResultWithMeta{
		{SearchResult: &types.SearchResult{ID: "c1", KnowledgeID: "d1", KnowledgeCustomMetadata: metadata, Content: "first"}, KnowledgeBaseID: "kb"},
		{SearchResult: &types.SearchResult{ID: "c2", KnowledgeID: "d2", Content: "specific answer"}, KnowledgeBaseID: "kb"},
	}
	ctx := WithOutputBudget(context.Background(), 3000)
	result, omitted := (&SearchKnowledgeTool{}).formatWithinBudget(ctx, results, []string{"kb"}, "q", SearchModeHybrid)
	rows := result.Data["results"].([]map[string]interface{})
	if omitted != 0 || len(rows) != 2 || rows[1]["content"] != "specific answer" {
		t.Fatalf("omitted=%d rows=%v", omitted, len(rows))
	}
	if rows[0]["knowledge_metadata"] != metadata || !strings.Contains(result.Output, `<metadata truncated="true">`) {
		t.Fatal("the model view should mark truncated metadata while preserving structured metadata")
	}
}
