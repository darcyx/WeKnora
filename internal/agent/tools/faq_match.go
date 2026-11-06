package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// FAQSearchError marks an incomplete candidate set while retaining each actual
// failed scope and cause. Returned matches remain evidence, not a unique answer.
type FAQSearchError struct {
	Failures []types.RetrievalFailure
	// Candidates are retrieved rows, not proof of applicability or uniqueness.
	Candidates []*types.SearchResult
	Cause      error
	CauseStage string
}

func (e *FAQSearchError) Error() string {
	var details []string
	for _, failure := range e.Failures {
		details = append(details, fmt.Sprintf("%s %v: %v", failure.Stage, failure.KnowledgeBaseIDs, failure.Cause))
	}
	if e.Cause != nil {
		details = append(details, fmt.Sprintf("%s: %v", e.CauseStage, e.Cause))
	}
	return "FAQ search incomplete: " + strings.Join(details, "; ")
}

func (e *FAQSearchError) Unwrap() []error {
	causes := make([]error, 0, len(e.Failures)+1)
	for _, failure := range e.Failures {
		if failure.Cause != nil {
			causes = append(causes, failure.Cause)
		}
	}
	if e.Cause != nil {
		causes = append(causes, e.Cause)
	}
	return causes
}

func faqSearchError(failures []types.RetrievalFailure, candidates []*types.SearchResult, stage string, cause error) error {
	if len(failures) == 0 && cause == nil {
		return nil
	}
	return &FAQSearchError{Failures: failures, Candidates: candidates, Cause: cause, CauseStage: stage}
}

// CompletedTargets describes successful FAQ lookup/recall, not exhaustive
// coverage, successful reranking, or applicability to the user's request.
type FAQSearchResult struct {
	Matches          []FAQMatch
	Candidates       []*types.SearchResult
	RequestedTargets types.SearchTargets
	CompletedTargets types.SearchTargets
	SkippedTargets   []FAQSkippedTarget
}

// A skipped target was not queried; its reason must not be inferred from no hits.
type FAQSkippedTarget struct {
	Target *types.SearchTarget
	Reason string
}

// FormatFAQResults uses the same source identities and presentation as document retrieval.
func (t *SearchKnowledgeTool) FormatFAQResults(ctx context.Context, query string, candidates []*types.SearchResult) *types.ToolResult {
	rows := make([]*searchResultWithMeta, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate != nil {
			rows = append(rows, &searchResultWithMeta{SearchResult: candidate, SourceQuery: query,
				KnowledgeBaseID: candidate.KnowledgeBaseID, KnowledgeBaseType: types.KnowledgeBaseTypeFAQ})
		}
	}
	result, omitted := t.formatWithinBudget(ctx, rows, t.searchTargets.GetAllKnowledgeBaseIDs(), query, SearchModeSemantic)
	if omitted > 0 {
		result.Data["omitted_for_budget"] = omitted
	}
	return result
}

// FindFAQCandidates retrieves semantic FAQ candidates within the resolved scope.
// These are evidence for clarification, not sufficient for automatic direct answers.
func (t *SearchKnowledgeTool) FindFAQCandidates(ctx context.Context, query string) (result FAQSearchResult, err error) {
	defer func() { t.attachFAQRecovery(&err, query, false) }()
	result.RequestedTargets = t.searchTargets.Clone()
	if strings.TrimSpace(query) == "" || len(t.searchTargets) == 0 {
		return result, nil
	}
	if t.knowledgeBaseService == nil {
		return result, faqSearchError(nil, nil, "faq_catalog", fmt.Errorf("FAQ search is unavailable"))
	}
	kbs, err := t.knowledgeBaseService.GetKnowledgeBasesByIDsOnly(ctx, t.searchTargets.GetAllKnowledgeBaseIDs())
	if err != nil {
		return result, faqSearchError([]types.RetrievalFailure{newRetrievalFailure(t.searchTargets, "faq_catalog", err)}, nil, "", nil)
	}
	byID := make(map[string]*types.KnowledgeBase, len(kbs))
	for _, kb := range kbs {
		if kb != nil {
			byID[kb.ID] = kb
		}
	}
	var targets types.SearchTargets
	modes := make(map[string]kbSearchMode)
	typesByID := make(map[string]string)
	var faqKBs []*types.KnowledgeBase
	var failures []types.RetrievalFailure
	for _, target := range t.searchTargets {
		if target == nil || target.KnowledgeBaseID == "" {
			continue
		}
		kb, ok := byID[target.KnowledgeBaseID]
		if !ok {
			failures = append(failures, newRetrievalFailure(types.SearchTargets{target}, "faq_catalog", fmt.Errorf("knowledge base %s could not be loaded", target.KnowledgeBaseID)))
			continue
		}
		if kb.Type != types.KnowledgeBaseTypeFAQ {
			result.SkippedTargets = append(result.SkippedTargets, FAQSkippedTarget{Target: types.SearchTargets{target}.Clone()[0], Reason: "not_faq"})
			continue
		}
		if target.Type == types.SearchTargetTypeKnowledge && len(target.KnowledgeIDs) == 0 {
			result.SkippedTargets = append(result.SkippedTargets, FAQSkippedTarget{Target: types.SearchTargets{target}.Clone()[0], Reason: "empty_document_selection"})
			continue
		}
		if _, exists := modes[kb.ID]; !exists {
			faqKBs = append(faqKBs, kb)
		}
		targets = append(targets, target)
		modes[kb.ID] = kbSearchMode{mode: SearchModeSemantic}
		typesByID[kb.ID] = kb.Type
	}
	if len(targets) == 0 {
		return result, faqSearchError(failures, nil, "", nil)
	}
	topK, vectorThreshold, keywordThreshold := t.retrievalParams(8)
	rows, searchFailures, _, _, completed := t.concurrentSearchByTargets(ctx, query, SearchModeSemantic, modes, targets, faqKBs, topK, vectorThreshold, keywordThreshold, typesByID)
	result.CompletedTargets = completed
	failures = append(failures, searchFailures...)
	unique := rows[:0]
	seenRows := make(map[string]bool)
	for _, row := range rows {
		if row == nil || row.SearchResult == nil || row.ID == "" || seenRows[row.ID] {
			continue
		}
		seenRows[row.ID] = true
		unique = append(unique, row)
		candidate := *row.SearchResult
		result.Candidates = append(result.Candidates, &candidate)
	}
	rows = unique
	if len(rows) > 0 {
		if t.rerankModel == nil {
			return result, faqSearchError(failures, result.Candidates, "faq_rerank", fmt.Errorf("FAQ semantic matching requires a rerank model"))
		}
		reranked, _, err := t.rerankResultsWithPolicy(ctx, query, rows, false, true)
		if err != nil {
			failure := newRetrievalFailure(result.CompletedTargets, "faq_rerank", err)
			original := append([]*searchResultWithMeta(nil), rows...)
			failure.Retry = func(ctx context.Context) (*types.ToolResult, error) {
				recovered, _, err := t.rerankResultsWithPolicy(ctx, query, original, false, true)
				if err != nil {
					return &types.ToolResult{Output: "FAQ reranking did not complete", Error: err.Error()}, err
				}
				var candidates []*types.SearchResult
				for _, row := range recovered {
					candidates = append(candidates, row.SearchResult)
				}
				return t.FormatFAQResults(ctx, query, candidates), nil
			}
			return result, faqSearchError(append(failures, failure), result.Candidates, "faq_rerank", fmt.Errorf("FAQ reranking did not complete: %w", err))
		}
		rows = reranked
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Score > rows[j].Score })
	var matches []FAQMatch
	seen := make(map[string]bool)
	for _, row := range rows {
		if row == nil || row.SearchResult == nil || row.ID == "" || seen[row.ID] || row.ChunkType != string(types.ChunkTypeFAQ) {
			continue
		}
		meta, err := faqMetadataFromResult(row.SearchResult)
		if err != nil || meta == nil || len(meta.Answers) == 0 {
			continue
		}
		seen[row.ID] = true
		// Bound the applicability model input, not the successful retrieval result.
		// All candidates remain available for document handoff; none is auto-selected.
		if len(matches) < 8 {
			matches = append(matches, FAQMatch{Result: row.SearchResult, Metadata: meta})
		}
	}
	result.Matches = matches
	return result, faqSearchError(failures, result.Candidates, "", nil)
}

// FindExactFAQs retrieves candidates within the same scopes as knowledge search.
func (t *SearchKnowledgeTool) FindExactFAQs(ctx context.Context, query string) (result FAQSearchResult, err error) {
	defer func() { t.attachFAQRecovery(&err, query, true) }()
	result.RequestedTargets = t.searchTargets.Clone()
	if strings.TrimSpace(query) == "" || len(t.searchTargets) == 0 {
		return result, nil
	}
	if t.knowledgeBaseService == nil || t.chunkService == nil || t.chunkService.GetRepository() == nil {
		return result, faqSearchError(nil, nil, "faq_catalog", fmt.Errorf("FAQ search is unavailable"))
	}
	kbs, err := t.knowledgeBaseService.GetKnowledgeBasesByIDsOnly(ctx, t.searchTargets.GetAllKnowledgeBaseIDs())
	if err != nil {
		return result, faqSearchError([]types.RetrievalFailure{newRetrievalFailure(t.searchTargets, "faq_catalog", err)}, nil, "", nil)
	}
	faqKBs := make(map[string]*types.KnowledgeBase, len(kbs))
	for _, kb := range kbs {
		if kb != nil {
			faqKBs[kb.ID] = kb
		}
	}
	var failures []types.RetrievalFailure
	for _, target := range t.searchTargets {
		if target == nil || target.KnowledgeBaseID == "" {
			continue
		}
		kb, known := faqKBs[target.KnowledgeBaseID]
		if !known {
			failures = append(failures, newRetrievalFailure(types.SearchTargets{target}, "faq_catalog", fmt.Errorf("knowledge base %s could not be loaded", target.KnowledgeBaseID)))
			continue
		}
		if kb.Type != types.KnowledgeBaseTypeFAQ {
			result.SkippedTargets = append(result.SkippedTargets, FAQSkippedTarget{Target: types.SearchTargets{target}.Clone()[0], Reason: "not_faq"})
			continue
		}
		if target.Type == types.SearchTargetTypeKnowledge && len(target.KnowledgeIDs) == 0 {
			result.SkippedTargets = append(result.SkippedTargets, FAQSkippedTarget{Target: types.SearchTargets{target}.Clone()[0], Reason: "empty_document_selection"})
			continue
		}
		chunks, err := t.chunkService.GetRepository().FindExactFAQQuestions(ctx, target, query)
		if err != nil {
			failures = append(failures, newRetrievalFailure(types.SearchTargets{target}, "faq_exact", err))
			continue
		}
		complete := true
		for _, chunk := range chunks {
			if chunk == nil {
				continue
			}
			if chunk.IndexStatus == "processing" || chunk.IndexStatus == "failed" {
				complete = false
				failures = append(failures, newRetrievalFailure(types.SearchTargets{target}, "faq_exact", fmt.Errorf("FAQ %s is not searchable", chunk.ID)))
				continue
			}
			result.Candidates = append(result.Candidates, &types.SearchResult{
				ID: chunk.ID, ChunkType: string(chunk.ChunkType), ChunkMetadata: chunk.Metadata,
				KnowledgeID: chunk.KnowledgeID, KnowledgeBaseID: chunk.KnowledgeBaseID,
				KnowledgeTitle: kb.Name, Content: chunk.Content, ChunkIndex: chunk.ChunkIndex,
			})
		}
		if complete {
			result.CompletedTargets = append(result.CompletedTargets, types.SearchTargets{target}.Clone()...)
		}
	}
	result.Matches = MatchFAQQuestions(query, result.Candidates)
	return result, faqSearchError(failures, result.Candidates, "", nil)
}

// FAQMatch pairs a candidate's answer metadata with its retrieval source.
type FAQMatch struct {
	Result   *types.SearchResult
	Metadata *types.FAQChunkMetadata
}

// MatchFAQQuestions selects distinct exact matches from authorized candidates.
// It does not establish completeness; callers must check retrieval errors too.
func MatchFAQQuestions(query string, candidates []*types.SearchResult) []FAQMatch {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	seen := make(map[string]bool)
	var matches []FAQMatch
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == "" || seen[candidate.ID] || candidate.ChunkType != string(types.ChunkTypeFAQ) {
			continue
		}
		meta, err := faqMetadataFromResult(candidate)
		if err != nil || meta == nil || len(meta.Answers) == 0 {
			continue
		}
		exact := func(question string) bool { return strings.TrimSpace(question) == query }
		negative := false
		for _, question := range meta.NegativeQuestions {
			negative = negative || strings.EqualFold(strings.TrimSpace(question), query)
		}
		if negative {
			continue
		}
		matched := exact(meta.StandardQuestion)
		for _, question := range meta.SimilarQuestions {
			matched = matched || exact(question)
		}
		if matched {
			seen[candidate.ID] = true
			matches = append(matches, FAQMatch{Result: candidate, Metadata: meta})
		}
	}
	return matches
}
