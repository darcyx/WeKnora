package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/modelcontext"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestFAQRejectionIsIncludedInOrdinarySearchOutput(t *testing.T) {
	tool := (&SearchKnowledgeTool{}).WithFAQPreflightRejections(map[string]types.FAQPreflightRejection{"faq-1": {Question: "充值失败如何排查？", Reason: "缺少适用条件"}})
	results := []*searchResultWithMeta{{SearchResult: &types.SearchResult{
		ID: "faq-1", KnowledgeID: "doc-1", ChunkType: "faq", Content: "排查步骤",
		ChunkMetadata: types.JSON(`{"standard_question":"充值失败如何排查？","answers":["排查步骤"]}`),
	}, KnowledgeBaseID: "kb", KnowledgeBaseType: types.KnowledgeBaseTypeFAQ}}
	result := tool.formatOutput(context.Background(), results, []string{"kb"}, "另一种查询", SearchModeHybrid)
	output := modelcontext.NewRegistry(true).ModelToolResultForTool(ToolSearchKnowledge, result)
	if !strings.Contains(output, "缺少适用条件") || !strings.Contains(output, "充值失败如何排查？") {
		t.Fatalf("missing preflight review in model output: %s", output)
	}
}

func TestFAQRejectionDoesNotLeakToSameQuestionInAnotherKB(t *testing.T) {
	// KB A's FAQ was rejected; KB B has an FAQ with the same standard question.
	tool := (&SearchKnowledgeTool{}).WithFAQPreflightRejections(map[string]types.FAQPreflightRejection{"faq-a": {Question: "充值失败如何排查？", Reason: "仅适用于 A 产品"}})
	results := []*searchResultWithMeta{{SearchResult: &types.SearchResult{
		ID: "faq-b", KnowledgeID: "doc-b", ChunkType: "faq", Content: "B 的排查步骤",
		ChunkMetadata: types.JSON(`{"standard_question":"充值失败如何排查？","answers":["B 的排查步骤"]}`),
	}, KnowledgeBaseID: "kb-b", KnowledgeBaseType: types.KnowledgeBaseTypeFAQ}}
	result := tool.formatOutput(context.Background(), results, []string{"kb-b"}, "充值失败", SearchModeHybrid)
	if _, ok := result.Data["preflight_rejection_reason"]; ok {
		t.Fatalf("KB A's rejection attached to KB B's FAQ: %v", result.Data["preflight_rejection_reason"])
	}
	output := modelcontext.NewRegistry(true).ModelToolResultForTool(ToolSearchKnowledge, result)
	if strings.Contains(output, "仅适用于 A 产品") {
		t.Fatalf("KB A's rejection leaked into KB B's output: %s", output)
	}
}

func TestSeenResultsAreDeprioritizedBeforeMMRLimit(t *testing.T) {
	rows := []*searchResultWithMeta{
		{SearchResult: &types.SearchResult{ID: "old-1", Score: 0.99, Content: "first old content"}},
		{SearchResult: &types.SearchResult{ID: "old-2", Score: 0.98, Content: "second old content"}},
		{SearchResult: &types.SearchResult{ID: "new", Score: 0.7, Content: "new relevant evidence"}},
	}
	selected := selectMMRWithSeen(context.Background(), rows, 2, map[string]bool{"old-1": true, "old-2": true})
	if len(selected) != 2 || selected[0].ID != "new" || rows[0].Score != 0.99 {
		t.Fatalf("selection=%v, original score=%v", selected, rows[0].Score)
	}
}

func TestSearchExecuteIncludesUnseenResultBeforeLimit(t *testing.T) {
	svc := &stubKnowledgeBaseService{results: []*types.SearchResult{
		{ID: "old-1", KnowledgeID: "d1", KnowledgeBaseID: "kb", Score: 0.99, Content: "first old content"},
		{ID: "old-2", KnowledgeID: "d2", KnowledgeBaseID: "kb", Score: 0.98, Content: "second old content"},
		{ID: "new", KnowledgeID: "d3", KnowledgeBaseID: "kb", Score: 0.7, Content: "new relevant evidence"},
	}}
	tool := NewSearchKnowledgeTool(svc, nil, nil, types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}, nil, nil)
	ctx := WithToolExecContext(context.Background(), &ToolExecContext{SeenChunkIDs: map[string]bool{"old-1": true, "old-2": true}})
	result, err := tool.Execute(ctx, json.RawMessage(`{"query":"content","limit":2}`))
	if err != nil || result == nil || !result.Success {
		t.Fatalf("search failed: result=%+v err=%v", result, err)
	}
	rows := result.Data["results"].([]map[string]interface{})
	if len(rows) != 2 || rows[0]["chunk_id"] != "new" {
		t.Fatalf("unseen result did not enter the limited search results: %v", rows)
	}
	if len(result.PublishedSources) == 0 || result.PublishedSources[0].ID != "new" || !result.PublishedSources[0].Citable {
		t.Fatalf("published sources = %+v", result.PublishedSources)
	}
}

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

func TestFormatWithinBudgetListsOmittedFAQsWithStandardQuestion(t *testing.T) {
	tool := &SearchKnowledgeTool{}
	results := []*searchResultWithMeta{
		{
			SearchResult:    &types.SearchResult{ID: "c1", KnowledgeID: "d1", KnowledgeTitle: "Wiki", Content: strings.Repeat("文", 900)},
			KnowledgeBaseID: "kb-doc",
		},
		{
			SearchResult: &types.SearchResult{
				ID:             "faq-1",
				KnowledgeID:    "faq-doc",
				KnowledgeTitle: "faq",
				ChunkType:      string(types.ChunkTypeFAQ),
				Content:        strings.Repeat("答", 1200),
				ChunkMetadata:  types.JSON(`{"standard_question":"充值不到账有哪些常见原因，应该如何排查？","answers":["分阶段排查"]}`),
			},
			KnowledgeBaseID:   "kb-faq",
			KnowledgeBaseType: types.KnowledgeBaseTypeFAQ,
		},
		{
			SearchResult: &types.SearchResult{
				ID:             "faq-2",
				KnowledgeID:    "faq-doc",
				KnowledgeTitle: "faq",
				ChunkType:      string(types.ChunkTypeFAQ),
				Content:        strings.Repeat("答", 1200),
				ChunkMetadata:  types.JSON(`{"standard_question":"支付问题怎么判断故障阶段？","answers":["看链路"]}`),
			},
			KnowledgeBaseID:   "kb-faq",
			KnowledgeBaseType: types.KnowledgeBaseTypeFAQ,
		},
	}
	ctx := WithOutputBudget(context.Background(), 2500)
	result, omitted := tool.formatWithinBudget(ctx, results, []string{"kb-doc", "kb-faq"}, "支付未到账", SearchModeHybrid)
	rows, ok := result.Data["omitted_candidates"].([]map[string]interface{})
	if omitted != 2 || !ok || len(rows) != 2 {
		t.Fatalf("omitted=%d rows=%v", omitted, rows)
	}
	if rows[0]["chunk_id"] != "faq-1" || rows[0]["chunk_type"] != "faq" || rows[0]["title"] != "充值不到账有哪些常见原因，应该如何排查？" {
		t.Fatalf("unexpected first omitted FAQ: %v", rows[0])
	}
	if rows[1]["chunk_id"] != "faq-2" || rows[1]["title"] != "支付问题怎么判断故障阶段？" {
		t.Fatalf("unexpected second omitted FAQ: %v", rows[1])
	}
	if utf8.RuneCountInString(result.Output) > OutputBudget(ctx)*4/5 {
		t.Fatalf("output exceeded budget: %d > %d", utf8.RuneCountInString(result.Output), OutputBudget(ctx)*4/5)
	}
}

func TestDeprioritizeSeenResultsGivesBudgetPriorityToUnseenChunks(t *testing.T) {
	results := []*searchResultWithMeta{
		{SearchResult: &types.SearchResult{ID: "seen-1", KnowledgeID: "d1", Content: strings.Repeat("旧", 900)}, KnowledgeBaseID: "kb"},
		{SearchResult: &types.SearchResult{ID: "new-1", KnowledgeID: "d2", Content: strings.Repeat("新", 400)}, KnowledgeBaseID: "kb"},
		{SearchResult: &types.SearchResult{ID: "new-2", KnowledgeID: "d3", Content: strings.Repeat("新", 400)}, KnowledgeBaseID: "kb"},
	}
	ordered := deprioritizeSeenResults(results, map[string]bool{"seen-1": true})
	if ordered[0].ID != "new-1" || ordered[1].ID != "new-2" || ordered[2].ID != "seen-1" {
		t.Fatalf("unexpected order: %s, %s, %s", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}

	allSeen := deprioritizeSeenResults(results, map[string]bool{"seen-1": true, "new-1": true, "new-2": true})
	if allSeen[0].ID != "seen-1" || allSeen[1].ID != "new-1" || allSeen[2].ID != "new-2" {
		t.Fatalf("all-seen results must keep original order: %s, %s, %s", allSeen[0].ID, allSeen[1].ID, allSeen[2].ID)
	}
}
