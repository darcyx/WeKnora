package modelcontext

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestSkippedSearchNotesAreBounded(t *testing.T) {
	skipped := make([]map[string]interface{}, 100)
	for i := range skipped {
		skipped[i] = map[string]interface{}{
			"knowledge_base_id": fmt.Sprintf("00000000-0000-0000-0000-%012d", i),
			"reason":            "no chunk index; use a wiki or graph tool",
		}
	}
	result := &types.ToolResult{Success: true, Output: "No matches.", Data: map[string]interface{}{
		"display_type": "search_results", "skipped_knowledge_bases": skipped,
	}}
	got := NewRegistry(true).ModelToolResultForTool("search_knowledge", result)
	if strings.Count(got, "Knowledge base kb://") != 3 || !strings.Contains(got, "97 additional knowledge bases were not searched") {
		t.Fatalf("skipped summary = %q", got)
	}
}

func TestAnnotateSearchNotes(t *testing.T) {
	t.Parallel()
	base := "<retrieval mode=\"hybrid\">\n</retrieval>"
	got := annotateSearchNotes(base, map[string]interface{}{
		"omitted_for_budget": 4,
		"partial_failures":   []string{"[kb-2]: vector store unavailable"},
	})
	if !strings.Contains(got, `<omitted count="4" reason="output_budget">`) ||
		!strings.Contains(got, "<partial_failure>[kb-2]: vector store unavailable") ||
		!strings.HasSuffix(got, "</retrieval>") {
		t.Fatalf("annotated = %q", got)
	}
	if annotateSearchNotes(base, map[string]interface{}{}) != base {
		t.Fatal("nothing to add must leave the output unchanged")
	}
}

func TestModelSearchOutputRetainsFAQPreflightRejection(t *testing.T) {
	result := &types.ToolResult{Success: true, Data: map[string]interface{}{
		"display_type": "search_results", "mode": "exact", "preflight_rejection_reason": "requires live status <query>",
		"results": []map[string]interface{}{{"faq_id": "faq-1", "faq_question": "状态？", "faq_answers": []string{"示例"}}},
	}}
	got := NewRegistry(true).ModelToolResultForTool("search_knowledge", result)
	if !strings.Contains(got, "requires live status &lt;query&gt;") || !strings.Contains(got, "preflight_review") {
		t.Fatalf("model result lost FAQ constraint: %s", got)
	}
}

// The subset note is conditional: only a counting or exhaustive-list question
// has to report the scope of the view. It must also carry the counts it was
// given, so the model reads the real shown / candidate numbers rather than a
// placeholder or a knowledge-base total.
func TestAnnotateSearchNotesReportsTruncatedRetrieval(t *testing.T) {
	t.Parallel()
	base := "<retrieval mode=\"hybrid\">\n</retrieval>"
	got := annotateSearchNotes(base, map[string]interface{}{
		"retrieval_shown":      5,
		"retrieval_candidates": 146,
	})
	want := `  <subset shown="5" candidates="146">If the question asks for a count or an exhaustive list, ` +
		`state that the provided context contains only the top 5 of 146 candidate passages at this ` +
		`filtering stage, and do not treat these passage counts as the requested total; otherwise ` +
		`answer normally without adding a disclaimer solely because of this truncation.</subset>`
	if !strings.Contains(got, want) {
		t.Fatalf("annotated = %q\nwant subset %q", got, want)
	}
	if strings.Contains(got, "say the answer may be incomplete") {
		t.Fatalf("the unconditional disclaimer must be gone: %q", got)
	}
	if !strings.HasSuffix(got, "</retrieval>") {
		t.Fatalf("subset note must stay inside the retrieval block: %q", got)
	}
	// A prompt that saw every candidate, or was given no usable counts,
	// carries no caveat at all.
	for name, data := range map[string]map[string]interface{}{
		"complete":         {"retrieval_shown": 5, "retrieval_candidates": 5},
		"no counts":        {},
		"shown only":       {"retrieval_shown": 5},
		"candidates only":  {"retrieval_candidates": 146},
		"more than pool":   {"retrieval_shown": 7, "retrieval_candidates": 5},
		"no shown passage": {"retrieval_shown": 0, "retrieval_candidates": 146},
	} {
		if annotateSearchNotes(base, data) != base {
			t.Fatalf("%s: a non-truncated retrieval must leave the output unchanged", name)
		}
	}
}

func TestAnnotateGraphResult(t *testing.T) {
	t.Parallel()
	base := "<retrieval mode=\"graph\">\n</retrieval>"
	got := annotateGraphResult(base, map[string]interface{}{
		"relations": []map[string]interface{}{{"source": "Kubernetes", "type": "orchestrates", "target": "Docker"}},
		"errors":    []string{"KB kb://2: graph extraction not configured"},
	})
	for _, want := range []string{
		`<relation source="Kubernetes" type="orchestrates" target="Docker" />`,
		"<error>KB kb://2: graph extraction not configured</error>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if annotateGraphResult(base, map[string]interface{}{}) != base {
		t.Fatal("nothing to add must leave the output unchanged")
	}
}

func TestMatchAddsToContent(t *testing.T) {
	t.Parallel()
	content := "Install the engine, then configure psionic drive parameters."
	if matchAddsToContent("... configure psionic drive ...", content) {
		t.Fatal("an excerpt of the content adds nothing")
	}
	if !matchAddsToContent("How do I tune the drive?", content) {
		t.Fatal("a matched question absent from the content adds information")
	}
	if !matchAddsToContent("anything", "") {
		t.Fatal("a snippet-only row keeps its snippet")
	}
}

func TestOmittedCandidatesExposeDocumentHandlesWithoutContent(t *testing.T) {
	r := newSourceRegistry()
	result := &types.ToolResult{Success: true, Output: "unused", Data: map[string]interface{}{
		"display_type": "search_results", "mode": "hybrid", "results": []map[string]interface{}{{"chunk_id": "11111111-1111-1111-1111-111111111111", "knowledge_id": "22222222-2222-2222-2222-222222222222", "content": "known"}},
		"omitted_for_budget": 1, "omitted_candidates": []map[string]interface{}{{"knowledge_id": "33333333-3333-3333-3333-333333333333", "knowledge_base_id": "44444444-4444-4444-4444-444444444444", "title": "next"}},
	}}
	out := r.ModelOutput(result)
	if !strings.Contains(out, `view="candidate_only"`) || !strings.Contains(out, `title="next"`) || !strings.Contains(out, `id="doc://2"`) || strings.Contains(out, `id="33333333-3333-3333-3333-333333333333"`) {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestOmittedFAQCandidatesExposeNavigationChunkHandleWithoutCitationEligibility(t *testing.T) {
	r := NewRegistry(true)
	const faqChunkID = "11c49bd9-2885-4363-a082-2ec269f903ca"
	result := &types.ToolResult{Success: true, Output: "unused", Data: map[string]interface{}{
		"display_type": "search_results",
		"mode":         "hybrid",
		"results": []map[string]interface{}{
			{"chunk_id": "11111111-1111-1111-1111-111111111111", "knowledge_id": "22222222-2222-2222-2222-222222222222", "knowledge_base_id": "55555555-5555-5555-5555-555555555555", "content": "known"},
		},
		"omitted_for_budget": 1,
		"omitted_candidates": []map[string]interface{}{
			{
				"chunk_id":          faqChunkID,
				"knowledge_id":      "c3a54bac-7396-4345-85cc-c403522bc9c0",
				"knowledge_base_id": "a3c7aaf1-de37-44a6-a444-bd9591d48a3f",
				"chunk_type":        "faq",
				"title":             "充值不到账有哪些常见原因，应该如何排查？",
			},
		},
	}}
	out := r.ModelToolResultForTool("search_knowledge", result)
	if !strings.Contains(out, `<chunk id="chunk://2" kb="kb://2" title="充值不到账有哪些常见原因，应该如何排查？" type="faq" view="candidate_only" />`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if strings.Contains(out, faqChunkID) {
		t.Fatalf("raw FAQ chunk ID leaked into model output: %s", out)
	}
	calls := []types.LLMToolCall{{
		Function: types.FunctionCall{Name: "read_document", Arguments: `{"id":"chunk://2"}`},
	}}
	r.DecodeToolCalls(calls)
	if len(calls[0].UnresolvedHandles) != 0 || !strings.Contains(calls[0].Function.Arguments, faqChunkID) {
		t.Fatalf("candidate_only chunk handle must resolve for read_document: args=%s unresolved=%v", calls[0].Function.Arguments, calls[0].UnresolvedHandles)
	}
	if got := r.DecodeOutputText(`<ref id="chunk://2"/>`); got != "" {
		t.Fatalf("unread candidate_only FAQ must not be citable, got %q", got)
	}
}
