package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type faqCandidateSearch struct {
	interfaces.KnowledgeBaseService
	results []*types.SearchResult
	params  types.SearchParams
}

func (*faqCandidateSearch) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	return []*types.KnowledgeBase{{ID: "kb", Type: types.KnowledgeBaseTypeFAQ}}, nil
}
func (*faqCandidateSearch) ResolveEmbeddingModelKeys(context.Context, []*types.KnowledgeBase) map[string]string {
	return nil
}
func (s *faqCandidateSearch) HybridSearch(_ context.Context, _ string, params types.SearchParams) ([]*types.SearchResult, error) {
	s.params = params
	return s.results, nil
}

func TestMatchFAQQuestions(t *testing.T) {
	row := func(id string, meta types.FAQChunkMetadata) *types.SearchResult {
		b, err := json.Marshal(meta)
		require.NoError(t, err)
		return &types.SearchResult{ID: id, ChunkType: string(types.ChunkTypeFAQ), ChunkMetadata: b}
	}
	first := row("one", types.FAQChunkMetadata{StandardQuestion: "如何申请？", SimilarQuestions: []string{"怎么申请？"}, Answers: []string{"答案"}})
	second := row("two", types.FAQChunkMetadata{StandardQuestion: "怎么申请？", Answers: []string{"另一个答案"}})
	negative := row("negative", types.FAQChunkMetadata{StandardQuestion: "怎么申请？", NegativeQuestions: []string{"怎么申请？"}, Answers: []string{"答案"}})
	require.Len(t, MatchFAQQuestions(" 怎么申请？ ", []*types.SearchResult{first, first, negative}), 1)
	require.Len(t, MatchFAQQuestions("怎么申请？", []*types.SearchResult{first, second}), 2)
	require.Empty(t, MatchFAQQuestions("怎么申请？以及费用", []*types.SearchResult{first}))
	require.Empty(t, MatchFAQQuestions("", []*types.SearchResult{first}))
}

func TestFindFAQCandidatesRespectsTargetAndKeepsDistinctFAQs(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "问题", Answers: []string{"答案"}})
	require.NoError(t, err)
	svc := &faqCandidateSearch{results: []*types.SearchResult{
		{ID: "one", ChunkType: string(types.ChunkTypeFAQ), Content: "same", ChunkMetadata: meta, KnowledgeBaseID: "kb"},
		{ID: "two", ChunkType: string(types.ChunkTypeFAQ), Content: "same", ChunkMetadata: meta, KnowledgeBaseID: "kb"},
	}}
	target := &types.SearchTarget{KnowledgeBaseID: "kb", TenantID: 1, TagIDs: []string{"tag"}}
	tool := NewSearchKnowledgeTool(svc, nil, nil, types.SearchTargets{target}, &stubReranker{scores: []float64{0.9, 0.9}}, nil)
	result, err := tool.FindFAQCandidates(context.Background(), "接入")
	require.NoError(t, err)
	require.Len(t, result.Matches, 2)
	require.Equal(t, types.SearchTargets{target}, result.CompletedTargets)
	require.Len(t, result.Candidates, 2)
	require.Equal(t, []string{"tag"}, svc.params.TagIDs)
	require.True(t, svc.params.DisableKeywordsMatch)
}

func TestFindFAQCandidatesBoundsSelectionWithoutFailingRetrieval(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "问题", Answers: []string{"答案"}})
	require.NoError(t, err)
	service := &faqCandidateSearch{}
	for i := 0; i < 9; i++ {
		service.results = append(service.results, &types.SearchResult{
			ID: fmt.Sprint(i), ChunkType: string(types.ChunkTypeFAQ), Content: "问题", ChunkMetadata: meta, KnowledgeBaseID: "kb",
		})
	}
	tool := NewSearchKnowledgeTool(service, nil, nil, types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}, &stubReranker{scores: []float64{0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9}}, nil)
	result, err := tool.FindFAQCandidates(context.Background(), "接入")
	require.NoError(t, err)
	require.Len(t, result.Matches, 8)
	require.Len(t, result.Candidates, 9, "selection bounds must not erase already retrieved evidence")
	require.Len(t, result.CompletedTargets, 1)
}

func TestFindFAQCandidatesRejectsRerankDegradation(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "问题", Answers: []string{"答案"}})
	require.NoError(t, err)
	service := &faqCandidateSearch{results: []*types.SearchResult{{ID: "one", ChunkType: string(types.ChunkTypeFAQ), ChunkMetadata: meta, Content: "问题", KnowledgeBaseID: "kb"}}}
	targets := types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}
	for _, model := range []*stubReranker{nil, {err: fmt.Errorf("unavailable")}, {scores: []float64{0.17}}} {
		var reranker rerank.Reranker
		if model != nil {
			reranker = model
		}
		tool := NewSearchKnowledgeTool(service, nil, nil, targets, reranker, nil)
		result, err := tool.FindFAQCandidates(context.Background(), "问题")
		require.Error(t, err)
		require.Empty(t, result.Matches)
		var incomplete *FAQSearchError
		require.ErrorAs(t, err, &incomplete)
		require.Len(t, incomplete.Candidates, 1, "unverified evidence must survive rejected reranking")
		require.Equal(t, "one", incomplete.Candidates[0].ID)
		require.Equal(t, targets, result.CompletedTargets)
		require.Equal(t, "faq_rerank", incomplete.CauseStage)
	}
}

func TestFAQSearchUnavailableIsNotEmptySuccess(t *testing.T) {
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
	tool := NewSearchKnowledgeTool(nil, nil, nil, targets, nil, nil)
	for _, find := range []func(context.Context, string) (FAQSearchResult, error){tool.FindExactFAQs, tool.FindFAQCandidates} {
		result, err := find(t.Context(), "问题")
		require.Error(t, err)
		require.Empty(t, result.Matches)
		var incomplete *FAQSearchError
		require.ErrorAs(t, err, &incomplete)
		result, err = find(t.Context(), "")
		require.NoError(t, err, "an empty request performs no search")
		require.Empty(t, result.Matches)
	}
}

type partialFAQSearch struct{ faqCandidateSearch }

func (s *partialFAQSearch) HybridSearch(_ context.Context, _ string, params types.SearchParams) ([]*types.SearchResult, error) {
	if len(params.KnowledgeIDs) > 0 && params.KnowledgeIDs[0] == "bad" {
		return nil, fmt.Errorf("FAQ adapter: %w", context.DeadlineExceeded)
	}
	return s.results, nil
}

type partialFAQRepository struct {
	interfaces.ChunkRepository
	chunk *types.Chunk
}

func (r *partialFAQRepository) FindExactFAQQuestions(_ context.Context, target *types.SearchTarget, _ string) ([]*types.Chunk, error) {
	if len(target.KnowledgeIDs) > 0 && target.KnowledgeIDs[0] == "bad" {
		return nil, fmt.Errorf("FAQ repository: %w", context.DeadlineExceeded)
	}
	return []*types.Chunk{r.chunk}, nil
}

type partialFAQChunks struct {
	interfaces.ChunkService
	repository interfaces.ChunkRepository
}

func (s partialFAQChunks) GetRepository() interfaces.ChunkRepository { return s.repository }

func TestFAQSearchKeepsPartialEvidenceAndTypedCause(t *testing.T) {
	metadata, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "问题", Answers: []string{"标准答案"}})
	require.NoError(t, err)
	chunk := &types.Chunk{ID: "answer", KnowledgeID: "good", KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, Metadata: metadata}
	svc := &partialFAQSearch{faqCandidateSearch: faqCandidateSearch{results: []*types.SearchResult{{
		ID: chunk.ID, KnowledgeID: chunk.KnowledgeID, KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeFAQ),
		Content: "问题", ChunkMetadata: metadata,
	}}}}
	targets := types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"bad"}, TagIDs: []string{"failed-tag"}, ScopeTagIDs: []string{"scope-tag"}},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"good"}},
	}
	tool := NewSearchKnowledgeTool(svc, nil, partialFAQChunks{repository: &partialFAQRepository{chunk: chunk}}, targets,
		&stubReranker{scores: []float64{0.9}}, nil)
	for _, tc := range []struct {
		name  string
		stage string
		find  func(context.Context, string) (FAQSearchResult, error)
	}{
		{"exact", "faq_exact", tool.FindExactFAQs},
		{"semantic", "search", tool.FindFAQCandidates},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.find(t.Context(), "问题")
			require.ErrorIs(t, err, context.DeadlineExceeded, "partial retrieval is not a complete candidate set")
			require.Len(t, result.Matches, 1, "a failed source must not discard another source's evidence")
			require.Equal(t, "answer", result.Matches[0].Result.ID)
			require.Equal(t, types.SearchTargets{targets[1]}, result.CompletedTargets)
			var incomplete *FAQSearchError
			require.ErrorAs(t, err, &incomplete)
			require.Len(t, incomplete.Failures, 1)
			failure := incomplete.Failures[0]
			require.Equal(t, []string{"kb"}, failure.KnowledgeBaseIDs)
			require.Equal(t, tc.stage, failure.Stage)
			require.ErrorIs(t, failure.Cause, context.DeadlineExceeded)
			require.Equal(t, types.SearchTargets{targets[0]}, failure.Targets)
			failure.Targets[0].TenantID = 99
			failure.Targets[0].KnowledgeIDs[0] = "changed"
			failure.Targets[0].TagIDs[0] = "changed"
			failure.Targets[0].ScopeTagIDs[0] = "changed"
			require.Equal(t, uint64(1), targets[0].TenantID)
			require.Equal(t, []string{"bad"}, targets[0].KnowledgeIDs)
			require.Equal(t, []string{"failed-tag"}, targets[0].TagIDs)
			require.Equal(t, []string{"scope-tag"}, targets[0].ScopeTagIDs)
		})
	}
}

func TestFAQSearchPartialFailureSurvivesRerankRejection(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "问题", Answers: []string{"待核对答案"}})
	require.NoError(t, err)
	svc := &partialFAQSearch{faqCandidateSearch: faqCandidateSearch{results: []*types.SearchResult{{
		ID: "candidate", KnowledgeID: "good", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeFAQ),
		Content: "问题", ChunkMetadata: meta,
	}}}}
	targets := types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"bad"}},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"good"}},
	}
	tool := NewSearchKnowledgeTool(svc, nil, nil, targets, &stubReranker{err: fmt.Errorf("rerank unavailable")}, nil)
	result, err := tool.FindFAQCandidates(t.Context(), "问题")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, result.Matches, "unverified candidates must not be returned as usable matches")
	var incomplete *FAQSearchError
	require.ErrorAs(t, err, &incomplete)
	require.ErrorContains(t, incomplete.Cause, "reranking")
	require.Len(t, incomplete.Candidates, 1)
	require.Equal(t, "candidate", incomplete.Candidates[0].ID)
	require.Len(t, incomplete.Failures, 2)
	require.Equal(t, types.SearchTargets{targets[0]}, incomplete.Failures[0].Targets)
	require.Equal(t, types.SearchTargets{targets[1]}, incomplete.Failures[1].Targets)
	require.NotNil(t, incomplete.Failures[1].Retry, "rerank recovery must reuse retained candidates")
	require.Equal(t, "faq_rerank", incomplete.CauseStage)
	require.Equal(t, types.SearchTargets{targets[1]}, result.CompletedTargets)
}

func TestFAQSearchDistinguishesFailureFromEmptyResults(t *testing.T) {
	for _, doc := range []string{"bad", "good"} {
		t.Run(doc, func(t *testing.T) {
			tool := NewSearchKnowledgeTool(&partialFAQSearch{}, nil,
				partialFAQChunks{repository: &partialFAQRepository{}},
				types.SearchTargets{{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{doc}}}, nil, nil)
			for _, find := range []func(context.Context, string) (FAQSearchResult, error){tool.FindExactFAQs, tool.FindFAQCandidates} {
				result, err := find(t.Context(), "问题")
				require.Empty(t, result.Matches)
				if doc == "bad" {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					var incomplete *FAQSearchError
					require.ErrorAs(t, err, &incomplete)
					require.Empty(t, incomplete.Candidates)
				} else {
					require.NoError(t, err, "a completed search without candidates is not a tool failure")
				}
			}
		})
	}
}
