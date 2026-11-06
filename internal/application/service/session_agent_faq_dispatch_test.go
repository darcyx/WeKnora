package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type faqDispatchKBService struct{ faqMatchKBService }

func (faqDispatchKBService) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	return []*types.KnowledgeBase{{ID: "kb", Type: types.KnowledgeBaseTypeFAQ}, {ID: "documents", Type: types.KnowledgeBaseTypeDocument}}, nil
}

func TestAgentFAQResolverRetainsAnswerScopeAndLocalUsage(t *testing.T) {
	s, req, cfg := faqEvaluationFixture(t)
	s.knowledgeBaseService = faqDispatchKBService{}
	cfg.FAQPreflightUsage = &types.TokenUsage{TotalTokens: 101}
	documents := &types.SearchTarget{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "documents", KnowledgeIDs: []string{"selected"}, TagIDs: []string{"tag"}, TenantID: 1}
	targets := append(cfg.SearchTargets.Clone(), documents)
	resolver := s.agentFAQResolver(req, cfg, faqMatchModel{response: `{"selected_id":"faq","scope_match":"same"}`, usage: types.TokenUsage{TotalTokens: 7}}, nil, nil)
	result, err := resolver(t.Context(), targets)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Result.Success)
	require.Equal(t, "原文答案", result.Result.FAQ.Reply)
	require.Equal(t, 7, result.Result.FAQ.Usage.TotalTokens)
	require.Equal(t, 101, cfg.FAQPreflightUsage.TotalTokens, "concurrent initial facts cannot share mutable usage")
	require.Equal(t, types.SearchTargets{documents}, result.DocumentTargets)
	require.Len(t, result.Result.FAQ.References, 1)
	require.Contains(t, result.Result.Output, "问题及原始上下文", "direct answer selection must preserve raw evidence")
	var observations []map[string]any
	require.NoError(t, json.Unmarshal(result.Observations, &observations))
	require.Len(t, observations, 3, "exact, scope and publication revalidation remain observable")
	require.Contains(t, string(result.Observations), "completed_targets")
	require.NotContains(t, string(result.Observations), "documents", "FAQ cannot claim document coverage")
	documents.KnowledgeIDs[0] = "changed"
	require.Equal(t, "selected", result.DocumentTargets[0].KnowledgeIDs[0])
}

func TestAgentFAQResolverClarificationCarriesNecessaryFields(t *testing.T) {
	for _, fields := range []string{`["platform"]`, `[]`, `["platform","PLATFORM"]`, `[""]`} {
		t.Run(fields, func(t *testing.T) {
			s, req, cfg := faqEvaluationFixture(t)
			s.knowledgeBaseService = faqDispatchKBService{}
			targets := append(cfg.SearchTargets.Clone(), &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "documents", TenantID: 1})
			resolver := s.agentFAQResolver(req, cfg, faqMatchModel{response: `{"question":"您使用哪个平台？","scope_match":"missing","missing_user_fields":` + fields + `}`}, nil, nil)
			result, err := resolver(t.Context(), targets)
			require.NoError(t, err)
			require.Empty(t, result.DocumentTargets, "a missing user condition must not trigger document fallback")
			if fields == `["platform"]` {
				require.Equal(t, "您使用哪个平台？", result.Result.FAQ.Reply)
				require.Equal(t, []string{"platform"}, result.Result.FAQ.MissingUserFields)
				require.NotEmpty(t, result.Result.FAQ.Clarification)
			} else {
				require.Empty(t, result.Result.FAQ.Reply)
				require.Contains(t, result.Result.Output, "requires user clarification")
				require.NotEmpty(t, result.Result.Data["stage_failures"])
			}
		})
	}
}

func TestAgentFAQResolverDoesNotClaimUntouchedDocumentSources(t *testing.T) {
	s, req, cfg := faqEvaluationFixture(t)
	s.knowledgeBaseService = faqDispatchKBService{}
	queries := []string{}
	s.chunkService = faqMatchChunkService{repo: faqRecordingRepository{queries: &queries}}
	resolver := s.agentFAQResolver(req, cfg, faqMatchModel{}, nil, nil)
	result, err := resolver(t.Context(), types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "documents", TenantID: 1}})
	require.NoError(t, err)
	require.Nil(t, result)
	require.Empty(t, queries)
}

func TestFAQDispatchResolutionPreservesPartialFailureCandidates(t *testing.T) {
	s, _, cfg := faqEvaluationFixture(t)
	search := tools.NewSearchKnowledgeTool(s.knowledgeBaseService, nil, s.chunkService, cfg.SearchTargets, nil, s.cfg)
	candidate := &types.SearchResult{ID: "partial", KnowledgeBaseID: "kb", KnowledgeID: "doc", Content: "available evidence"}
	evaluation := &agentFAQEvaluation{Query: "问题"}
	evaluation.observeSearch("faq_semantic", "问题", tools.FAQSearchResult{
		RequestedTargets: cfg.SearchTargets.Clone(), Candidates: []*types.SearchResult{candidate},
	}, &tools.FAQSearchError{Candidates: []*types.SearchResult{candidate}, Failures: []types.RetrievalFailure{{
		KnowledgeBaseIDs: []string{"kb"}, Targets: cfg.SearchTargets.Clone(), Stage: "search", Cause: context.DeadlineExceeded,
	}}})
	result, err := faqDispatchResolution(t.Context(), search, evaluation, nil)
	require.NoError(t, err)
	require.True(t, result.Result.Success)
	require.Empty(t, result.Result.FAQ.Reply)
	require.Contains(t, result.Result.Output, "available evidence")
	require.Len(t, result.Result.RetrievalFailures, 1)
	require.Equal(t, "faq_semantic/search", result.Result.RetrievalFailures[0].Stage)
	require.ErrorIs(t, result.Result.RetrievalFailures[0].Cause, context.DeadlineExceeded)
}

func TestAgentFAQResolverCancellationPreservesCompletedWork(t *testing.T) {
	s, req, cfg := faqEvaluationFixture(t)
	s.knowledgeBaseService = faqDispatchKBService{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resolver := s.agentFAQResolver(req, cfg, faqMatchModel{response: `{"selected_id":"faq","scope_match":"same"}`,
		usage: types.TokenUsage{TotalTokens: 7}, onChat: cancel}, nil, nil)
	resolution, err := resolver(ctx, cfg.SearchTargets)
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, resolution, "cancellation after retrieval must preserve the observations")
	require.Contains(t, resolution.Result.Output, "问题及原始上下文")
	require.Equal(t, 7, resolution.Result.FAQ.Usage.TotalTokens)
	require.Empty(t, resolution.Result.FAQ.Reply)
	require.Contains(t, string(resolution.Observations), "faq_exact")
}

func TestFAQDispatchResolutionRetainsSelectedEvidenceWithinBudget(t *testing.T) {
	s, _, cfg := faqEvaluationFixture(t)
	search := tools.NewSearchKnowledgeTool(s.knowledgeBaseService, nil, s.chunkService, cfg.SearchTargets, nil, s.cfg)
	unselected := &types.SearchResult{ID: "other", KnowledgeBaseID: "kb", KnowledgeID: "doc", Content: strings.Repeat("unrelated ", 200)}
	selected := &types.SearchResult{ID: "selected", KnowledgeBaseID: "kb", KnowledgeID: "doc", Content: "verified source for the selected answer"}
	evaluation := &agentFAQEvaluation{Query: "问题", Match: &tools.FAQMatch{Result: selected,
		Metadata: &types.FAQChunkMetadata{Answers: []string{"原文答案"}}}}
	evaluation.observeSearch("faq_exact", "问题", tools.FAQSearchResult{
		Candidates: []*types.SearchResult{unselected, selected}, CompletedTargets: cfg.SearchTargets.Clone(),
	}, nil)
	resolution, err := faqDispatchResolution(tools.WithOutputBudget(t.Context(), 600), search, evaluation, nil)
	require.NoError(t, err)
	require.Equal(t, "原文答案", resolution.Result.FAQ.Reply)
	require.Contains(t, resolution.Result.Output, selected.Content, "the direct decision must not be backed only by a different candidate")
	require.Equal(t, 1, resolution.Result.Data["omitted_for_budget"])
	require.Equal(t, "other", evaluation.Observations[0].Candidates[0].ID, "formatting must not mutate the observation")
}
