package types

import (
	"strings"
	"testing"
)

func TestCustomMetadataTextExcludesContentAndProcessOverrides(t *testing.T) {
	k := &Knowledge{
		CustomMetadata: JSON(`{
			"title": "游戏项目接入名词解释手册",
			"source_path": "docs/glossary.md",
			"content": "29 个分类 · 424 个词条 — 超长正文不应进入模型 metadata 头部",
			"process_overrides": {"chunk_size": 1000}
		}`),
	}
	got := k.CustomMetadataText()
	if strings.Contains(got, "content:") || strings.Contains(got, "超长正文") {
		t.Fatalf("CustomMetadataText leaked raw content field: %q", got)
	}
	if strings.Contains(got, "process_overrides") {
		t.Fatalf("CustomMetadataText leaked process_overrides field: %q", got)
	}
	if !strings.Contains(got, "title: 游戏项目接入名词解释手册") || !strings.Contains(got, "source_path: docs/glossary.md") {
		t.Fatalf("CustomMetadataText dropped legitimate metadata fields: %q", got)
	}
}
