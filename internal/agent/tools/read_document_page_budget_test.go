package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/modelcontext"
)

func TestReadDocumentPageBudgetsEscapedContentAndPreservesCursor(t *testing.T) {
	tool, repo := newReadDocumentFixture(3)
	repo.ordered[0].Content = "first " + strings.Repeat("&", 2800)
	repo.ordered[1].Content = "second " + strings.Repeat("&", 2800)
	repo.ordered[2].Content = "last"
	var got []string
	for offset := 0; offset < len(repo.ordered); {
		input := json.RawMessage(fmt.Sprintf(`{"id":"doc-1","offset":%d}`, offset))
		result, err := tool.Execute(t.Context(), input)
		if err != nil || !result.Success {
			t.Fatalf("read at offset %d: result=%+v err=%v", offset, result, err)
		}
		output := modelcontext.NewRegistry(true).ModelToolResultForTool(ToolReadDocument, result)
		if size := utf8.RuneCountInString(output); size > DefaultMaxToolOutput {
			t.Fatalf("model received %d runes at offset %d, budget is %d", size, offset, DefaultMaxToolOutput)
		}
		ids := chunkIDsFromData(t, result.Data)
		got = append(got, ids...)
		offset += len(ids)
		if len(ids) == 0 {
			t.Fatal("page did not advance")
		}
		if offset < len(repo.ordered) && result.Data["next_offset"] != offset {
			t.Fatalf("next offset = %v, want %d", result.Data["next_offset"], offset)
		}
	}
	if strings.Join(got, ",") != "chunk-0,chunk-1,chunk-2" {
		t.Fatalf("unexpected pages: %v", got)
	}
}

func TestReadDocumentPageIgnoresUnrenderedImageOCRInBudget(t *testing.T) {
	tool, repo := newReadDocumentFixture(4)
	imgJSON := fmt.Sprintf(
		`[{"url":"resource://img-1","caption":"Steam API 权限截图","ocr_text":%q},{"url":"resource://img-2","caption":"Steam IP 白名单截图","ocr_text":%q}]`,
		strings.Repeat("OCR菜单文字", 300),
		strings.Repeat("OCR配置文字", 300),
	)
	for i, c := range repo.ordered {
		c.Content = fmt.Sprintf("chunk %d body: %s", i, strings.Repeat("正文", 120))
		c.ImageInfo = imgJSON
	}
	const budget = 4000
	ctx := WithOutputBudget(t.Context(), budget)
	res, err := tool.Execute(ctx, json.RawMessage(`{"id":"doc-1","offset":0,"limit":20}`))
	if err != nil || !res.Success {
		t.Fatalf("read failed: res=%+v err=%v", res, err)
	}
	ids := chunkIDsFromData(t, res.Data)
	if len(ids) != 4 {
		t.Fatalf("expected all 4 chunks in one page when OCR is unrendered, got %d (%v)", len(ids), ids)
	}
	output := modelcontext.NewRegistry(true).ModelToolResultForTool(ToolReadDocument, res)
	if size := utf8.RuneCountInString(output); size > budget {
		t.Fatalf("model output %d runes exceeded budget %d", size, budget)
	}
}
