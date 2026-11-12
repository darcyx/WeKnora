package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type faqCoverageRepository struct {
	interfaces.ChunkRepository
	calls  []types.SearchTargets
	chunks []*types.Chunk
}

func (r *faqCoverageRepository) FindExactFAQQuestions(_ context.Context, target *types.SearchTarget, _ string) ([]*types.Chunk, error) {
	r.calls = append(r.calls, types.SearchTargets{target}.Clone())
	return r.chunks, nil
}

func TestFAQExactEmptyDocumentScopeDoesNotSearch(t *testing.T) {
	repo := &faqCoverageRepository{}
	tool := NewSearchKnowledgeTool(&faqCandidateSearch{}, nil, partialFAQChunks{repository: repo},
		types.SearchTargets{{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1}}, nil, nil)
	result, err := tool.FindExactFAQs(t.Context(), "问题")
	require.NoError(t, err)
	require.Empty(t, result.Matches)
	require.Empty(t, result.CompletedTargets)
	require.Equal(t, []FAQSkippedTarget{{Target: result.RequestedTargets[0], Reason: "empty_document_selection"}}, result.SkippedTargets)
	require.Empty(t, repo.calls, "an empty document selection must not become a database query with no document filter")
}

type faqCoverageService struct {
	faqCandidateSearch
	kbs          []*types.KnowledgeBase
	catalogErr   error
	embeddingErr error
	panicStage   string
	mu           sync.Mutex
	calls        []types.SearchParams
}

func (s *faqCoverageService) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	return s.kbs, s.catalogErr
}

func (s *faqCoverageService) ResolveEmbeddingModelKeys(context.Context, []*types.KnowledgeBase) map[string]string {
	if s.embeddingErr != nil || s.panicStage == "embedding" {
		return map[string]string{"kb": "model"}
	}
	return nil
}

func (s *faqCoverageService) GetQueryEmbedding(context.Context, string, string) ([]float32, error) {
	if s.panicStage == "embedding" {
		panic("embedding failed")
	}
	return nil, s.embeddingErr
}

func (s *faqCoverageService) HybridSearch(_ context.Context, kbID string, params types.SearchParams) ([]*types.SearchResult, error) {
	s.mu.Lock()
	if len(params.KnowledgeBaseIDs) == 0 {
		params.KnowledgeBaseIDs = []string{kbID} // Single-target searches pass the KB as a positional argument.
	}
	s.calls = append(s.calls, params)
	s.mu.Unlock()
	if s.panicStage == "search" {
		panic("search failed")
	}
	return s.results, nil
}

func TestFAQCoverageSeparatesCompletedSkippedAndMissingTargets(t *testing.T) {
	for _, semantic := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "semantic"}[semantic], func(t *testing.T) {
			targets := types.SearchTargets{
				{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"doc"}, TagIDs: []string{"tag"}, ScopeTagIDs: []string{"scope"}},
				{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1},
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "documents", TenantID: 1},
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "missing", TenantID: 1},
			}
			svc := &faqCoverageService{kbs: []*types.KnowledgeBase{{ID: "kb", Type: types.KnowledgeBaseTypeFAQ}, {ID: "documents", Type: types.KnowledgeBaseTypeDocument}}}
			repo := &faqCoverageRepository{}
			tool := NewSearchKnowledgeTool(svc, nil, partialFAQChunks{repository: repo}, targets, nil, nil)
			find := tool.FindExactFAQs
			if semantic {
				find = tool.FindFAQCandidates
			}
			result, err := find(t.Context(), "问题")
			var incomplete *FAQSearchError
			require.ErrorAs(t, err, &incomplete)
			require.Equal(t, targets, result.RequestedTargets)
			require.Equal(t, targets[:1], result.CompletedTargets, "empty successful retrieval still completed its actual target")
			require.Equal(t, []FAQSkippedTarget{
				{Target: targets[1], Reason: "empty_document_selection"},
				{Target: targets[2], Reason: "not_faq"},
			}, result.SkippedTargets)
			require.Len(t, incomplete.Failures, 1)
			require.Equal(t, "faq_catalog", incomplete.Failures[0].Stage)
			require.Equal(t, targets[3:], incomplete.Failures[0].Targets)
			require.Empty(t, result.Matches)
			if semantic {
				require.Len(t, svc.calls, 1)
				require.Equal(t, []string{"doc"}, svc.calls[0].KnowledgeIDs)
				require.Equal(t, []string{"tag"}, svc.calls[0].TagIDs)
				require.Equal(t, []string{"scope"}, svc.calls[0].ScopeTagIDs)
			} else {
				require.Equal(t, []types.SearchTargets{targets[:1]}, repo.calls)
			}
			result.CompletedTargets[0].KnowledgeIDs[0] = "changed"
			result.CompletedTargets[0].TagIDs[0] = "changed"
			result.CompletedTargets[0].ScopeTagIDs[0] = "changed"
			result.SkippedTargets[0].Target.KnowledgeBaseID = "changed"
			require.Equal(t, "doc", targets[0].KnowledgeIDs[0])
			require.Equal(t, "tag", targets[0].TagIDs[0])
			require.Equal(t, "scope", targets[0].ScopeTagIDs[0])
			require.Equal(t, "kb", targets[1].KnowledgeBaseID)
			require.Equal(t, targets, result.RequestedTargets, "requested and actual coverage must not alias")
		})
	}
}

func TestFAQCoverageCatalogFailureDoesNotClaimCompletion(t *testing.T) {
	svc := &faqCoverageService{catalogErr: context.DeadlineExceeded}
	repo := &faqCoverageRepository{}
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
	tool := NewSearchKnowledgeTool(svc, nil, partialFAQChunks{repository: repo}, targets, nil, nil)
	for _, find := range []func(context.Context, string) (FAQSearchResult, error){tool.FindExactFAQs, tool.FindFAQCandidates} {
		result, err := find(t.Context(), "问题")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, targets, result.RequestedTargets)
		require.Empty(t, result.CompletedTargets)
		require.Empty(t, result.SkippedTargets, "unavailable metadata is not a known non-FAQ target")
		var incomplete *FAQSearchError
		require.ErrorAs(t, err, &incomplete)
		require.Equal(t, targets, incomplete.Failures[0].Targets)
	}
	require.Empty(t, svc.calls)
	require.Empty(t, repo.calls)
}

func TestFAQCoverageCombinedEmptySearchCompletesAllExecutedTargets(t *testing.T) {
	svc := &faqCoverageService{kbs: []*types.KnowledgeBase{{ID: "a", Type: types.KnowledgeBaseTypeFAQ}, {ID: "b", Type: types.KnowledgeBaseTypeFAQ}}}
	targets := types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "a", TenantID: 1},
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "b", TenantID: 1},
	}
	tool := NewSearchKnowledgeTool(svc, nil, nil, targets, nil, nil)
	result, err := tool.FindFAQCandidates(t.Context(), "问题")
	require.NoError(t, err)
	require.ElementsMatch(t, targets, result.CompletedTargets)
	require.Empty(t, result.Matches)
	require.Len(t, svc.calls, 1)
	require.ElementsMatch(t, []string{"a", "b"}, svc.calls[0].KnowledgeBaseIDs)
}

func TestFAQCoverageFailedExecutionHasNoCompletedTargets(t *testing.T) {
	for _, stage := range []string{"embedding error", "embedding", "search"} {
		t.Run(stage, func(t *testing.T) {
			svc := &faqCoverageService{kbs: []*types.KnowledgeBase{{ID: "kb", Type: types.KnowledgeBaseTypeFAQ}}, panicStage: stage}
			if stage == "embedding error" {
				svc.embeddingErr = context.DeadlineExceeded
			}
			targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
			tool := NewSearchKnowledgeTool(svc, nil, nil, targets, nil, nil)
			result, err := tool.FindFAQCandidates(t.Context(), "问题")
			require.Error(t, err)
			require.Empty(t, result.CompletedTargets)
			require.Empty(t, result.SkippedTargets)
			var incomplete *FAQSearchError
			require.ErrorAs(t, err, &incomplete)
			require.Len(t, incomplete.Failures, 1)
			require.Equal(t, targets, incomplete.Failures[0].Targets)
			if stage == "search" {
				require.Equal(t, "search", incomplete.Failures[0].Stage)
				require.Len(t, svc.calls, 1)
			} else {
				require.Equal(t, "embedding", incomplete.Failures[0].Stage)
				require.Empty(t, svc.calls)
			}
		})
	}
}

func TestFAQCoverageExactUnsearchableChunkKeepsPartialEvidence(t *testing.T) {
	meta := []byte(`{"standard_question":"问题","answers":["答案"]}`)
	repo := &faqCoverageRepository{chunks: []*types.Chunk{
		{ID: "ready", KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, Metadata: meta},
		{ID: "pending", KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, Metadata: meta, IndexStatus: "processing"},
	}}
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
	tool := NewSearchKnowledgeTool(&faqCandidateSearch{}, nil, partialFAQChunks{repository: repo}, targets, nil, nil)
	result, err := tool.FindExactFAQs(t.Context(), "问题")
	require.Error(t, err)
	require.Empty(t, result.CompletedTargets, "an unavailable part must not be counted as a complete exact match set")
	require.Len(t, result.Matches, 1)
	require.Equal(t, "ready", result.Matches[0].Result.ID)
	var incomplete *FAQSearchError
	require.ErrorAs(t, err, &incomplete)
	require.Equal(t, targets, incomplete.Failures[0].Targets)
	require.Equal(t, "faq_exact", incomplete.Failures[0].Stage)
}

func TestFAQCoverageRetainsRawCandidatesBeforeRerank(t *testing.T) {
	row := &types.SearchResult{ID: "faq", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeFAQ),
		Content: "问题", Score: 0.8, ChunkMetadata: []byte(`{"standard_question":"问题","answers":["答案"]}`)}
	svc := &faqCandidateSearch{results: []*types.SearchResult{row}}
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
	tool := NewSearchKnowledgeTool(svc, nil, nil, targets, &stubReranker{scores: []float64{0.95}}, nil)
	result, err := tool.FindFAQCandidates(t.Context(), "问题")
	require.NoError(t, err)
	require.Len(t, result.Matches, 1)
	require.Len(t, result.Candidates, 1)
	require.Equal(t, 0.8, result.Candidates[0].Score, "reranking must not replace the recall observation")
	require.NotSame(t, result.Candidates[0], result.Matches[0].Result)
	require.Equal(t, targets, result.CompletedTargets)
}

// A FAQ miss must not remove FAQ sources from the turn's scope: the agent
// still needs them for related questions and for its own follow-up
// sub-questions, and the retrieved FAQ content is evidence like any other.
