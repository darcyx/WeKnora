package tools

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

func (t *QueryKnowledgeGraphTool) isolatedGraphFailure(id, stage string, cause error, retry func(context.Context) ([]*types.SearchResult, []*types.GraphRelation, error)) types.RetrievalFailure {
	var targets types.SearchTargets
	for _, target := range t.searchTargets {
		if target != nil && target.KnowledgeBaseID == id {
			targets = append(targets, target)
		}
	}
	failure := newRetrievalFailure(targets, stage, cause)
	failure.KnowledgeBaseIDs = []string{id}
	if len(targets) == 0 {
		return failure
	}
	failure.Retry = func(ctx context.Context) (*types.ToolResult, error) {
		rows, relations, err := retry(ctx)
		if err != nil {
			return &types.ToolResult{Output: "The isolated graph stage did not complete", Error: err.Error()}, err
		}
		var results []map[string]any
		for _, row := range rows {
			if row != nil {
				results = append(results, map[string]any{"chunk_id": row.ID, "knowledge_id": row.KnowledgeID, "knowledge_base_id": row.KnowledgeBaseID, "title": row.KnowledgeTitle, "content": row.Content, "score": row.Score, "metadata": row.Metadata})
			}
		}
		return &types.ToolResult{Success: true, Output: "Recovered isolated " + stage + " stage", Data: map[string]any{"display_type": "graph_query_results", "results": results, "relations": relations}}, nil
	}
	return failure
}
