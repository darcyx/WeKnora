package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/modelcontext"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type scopedSearchFailureService struct {
	stubKnowledgeBaseService
	failures map[string]error
}

type mixedIndexSearchService struct {
	stubKnowledgeBaseService
	rows    []*types.SearchResult
	failAll bool
}

func (s *mixedIndexSearchService) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	return []*types.KnowledgeBase{
		kbWithIndexes("ordinary", true, true, types.KnowledgeBaseTypeDocument),
		kbWithIndexes("00000000-0000-0000-0000-000000000002", false, false, types.KnowledgeBaseTypeDocument),
	}, nil
}

func (s *mixedIndexSearchService) HybridSearch(_ context.Context, id string, _ types.SearchParams) ([]*types.SearchResult, error) {
	if s.failAll || id == "bad" {
		return nil, context.DeadlineExceeded
	}
	return s.rows, nil
}

func TestSearchKnowledgeReportsSkippedWikiWhenOtherSearchFails(t *testing.T) {
	tool := NewSearchKnowledgeTool(&mixedIndexSearchService{failAll: true}, nil, nil, types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "ordinary"},
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "00000000-0000-0000-0000-000000000002"},
	}, nil, nil)
	result, err := tool.Execute(t.Context(), json.RawMessage(`{"query":"payment","mode":"keyword"}`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Output, "not evidence")
	require.Contains(t, result.Output, "knowledge bases were not searched")
	modelOutput := modelcontext.NewRegistry(true).ModelToolResultForTool(ToolSearchKnowledge, result)
	require.Contains(t, modelOutput, "wiki or graph tool")
	require.NotContains(t, modelOutput, "00000000-0000-0000-0000-000000000002")
}

func TestSearchKnowledgePreservesSkippedWikiUnderOutputBudget(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%t", fail), func(t *testing.T) {
			tool := NewSearchKnowledgeTool(&mixedIndexSearchService{failAll: fail}, nil, nil, types.SearchTargets{
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "ordinary"},
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "00000000-0000-0000-0000-000000000002"},
			}, nil, nil)
			registry := NewToolRegistry()
			registry.SetMaxToolOutputSize(100)
			registry.RegisterTool(tool)
			result, err := registry.ExecuteTool(t.Context(), ToolSearchKnowledge, json.RawMessage(`{"query":"payment","mode":"keyword"}`))
			require.NoError(t, err)
			modelOutput := modelcontext.NewRegistry(true).ModelToolResultForTool(ToolSearchKnowledge, result)
			require.Contains(t, modelOutput, "was not searched")
			require.Contains(t, modelOutput, "wiki or graph tool")
			require.NotContains(t, modelOutput, "00000000-0000-0000-0000-000000000002")
		})
	}
}

func TestSearchKnowledgeReportsSkippedWikiAlongsideActualResults(t *testing.T) {
	for _, hit := range []bool{false, true} {
		t.Run(fmt.Sprintf("hit=%t", hit), func(t *testing.T) {
			svc := &mixedIndexSearchService{}
			if hit {
				svc.rows = []*types.SearchResult{{ID: "00000000-0000-0000-0000-000000000003", KnowledgeID: "00000000-0000-0000-0000-000000000004", KnowledgeBaseID: "ordinary", Content: "Payment setup", Score: 0.9}}
			}
			tool := NewSearchKnowledgeTool(svc, nil, nil, types.SearchTargets{
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "ordinary"},
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "00000000-0000-0000-0000-000000000002"},
				{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "bad", KnowledgeIDs: []string{"doc-bad"}},
			}, nil, nil)
			result, err := tool.Execute(t.Context(), json.RawMessage(`{"query":"payment","mode":"keyword"}`))
			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, []string{"ordinary"}, result.Data["knowledge_base_ids"])
			require.Len(t, result.RetrievalFailures, 1)
			require.Contains(t, result.Output, "knowledge bases were not searched")
			modelOutput := modelcontext.NewRegistry(true).ModelToolResultForTool(ToolSearchKnowledge, result)
			require.Contains(t, modelOutput, "was not searched")
			require.Contains(t, modelOutput, "wiki or graph tool")
			if hit {
				require.Contains(t, modelOutput, "Payment setup")
				require.Contains(t, modelOutput, "partial_failure")
			} else {
				require.Contains(t, modelOutput, "in 1 knowledge base(s)")
				require.Contains(t, modelOutput, "could not be searched")
			}
		})
	}
}

func (s *scopedSearchFailureService) HybridSearch(_ context.Context, id string, _ types.SearchParams) ([]*types.SearchResult, error) {
	if err := s.failures[id]; err != nil {
		return nil, fmt.Errorf("retrieval adapter: %w", err)
	}
	return []*types.SearchResult{{ID: "chunk-" + id, KnowledgeID: "doc-" + id,
		KnowledgeBaseID: id, Content: "Use the US sandbox region.", Score: 0.9}}, nil
}

func TestSearchKnowledgePreservesPartialEvidenceAndTypedFailures(t *testing.T) {
	tool := NewSearchKnowledgeTool(&scopedSearchFailureService{failures: map[string]error{
		"bad": context.DeadlineExceeded,
	}}, nil, nil, types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "good", KnowledgeIDs: []string{"doc-good"}},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "bad", KnowledgeIDs: []string{"doc-bad"}},
	}, nil, nil)
	result, err := tool.Execute(t.Context(), json.RawMessage(`{"query":"configuration"}`))
	require.NoError(t, err)
	require.True(t, result.Success, "usable evidence survives another source's failure")
	require.Len(t, result.RetrievalFailures, 1)
	require.Equal(t, []string{"bad"}, result.RetrievalFailures[0].KnowledgeBaseIDs)
	require.ErrorIs(t, result.RetrievalFailures[0].Cause, context.DeadlineExceeded)
	registry := modelcontext.NewRegistry(true)
	modelOutput := registry.ModelToolResultForTool(ToolSearchKnowledge, result)
	require.Contains(t, registry.DecodeOutputText(modelOutput), "Use the US sandbox region.")
	require.Contains(t, modelOutput, "partial_failure")
	require.Contains(t, modelOutput, "not searched")
	serialized, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(serialized), "RetrievalFailures", "typed runtime errors are not public response fields")
}

func TestSearchKnowledgeAllFailuresAreNotEmptySuccess(t *testing.T) {
	tool := NewSearchKnowledgeTool(&scopedSearchFailureService{failures: map[string]error{
		"bad": context.DeadlineExceeded,
	}}, nil, nil, types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "bad"},
	}, nil, nil)
	result, err := tool.Execute(t.Context(), json.RawMessage(`{"query":"configuration"}`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Len(t, result.RetrievalFailures, 1)
	require.ErrorIs(t, result.RetrievalFailures[0].Cause, context.DeadlineExceeded)
	require.Contains(t, modelcontext.NewRegistry(true).ModelToolResult(result), "not evidence")
}

func TestGraphPartialFailureKeepsEmptySuccessfulSource(t *testing.T) {
	tool := NewQueryKnowledgeGraphTool(&stubKnowledgeBaseService{
		kb:  &types.KnowledgeBase{ID: "kb", ExtractConfig: &types.ExtractConfig{Nodes: []*types.GraphNode{{Name: "entity"}}}},
		err: context.DeadlineExceeded,
	})
	// The optional graph backend completes empty; the text source times out.
	result, err := tool.Execute(t.Context(), json.RawMessage(`{"query":"entity","knowledge_base_ids":["kb"]}`))
	require.NoError(t, err)
	require.True(t, result.Success, "an empty completed graph and a failed text source are partial, not total failure")
	require.Empty(t, result.Data["results"])
	require.Len(t, result.RetrievalFailures, 1)
	require.True(t, errors.Is(result.RetrievalFailures[0].Cause, context.DeadlineExceeded))
	require.Contains(t, modelcontext.NewRegistry(true).ModelToolResult(result), "could not be queried")
}
