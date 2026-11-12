package tools

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestWriteKnowledgeMetadataHeaderDeduplicatesByKnowledge(t *testing.T) {
	t.Parallel()
	results := []*searchResultWithMeta{
		{
			SearchResult: &types.SearchResult{
				KnowledgeID: "knowledge-1", KnowledgeTitle: "Document <One>",
				KnowledgeCustomMetadata: "region: Shanghai & Suzhou",
			},
			KnowledgeBaseID: "kb-1",
		},
		{
			SearchResult: &types.SearchResult{
				KnowledgeID: "knowledge-1", KnowledgeTitle: "Document <One>",
				KnowledgeCustomMetadata: "region: Shanghai & Suzhou",
			},
			KnowledgeBaseID: "kb-1",
		},
		{
			SearchResult: &types.SearchResult{
				KnowledgeID: "knowledge-2", KnowledgeTitle: "Document Two",
			},
			KnowledgeBaseID: "kb-1",
		},
		{
			SearchResult: &types.SearchResult{
				KnowledgeID: "knowledge-3", KnowledgeTitle: "Document Three",
				KnowledgeCustomMetadata: "owner: Alice",
			},
			KnowledgeBaseID: "kb-2",
		},
	}

	var output strings.Builder
	writeKnowledgeMetadataHeader(&output, results)
	text := output.String()

	require.Equal(t, 1, strings.Count(text, `knowledge_id="knowledge-1"`))
	require.Equal(t, 1, strings.Count(text, `knowledge_id="knowledge-3"`))
	require.NotContains(t, text, `knowledge_id="knowledge-2"`)
	require.Equal(t, 2, strings.Count(text, "<metadata>"))
	require.Contains(t, text, "Document &lt;One&gt;")
	require.Contains(t, text, "Shanghai &amp; Suzhou")
}

func TestWriteKnowledgeMetadataHeaderOmitsEmptySection(t *testing.T) {
	t.Parallel()
	var output strings.Builder
	writeKnowledgeMetadataHeader(&output, []*searchResultWithMeta{{
		SearchResult: &types.SearchResult{KnowledgeID: "knowledge-1"},
	}})
	require.Empty(t, output.String())
}

func TestDocumentReferenceLinksSurviveSearchAndRead(t *testing.T) {
	knowledge := &types.Knowledge{ID: "doc", Title: "说明", Source: "https://fallback.example/doc", CustomMetadata: types.JSON(`{"source_url":"https://docs.example/original","url":"https://docs.example/secondary"}`)}
	result := (&SearchKnowledgeTool{}).formatOutput(t.Context(), []*searchResultWithMeta{{SearchResult: &types.SearchResult{
		ID: "chunk", KnowledgeID: knowledge.ID, KnowledgeTitle: knowledge.Title, KnowledgeSource: knowledge.Source, KnowledgeCustomMetadata: knowledge.CustomMetadataText(), KnowledgeCustomMetadataRaw: knowledge.CustomMetadata, Content: "正文",
	}}}, nil, "查询", "search")
	row := result.Data["results"].([]map[string]interface{})[0]
	require.Equal(t, "https://docs.example/original", row["source_url"])
	data := (&ReadDocumentTool{}).buildData(knowledge, 1, []readChunkRow{{chunk: &types.Chunk{ID: "chunk", KnowledgeID: "doc", Content: "正文"}}})
	require.Equal(t, row["source_url"], data["source_url"])
	require.Equal(t, knowledge.Source, types.DocumentReferenceURL(nil, types.JSON(`{"description":"说明\nsource_url: https://wrong.example"}`), knowledge.Source))
	require.Equal(t, "https://docs.example/secondary", types.DocumentReferenceURL(map[string]string{"url": "https://docs.example/secondary"}, nil, knowledge.Source))
}
