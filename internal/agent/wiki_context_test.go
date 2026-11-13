package agent

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
)

func TestCompactWikiHistoryKeepsEvidenceAndLatestBatch(t *testing.T) {
	page := func(kb, typ, body string) string {
		return "<wiki_page>\n<metadata>\n<knowledge_base_id>" + kb + "</knowledge_base_id>\n<link>[[login|Login]]</link>\n<type>" + typ + "</type>\n</metadata>\n<relationships>\n" + strings.Repeat("navigation ", 32) + "\n</relationships>\n<sources>source</sources>\n<summary>summary</summary>\n<content>\n" + body + "\n</content>\n</wiki_page>"
	}
	p := page("kbA", "concept", "requires init & setup <T>")
	index := page("kbA", "index", strings.Repeat("old index\n", 64))
	hit := "<search_results>\n<page>\n<knowledge_base_id>kbA</knowledge_base_id>\n<link>[[login|Login]]</link>\n<summary>browse hit</summary>\n</page>\n</search_results>"
	msg := func(name, text string) chat.Message {
		return chat.Message{Role: "tool", Name: name, ToolCallID: name, Content: text}
	}
	history := []chat.Message{
		msg("wiki_read_page", index), {Role: "user", Content: "question"}, {Role: "assistant"},
		msg("wiki_read_page", index+"\n\n"+p+"\n\n"+page("kbB", "concept", "cross library")),
		msg("wiki_search", hit), msg("read_document", p), {Role: "assistant"},
		msg("wiki_read_page", p), msg("wiki_search", hit),
	}
	got := compactWikiHistory(history)
	if history[3].Content == got[3].Content || !strings.Contains(history[3].Content, "old index") {
		t.Fatal("expected copy-on-write")
	}
	for _, i := range []int{0, 1, 2, 4, 5, 6, 7, 8} {
		if got[i].Content != history[i].Content || got[i].ToolCallID != history[i].ToolCallID {
			t.Fatalf("message %d changed", i)
		}
	}
	old := got[3].Content
	for _, s := range []string{"old index", "requires init", "navigation "} {
		if strings.Contains(old, s) {
			t.Errorf("retained %q", s)
		}
	}
	for _, s := range []string{"kbB", "cross library", "<sources>source</sources>", "<summary>summary</summary>"} {
		if !strings.Contains(old, s) {
			t.Errorf("lost %q", s)
		}
	}
}
