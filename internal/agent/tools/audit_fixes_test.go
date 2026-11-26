package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestReadDocumentFitRowsAroundFocusAndXMLEscapeMetadata(t *testing.T) {
	rows := []readChunkRow{
		{chunk: &types.Chunk{ID: "c0", Content: "0000000000000000000000000000000000000000"}, role: "context_before"},
		{chunk: &types.Chunk{ID: "c1", Content: "1111111111111111111111111111111111111111"}, role: "context_before"},
		{chunk: &types.Chunk{ID: "c2", Content: "2222222222222222222222222222222222222222"}, role: "focus"},
		{chunk: &types.Chunk{ID: "c3", Content: "3333333333333333333333333333333333333333"}, role: "context_after"},
		{chunk: &types.Chunk{ID: "c4", Content: "4444444444444444444444444444444444444444"}, role: "context_after"},
	}
	budget := readDocumentRowCost(rows[2].chunk) + readDocumentRowCost(rows[1].chunk) + readDocumentRowCost(rows[3].chunk)
	fitted := fitRowsAroundFocusToBudget(rows, budget)
	require.Len(t, fitted, 3)
	require.Equal(t, "c1", fitted[0].chunk.ID)
	require.Equal(t, "c2", fitted[1].chunk.ID)
	require.Equal(t, "c3", fitted[2].chunk.ID)

	tool := &ReadDocumentTool{}
	meta := types.JSON(`{"note":"a < b & c > d"}`)
	out := tool.buildOutput(&types.Knowledge{ID: "k1", Title: "T", Metadata: meta}, 1, []readChunkRow{rows[2]}, "", "")
	require.Contains(t, out, "a &lt; b &amp; c &gt; d")
}

func TestWikiRenamePageProtectsIndexAndSummaryAndSkipsStaleInLinks(t *testing.T) {
	svc := &mutationScopeWikiService{fakeWikiPageService: &fakeWikiPageService{
		pages: map[string]*types.WikiPage{
			wikiPageKey("kb-1", "index"):          {KnowledgeBaseID: "kb-1", Slug: "index", PageType: types.WikiPageTypeIndex},
			wikiPageKey("kb-1", "summary/doc-1"):  {KnowledgeBaseID: "kb-1", Slug: "summary/doc-1", PageType: types.WikiPageTypeSummary},
			wikiPageKey("kb-1", "concept/target"): {KnowledgeBaseID: "kb-1", Slug: "concept/target", Content: "self [[concept/target]]", InLinks: types.StringArray{"concept/target", "concept/deleted-stale", "concept/live"}},
			wikiPageKey("kb-1", "concept/live"):   {KnowledgeBaseID: "kb-1", Slug: "concept/live", Content: "ref [[concept/target]]"},
		},
	}}
	tool := NewWikiRenamePageTool(svc, []string{"kb-1"})

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"slug":"index","new_slug":"concept/idx"}`))
	require.NoError(t, err)
	require.False(t, res.Success)

	res, err = tool.Execute(context.Background(), json.RawMessage(`{"slug":"summary/doc-1","new_slug":"concept/doc-1"}`))
	require.NoError(t, err)
	require.False(t, res.Success)

	res, err = tool.Execute(context.Background(), json.RawMessage(`{"slug":"concept/target","new_slug":"concept/renamed"}`))
	require.NoError(t, err)
	require.True(t, res.Success, res.Error)
	require.Equal(t, "ref [[concept/renamed]]", svc.pages[wikiPageKey("kb-1", "concept/live")].Content)
}

func TestRollbackWikiContentChangesSurvivesCancelledContext(t *testing.T) {
	var seenErr error
	svc := &cancelledCtxWikiService{onUpdate: func(ctx context.Context) error {
		seenErr = ctx.Err()
		return seenErr
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rollbackWikiContentChanges(ctx, svc, []appliedWikiContentChange{
		{page: &types.WikiPage{Slug: "concept/a", Content: "new"}, originalContent: "old"},
	})
	require.NoError(t, err)
	require.NoError(t, seenErr)
}

type cancelledCtxWikiService struct {
	fakeWikiPageService
	onUpdate func(context.Context) error
}

func (s *cancelledCtxWikiService) UpdateAutoLinkedContent(ctx context.Context, _ *types.WikiPage) error {
	if s.onUpdate != nil {
		return s.onUpdate(ctx)
	}
	return errors.New("unexpected")
}
