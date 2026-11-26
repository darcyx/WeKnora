package modelcontext

import (
	"strings"
	"testing"
)

func TestWriteDocumentInfoExcludesContentAndProcessOverrides(t *testing.T) {
	var b strings.Builder
	writeDocumentInfo(&b, map[string]interface{}{
		"source": "docs/glossary.md",
		"metadata": map[string]interface{}{
			"title":             "游戏项目接入名词解释手册",
			"content":           "超长正文不应出现在 info metadata 中",
			"process_overrides": "{\"chunk_size\":1000}",
		},
	})
	got := b.String()
	if strings.Contains(got, "超长正文") || strings.Contains(got, "process_overrides") {
		t.Fatalf("writeDocumentInfo leaked excluded metadata keys: %s", got)
	}
	if !strings.Contains(got, "title: 游戏项目接入名词解释手册") {
		t.Fatalf("writeDocumentInfo dropped legitimate metadata: %s", got)
	}
}
