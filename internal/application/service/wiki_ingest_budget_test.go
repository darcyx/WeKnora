package service

import (
	"context"
	"errors"
	"testing"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type wikiBudgetLookupFailure struct{ interfaces.WikiPageService }

func (wikiBudgetLookupFailure) ExistsSlugs(context.Context, string, []string) (map[string]bool, error) {
	return nil, errors.New("lookup unavailable")
}

func TestWikiBudgetLookupFailureRetriesDocument(t *testing.T) {
	pageSvc, page := newWikiWriteGuardStore(t, "# Existing\n\nExisting facts.")
	svc := &wikiIngestService{
		wikiService: wikiBudgetLookupFailure{WikiPageService: pageSvc},
		chunkRepo:   &wikiReparseChunks{},
		knowledgeSvc: &wikiKnowledgeLookupStub{lookup: func(context.Context, string) (*types.Knowledge, error) {
			return &types.Knowledge{ID: "kn-1", Title: "Certification"}, nil
		}},
	}
	batchCtx := &WikiBatchContext{
		MaxPagesPerIngest:           1,
		SummaryContentByKnowledgeID: func(context.Context, string) string { return "" },
		SlugTitleMany:               func(context.Context, []string) map[string]string { return nil },
	}
	result, updates, err := svc.mapOneDocument(context.Background(), newWikiPageRetryModel(),
		WikiIngestPayload{TenantID: 1, KnowledgeBaseID: page.KnowledgeBaseID},
		WikiPendingOp{KnowledgeID: "kn-1", Op: WikiOpIngest}, batchCtx)
	require.ErrorContains(t, err, "resolve existing wiki pages for ingest budget")
	require.Nil(t, result)
	require.Nil(t, updates)
}

func TestWikiBudgetTracksOriginalSlugAfterIdentityRemap(t *testing.T) {
	pageSvc, page := newWikiWriteGuardStore(t, "# Existing\n\nExisting facts.")
	svc := &wikiIngestService{
		wikiService: pageSvc,
		chunkRepo:   &wikiReparseChunks{},
		knowledgeSvc: &wikiKnowledgeLookupStub{lookup: func(context.Context, string) (*types.Knowledge, error) {
			return &types.Knowledge{ID: "kn-1", Title: "Certification"}, nil
		}},
	}
	batchCtx := &WikiBatchContext{
		MaxPagesPerIngest:           1,
		SummaryContentByKnowledgeID: func(context.Context, string) string { return "" },
		SlugTitleMany:               func(context.Context, []string) map[string]string { return nil },
	}
	// Pass 0 first claims alpha. Replace the claim during the citation pass to
	// represent another map worker choosing a canonical slug before reclaim.
	model := newWikiPageRetryModel()
	model.respond = func(ctx context.Context, purpose, _ string, _ int) (string, error) {
		switch purpose {
		case "wiki_candidate_slug":
			return `{"entities":[{"name":"Alpha","slug":"entity/alpha"},{"name":"Beta","slug":"entity/beta"}],"concepts":[]}`, nil
		case "wiki_summary":
			return "SUMMARY: Alpha and Beta\n\n[[entity/alpha|Alpha]] and [[entity/beta|Beta]]", nil
		case "wiki_chunk_citation":
			svc.claimWikiIdentitySlug(ctx, page.KnowledgeBaseID, types.WikiPageTypeEntity, "Alpha", "entity/zalpha", true, batchCtx)
			return `{"citations":{},"new_slugs":[]}`, nil
		default:
			return `{"decisions":[]}`, nil
		}
	}
	result, _, err := svc.mapOneDocument(context.Background(), model,
		WikiIngestPayload{TenantID: 1, KnowledgeBaseID: page.KnowledgeBaseID},
		WikiPendingOp{KnowledgeID: "kn-1", Op: WikiOpIngest}, batchCtx)
	require.NoError(t, err)
	require.Contains(t, result.BudgetDroppedSlugs, "entity/alpha")
	require.Contains(t, result.BudgetDroppedSlugs, "entity/zalpha")
}

func TestApplyMaxPagesPerIngestBudget(t *testing.T) {
	entities := []extractedItem{
		{Name: "Existing", Slug: "entity/existing", SourceChunks: []string{"c1"}},
		{Name: "NewLow", Slug: "entity/new-low", SourceChunks: []string{"c2"}},
		{Name: "NewHigh", Slug: "entity/new-high", SourceChunks: []string{"c3", "c4", "c5"}},
		{Name: "NewTieA", Slug: "entity/new-tie-a", SourceChunks: []string{"c6", "c7"}},
	}
	concepts := []extractedItem{
		{Name: "ExistingConcept", Slug: "concept/existing", SourceChunks: []string{"c8"}},
		{Name: "NewTieB", Slug: "concept/new-tie-b", SourceChunks: []string{"c9", "c10"}},
		{Name: "NewZero", Slug: "concept/new-zero"},
	}
	existing := map[string]bool{
		"entity/existing":  true,
		"concept/existing": true,
	}

	// Unlimited (0) keeps everything.
	gotE, gotC, dropped := applyMaxPagesPerIngestBudget(entities, concepts, existing, 0)
	if dropped != 0 || len(gotE) != 4 || len(gotC) != 3 {
		t.Fatalf("unlimited budget: dropped=%d lenE=%d lenC=%d", dropped, len(gotE), len(gotC))
	}

	// Cap brand-new pages to 2: must keep BOTH existing pages plus the top 2 brand-new
	// pages by chunk count (NewHigh with 3 chunks, NewTieB with 2 chunks breaking tie by slug).
	gotE, gotC, dropped = applyMaxPagesPerIngestBudget(entities, concepts, existing, 2)
	if dropped != 3 {
		t.Fatalf("expected 3 dropped brand-new pages, got %d", dropped)
	}
	if len(gotE) != 2 {
		t.Fatalf("expected 2 kept entities (Existing + NewHigh), got %d: %+v", len(gotE), gotE)
	}
	if len(gotC) != 2 || gotC[0].Slug != "concept/existing" || gotC[1].Slug != "concept/new-tie-b" {
		t.Fatalf("expected existing concept and NewTieB, got %+v", gotC)
	}
	if findBySlug(gotE, "entity/existing") == nil || findBySlug(gotE, "entity/new-high") == nil {
		t.Fatalf("unexpected kept entities: %+v", gotE)
	}
}

func TestApplyMaxPagesPerIngestBudgetStableOnCandidateOrder(t *testing.T) {
	items := []extractedItem{{Slug: "entity/b", SourceChunks: []string{"c1"}}, {Slug: "entity/a", SourceChunks: []string{"c2"}}}
	kept, _, _ := applyMaxPagesPerIngestBudget(items, nil, nil, 1)
	if len(kept) != 1 || kept[0].Slug != "entity/a" {
		t.Fatalf("unexpected winner: %+v", kept)
	}
	items[0], items[1] = items[1], items[0]
	kept, _, _ = applyMaxPagesPerIngestBudget(items, nil, nil, 1)
	if len(kept) != 1 || kept[0].Slug != "entity/a" {
		t.Fatalf("winner changed after reordering: %+v", kept)
	}
}

func TestStripBudgetDroppedSummaryLinksKeepsPagesFromOtherDocuments(t *testing.T) {
	updates := map[string][]SlugUpdate{
		"summary/one":   {{Type: types.WikiPageTypeSummary, KnowledgeID: "one", SummaryLine: "[[entity/dropped|Dropped]] [[entity/shared|Shared]]", SummaryBody: "[[entity/dropped|Dropped]] [[entity/shared|Shared]] [[entity/existing|Existing]]"}},
		"entity/shared": {{Type: types.WikiPageTypeEntity, KnowledgeID: "two"}},
	}
	docs := []*docIngestResult{{KnowledgeID: "one", Summary: "[[entity/dropped|Dropped]] [[entity/shared|Shared]]", BudgetDroppedSlugs: []string{"entity/dropped", "entity/shared"}}}
	stripBudgetDroppedSummaryLinks(updates, docs)
	if got := updates["summary/one"][0].SummaryBody; got != "Dropped [[entity/shared|Shared]] [[entity/existing|Existing]]" {
		t.Fatalf("unexpected summary: %s", got)
	}
	require.Equal(t, "Dropped [[entity/shared|Shared]]", updates["summary/one"][0].SummaryLine)
	require.Equal(t, "Dropped [[entity/shared|Shared]]", docs[0].Summary)
}

func TestMergeCitationsIntoItems_PreservesExistingAndDeduplicatesDuplicateNewSlug(t *testing.T) {
	entities := []extractedItem{
		{Name: "Known", Slug: "entity/known"},
	}
	citations := map[string][]string{
		"entity/known": {"c001"},
	}
	newSlugs := []newSlugFromCitation{
		{Type: "entity", Name: "Known", Slug: "entity/known", SourceChunks: []string{"c001", "c020"}},
	}
	gotE, _, uncited := mergeCitationsIntoItems(entities, nil, citations, newSlugs)
	if uncited != 0 {
		t.Fatalf("expected uncited=0, got %d", uncited)
	}
	if !equalStrings(gotE[0].SourceChunks, []string{"c001", "c020"}) {
		t.Fatalf("expected deduplicated union [c001 c020], got %v", gotE[0].SourceChunks)
	}
}

func TestWikiLintGovernanceConstantsReadOnly(t *testing.T) {
	// Guardrail sanity checks: stub/oversized thresholds use Unicode runes.
	shortCN := "这是一个很短的词条说明"
	if utf8.RuneCountInString(shortCN) >= wikiLintThinStubMaxRunes {
		t.Fatalf("expected short Chinese text under stub threshold")
	}
	if LintIssueThinStub == "" || LintIssueOversizedPage == "" || LintIssueNearDuplicate == "" {
		t.Fatalf("governance lint issue constants must be non-empty")
	}
	if normalizeWikiIdentityTitle("  App ID ") != normalizeWikiIdentityTitle("appid") {
		t.Fatalf("expected normalized titles to match")
	}
	_ = types.WikiPageTypeConcept
}
