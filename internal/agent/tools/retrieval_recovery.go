package tools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Tencent/WeKnora/internal/types"
)

func (t *SearchKnowledgeTool) attachSearchRecovery(result *types.ToolResult, query string, mode string, limit int) {
	for i := range result.RetrievalFailures {
		failure := &result.RetrievalFailures[i]
		if failure.Retry != nil || len(failure.Targets) == 0 {
			continue
		}
		child := *t
		child.searchTargets = failure.Targets.Clone()
		child.rerankModel = nil
		input, _ := json.Marshal(SearchKnowledgeInput{Query: query, Mode: mode, Limit: limit, KnowledgeBaseIDs: child.searchTargets.GetAllKnowledgeBaseIDs()})
		failure.Retry = func(ctx context.Context) (*types.ToolResult, error) {
			if meta, ok := ToolExecFromContext(ctx); ok {
				copyMeta := *meta
				copyMeta.KnowledgeSearch = nil
				copyMeta.RetrievalTargets = child.searchTargets.Clone()
				copyMeta.RetrievalRecovery = nil
				ctx = WithToolExecContext(ctx, &copyMeta)
			}
			return child.Execute(ctx, input)
		}
	}
}

func (t *SearchKnowledgeTool) attachFAQRecovery(searchErr *error, query string, exact bool) {
	var failure *FAQSearchError
	if !errors.As(*searchErr, &failure) {
		return
	}
	for i := range failure.Failures {
		item := &failure.Failures[i]
		if item.Retry != nil || len(item.Targets) == 0 {
			continue
		}
		child := *t
		child.searchTargets = item.Targets.Clone()
		item.Retry = func(ctx context.Context) (*types.ToolResult, error) {
			var recovered FAQSearchResult
			var err error
			if exact {
				recovered, err = child.FindExactFAQs(ctx, query)
			} else {
				recovered, err = child.FindFAQCandidates(ctx, query)
			}
			result := child.FormatFAQResults(ctx, query, recovered.Candidates)
			result.Success = len(recovered.CompletedTargets) > 0 || len(recovered.Candidates) > 0
			var failure *FAQSearchError
			if errors.As(err, &failure) {
				result.RetrievalFailures = failure.Failures
			}
			if !result.Success && err != nil {
				result.Error = err.Error()
				return result, err
			}
			return result, nil
		}
	}
}
