package tools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
)

type faqPreflightCacheKey struct{}

type faqPreflightCacheEntry struct {
	topK    int
	results []*types.SearchResult
}

type faqPreflightCache struct {
	mu      sync.Mutex
	entries map[string]faqPreflightCacheEntry
}

func WithFAQPreflightCache(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(faqPreflightCacheKey{}).(*faqPreflightCache); ok {
		return ctx
	}
	return context.WithValue(ctx, faqPreflightCacheKey{}, &faqPreflightCache{
		entries: make(map[string]faqPreflightCacheEntry),
	})
}

func faqPreflightCacheFromContext(ctx context.Context) *faqPreflightCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(faqPreflightCacheKey{}).(*faqPreflightCache)
	return cache
}

func normalizeFAQCacheQuery(query string) string {
	normalized := strings.Join(strings.Fields(strings.TrimSpace(query)), " ")
	normalized = strings.TrimRight(normalized, "?？。.!！")
	normalized = strings.TrimSpace(normalized)
	return strings.ToLower(normalized)
}

func faqTargetCacheKey(query string, target *types.SearchTarget) string {
	normalizedQuery := normalizeFAQCacheQuery(query)
	if normalizedQuery == "" || target == nil || target.KnowledgeBaseID == "" {
		return ""
	}
	knowledgeIDs := slices.Clone(target.KnowledgeIDs)
	sort.Strings(knowledgeIDs)
	tagIDs := slices.Clone(target.TagIDs)
	sort.Strings(tagIDs)
	scopeTagIDs := slices.Clone(target.ScopeTagIDs)
	sort.Strings(scopeTagIDs)
	return fmt.Sprintf("%s|%s|%d|%s|%s|%s|%s",
		normalizedQuery,
		target.Type,
		target.TenantID,
		target.KnowledgeBaseID,
		strings.Join(knowledgeIDs, ","),
		strings.Join(tagIDs, ","),
		strings.Join(scopeTagIDs, ","),
	)
}

func cloneSearchResult(r *types.SearchResult) *types.SearchResult {
	if r == nil {
		return nil
	}
	cp := *r
	if r.Metadata != nil {
		cp.Metadata = make(map[string]string, len(r.Metadata))
		for k, v := range r.Metadata {
			cp.Metadata[k] = v
		}
	}
	if r.ChunkMetadata != nil {
		cp.ChunkMetadata = slices.Clone(r.ChunkMetadata)
	}
	return &cp
}

func (c *faqPreflightCache) store(query string, completed types.SearchTargets, rows []*searchResultWithMeta, topK int) {
	if c == nil || len(completed) == 0 || topK <= 0 {
		return
	}
	kbCount := make(map[string]int, len(completed))
	for _, target := range completed {
		if target != nil && target.KnowledgeBaseID != "" {
			kbCount[target.KnowledgeBaseID]++
		}
	}
	byKB := make(map[string][]*types.SearchResult, len(kbCount))
	for _, row := range rows {
		if row == nil || row.SearchResult == nil {
			continue
		}
		kbID := row.KnowledgeBaseID
		if kbID == "" {
			kbID = row.SearchResult.KnowledgeBaseID
		}
		byKB[kbID] = append(byKB[kbID], cloneSearchResult(row.SearchResult))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, target := range completed {
		if target == nil || kbCount[target.KnowledgeBaseID] != 1 {
			continue
		}
		key := faqTargetCacheKey(query, target)
		if key == "" {
			continue
		}
		c.entries[key] = faqPreflightCacheEntry{
			topK:    topK,
			results: byKB[target.KnowledgeBaseID],
		}
	}
}

func (c *faqPreflightCache) lookup(query string, target *types.SearchTarget, minTopK int) ([]*types.SearchResult, bool) {
	if c == nil || target == nil {
		return nil, false
	}
	key := faqTargetCacheKey(query, target)
	if key == "" {
		return nil, false
	}
	c.mu.Lock()
	entry, ok := c.entries[key]
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	if minTopK > entry.topK && len(entry.results) >= entry.topK {
		return nil, false
	}
	cloned := make([]*types.SearchResult, len(entry.results))
	for i, row := range entry.results {
		cloned[i] = cloneSearchResult(row)
	}
	return cloned, true
}

// faqCandidateLimit caps FAQ retrieval candidates independently of the
// document-search topK configuration.
const faqCandidateLimit = 10

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

// A repeated title can be looked up directly. Re-read it under this call's
// filters so an edited, disabled or out-of-scope answer is never replayed.
func (t *SearchKnowledgeTool) preflightQuestionResult(ctx context.Context, targets types.SearchTargets, query string) *types.ToolResult {
	key := strings.TrimRight(query, "?？")
	for _, question := range t.preflightQuestions {
		if key == "" || strings.TrimRight(strings.TrimSpace(question), "?？") != key {
			continue
		}
		scoped := *t
		scoped.searchTargets = targets
		result, err := scoped.FindExactFAQs(ctx, question)
		if err != nil || len(result.Matches) != 1 {
			return nil
		}
		row := result.Matches[0].Result
		rows := []*searchResultWithMeta{{SearchResult: row, SourceQuery: query, KnowledgeBaseID: row.KnowledgeBaseID, KnowledgeBaseType: types.KnowledgeBaseTypeFAQ}}
		return scoped.formatOutput(ctx, rows, targets.GetAllKnowledgeBaseIDs(), query, "exact")
	}
	return nil
}

// FindFAQCandidates retrieves semantic FAQ candidates within the resolved scope.
// A candidate still needs a coverage check before its answer can be published.
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
	// FAQ answers need only a handful of items; a smaller pool keeps rerank and
	// the FAQ-coverage judge prompt small.
	topK = min(topK, faqCandidateLimit)
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
	if cache := faqPreflightCacheFromContext(ctx); cache != nil && len(completed) > 0 {
		cache.store(query, completed, rows, topK)
	}
	if len(rows) > 0 {
		if t.rerankModel == nil {
			return result, faqSearchError(failures, result.Candidates, "faq_rerank", fmt.Errorf("FAQ semantic matching requires a rerank model"))
		}
		reranked, _, err := t.rerankResultsWithKBs(ctx, query, rows, faqKBs, false, true)
		if err != nil {
			failure := newRetrievalFailure(result.CompletedTargets, "faq_rerank", err)
			original := append([]*searchResultWithMeta(nil), rows...)
			failure.Retry = func(ctx context.Context) (*types.ToolResult, error) {
				recovered, _, err := t.rerankResultsWithKBs(ctx, query, original, faqKBs, false, true)
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
		negative := matchFAQQuestionList(meta.NegativeQuestions, func(question string) bool {
			return strings.EqualFold(strings.TrimSpace(question), query)
		})
		if negative {
			continue
		}
		matched := exact(meta.StandardQuestion) || matchFAQQuestionList(meta.SimilarQuestions, exact)
		if matched {
			seen[candidate.ID] = true
			matches = append(matches, FAQMatch{Result: candidate, Metadata: meta})
		}
	}
	return matches
}

func matchFAQQuestionList(questions []string, match func(string) bool) bool {
	for _, question := range questions {
		if match(question) {
			return true
		}
		if strings.Contains(question, "、") {
			for _, part := range strings.Split(question, "、") {
				if match(part) {
					return true
				}
			}
		}
	}
	return false
}
