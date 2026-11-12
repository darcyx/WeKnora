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
