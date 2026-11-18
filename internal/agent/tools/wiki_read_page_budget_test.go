package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBulkyWikiPage builds a page whose body alone is larger than a fair share
// of the default output budget.
func newBulkyWikiPage(kbID, slug string, bodyRunes int) *types.WikiPage {
	page := newTestWikiPage(kbID, slug)
	page.Summary = "summary of " + slug
	page.Content = slug + " body " + strings.Repeat("x", bodyRunes)
	return page
}

func readPageOutput(t *testing.T, ctx context.Context, tool types.Tool, slugs []string) *types.ToolResult {
	t.Helper()
	args, err := json.Marshal(map[string]any{"slugs": slugs})
	require.NoError(t, err)
	result, err := tool.Execute(ctx, args)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success, "wiki_read_page failed: %s", result.Error)
	return result
}

// assertWellFormedPages guards the property the old tail truncation broke: the
// UI pairs <wiki_page> with </wiki_page>, so an unbalanced block is dropped
// entirely rather than shown partially.
func assertWellFormedPages(t *testing.T, output string, want int) {
	t.Helper()
	assert.Equal(t, want, strings.Count(output, "<wiki_page>"), "opening tags")
	assert.Equal(t, want, strings.Count(output, "</wiki_page>"), "closing tags")
	assert.Equal(t, want, strings.Count(output, "</content>"), "content sections")
}

// A batch read used to lose its middle pages: the pages were concatenated and
// then cut to a head/tail window by the registry, so slugs 3..5 of a 5-slug
// call disappeared without any signal to the model.
func TestWikiReadPageKeepsEveryRequestedSlugWithinBudget(t *testing.T) {
	slugs := []string{
		"concept/open-source-accessibility",
		"entity/open-source-accessibility-community-day",
		"concept/assistive-technology",
		"entity/github-multilingual-repositories-dataset",
		"concept/multilingual-ai",
	}
	pages := map[string]*types.WikiPage{}
	for _, slug := range slugs {
		pages[wikiPageKey("kb-1", slug)] = newBulkyWikiPage("kb-1", slug, 8000)
	}
	service := &fakeWikiPageService{pages: pages}
	tool := NewWikiReadPageTool(service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver())

	result := readPageOutput(t, context.Background(), tool, slugs)

	assertWellFormedPages(t, result.Output, len(slugs))
	for _, slug := range slugs {
		assert.Contains(t, result.Output, fmt.Sprintf("[[%s|%s]]", slug, slug),
			"every requested slug must still be rendered")
	}
	assert.LessOrEqual(t, utf8.RuneCountInString(result.Output), DefaultMaxToolOutput)
	assert.NotContains(t, result.Output, "<omitted_pages")
	assert.Len(t, result.Data["truncated_slugs"], len(slugs))
}

// Trimming has a floor. Below it, dropping a page by name beats rendering a
// stub, but the model must be told which slugs it still has not seen.
func TestWikiReadPageNamesOmittedPagesWhenBudgetIsTooSmall(t *testing.T) {
	slugs := []string{"concept/a", "concept/b", "concept/c", "concept/d", "concept/e"}
	pages := map[string]*types.WikiPage{}
	for _, slug := range slugs {
		pages[wikiPageKey("kb-1", slug)] = newBulkyWikiPage("kb-1", slug, 4000)
	}
	service := &fakeWikiPageService{pages: pages}
	tool := NewWikiReadPageTool(service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver())

	ctx := WithOutputBudget(context.Background(), 2000)
	result := readPageOutput(t, ctx, tool, slugs)

	omitted, ok := result.Data["omitted_slugs"].([]string)
	require.True(t, ok)
	require.NotEmpty(t, omitted, "a 2000-rune budget cannot hold five pages")

	assertWellFormedPages(t, result.Output, len(slugs)-len(omitted))
	assert.Contains(t, result.Output, "<omitted_pages")
	for _, slug := range omitted {
		assert.Contains(t, result.Output, slug)
	}
	assert.Contains(t, result.Output, "Call wiki_read_page again with fewer slugs")
}

// A page short enough to fit must not be trimmed just because a sibling in the
// same batch is huge.
func TestWikiReadPageKeepsSmallPagesIntactBesideLargeOnes(t *testing.T) {
	small := newTestWikiPage("kb-1", "concept/small")
	small.Content = "a compact body that easily fits the budget"
	service := &fakeWikiPageService{pages: map[string]*types.WikiPage{
		wikiPageKey("kb-1", "concept/small"): small,
		wikiPageKey("kb-1", "concept/huge"):  newBulkyWikiPage("kb-1", "concept/huge", 40000),
	}}
	tool := NewWikiReadPageTool(service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver())

	result := readPageOutput(t, context.Background(), tool, []string{"concept/small", "concept/huge"})

	assertWellFormedPages(t, result.Output, 2)
	assert.Contains(t, result.Output, small.Content, "the small page must survive untrimmed")
	assert.Equal(t, []string{"concept/huge"}, result.Data["truncated_slugs"])
}

// Inlining a full summary for every neighbour costs one query each and used to
// consume more budget than the bodies the caller actually asked for.
// Listing every remaining bare slug of a hub page (80+ links) still cost
// thousands of tokens, so each section is capped at wikiMaxLinkSlugs entries.
func TestWikiReadPageCapsLinkSections(t *testing.T) {
	tests := []struct {
		name      string
		links     int
		wantShown int
		wantNote  string
	}{
		{name: "hub page is capped", links: 100, wantShown: wikiMaxLinkSlugs,
			wantNote: fmt.Sprintf("(+%d more links omitted;", 100-wikiMaxLinkSlugs)},
		{name: "small page is unchanged", links: 10, wantShown: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := newTestWikiPage("kb-1", "concept/hub")
			pages := map[string]*types.WikiPage{wikiPageKey("kb-1", "concept/hub"): page}
			for i := 0; i < tt.links; i++ {
				out := fmt.Sprintf("entity/out-%03d", i)
				in := fmt.Sprintf("entity/in-%03d", i)
				page.OutLinks = append(page.OutLinks, out)
				page.InLinks = append(page.InLinks, in)
				neighbour := newTestWikiPage("kb-1", out)
				neighbour.Summary = "NEIGHBOUR_SUMMARY " + strings.Repeat("y", 500)
				pages[wikiPageKey("kb-1", out)] = neighbour
			}
			tool := NewWikiReadPageTool(&fakeWikiPageService{pages: pages}, nil,
				NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver())

			result := readPageOutput(t, context.Background(), tool, []string{"concept/hub"})

			assert.Equal(t, min(wikiMaxLinkSummaries, tt.links), strings.Count(result.Output, "NEIGHBOUR_SUMMARY"),
				"only the first %d neighbour summaries may be inlined", wikiMaxLinkSummaries)
			assert.NotContains(t, result.Output, strings.Repeat("y", wikiLinkSummaryMaxRunes+1),
				"each inlined summary must be capped")
			assert.Equal(t, tt.wantShown, strings.Count(result.Output, "[[entity/out-"))
			assert.Equal(t, tt.wantShown, strings.Count(result.Output, "[[entity/in-"))
			for i := 0; i < tt.wantShown; i++ {
				assert.Contains(t, result.Output, fmt.Sprintf("[[entity/out-%03d]]", i))
			}
			if tt.wantNote == "" {
				assert.NotContains(t, result.Output, "more links omitted")
				return
			}
			assert.Equal(t, 2, strings.Count(result.Output, tt.wantNote), "one note per link section")
		})
	}
}

// The same slug arriving twice used to be resolved and rendered twice, spending
// budget that a distinct page needed.
func TestWikiReadPageDedupesRequestedSlugs(t *testing.T) {
	service := &fakeWikiPageService{pages: map[string]*types.WikiPage{
		wikiPageKey("kb-1", "concept/a"): newTestWikiPage("kb-1", "concept/a"),
	}}
	tool := NewWikiReadPageTool(service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver())

	args := json.RawMessage(`{"slugs":["concept/a","concept/a"],"slug":"concept/a"}`)
	result, err := tool.Execute(context.Background(), args)
	require.NoError(t, err)
	require.True(t, result.Success)

	assertWellFormedPages(t, result.Output, 1)
}

func TestWikiReadSingleAmbiguousSlugDoesNotSuggestFewerSlugs(t *testing.T) {
	pages := map[string]*types.WikiPage{}
	var kbs []string
	for i := 0; i < 5; i++ {
		kb := fmt.Sprintf("kb-%d", i)
		kbs = append(kbs, kb)
		pages[wikiPageKey(kb, "index")] = newBulkyWikiPage(kb, "index", 4000)
	}
	tool := NewWikiReadPageTool(&fakeWikiPageService{pages: pages}, nil, NewWikiScopesFromKBIDs(kbs), NewWikiRouteResolver())
	result := readPageOutput(t, WithOutputBudget(context.Background(), 2000), tool, []string{"index"})
	assert.NotContains(t, result.Output, "fewer slugs")
	assert.Contains(t, result.Output, "knowledge_base_ids")
}

func TestWikiReadPageReservesBodyWhenRelationshipsExhaustBudget(t *testing.T) {
	page := newBulkyWikiPage("kb-platform", "entity/taptap", 2500)
	pending := pendingWikiPage{page: page, kbID: "kb-platform", body: "TAPTAP EVIDENCE " + strings.Repeat("x", 2500),
		outLinks: []string{strings.Repeat("related ", 1200)}, inLinks: []string{strings.Repeat("incoming ", 600)}, sources: []string{`<source knowledge_id="doc-1"/>`}}
	out, _, omitted := renderWikiPagesWithinBudget([]pendingWikiPage{pending}, 5000)
	require.Contains(t, out, "TAPTAP EVIDENCE")
	require.Contains(t, out, "kb-platform")
	require.Contains(t, out, "doc-1")
	require.Contains(t, out, "relationships omitted for output budget")
	require.NotContains(t, out, "body omitted: output budget exhausted")
	require.Empty(t, omitted)
	require.LessOrEqual(t, utf8.RuneCountInString(out), 5000)
}

func TestWikiBudgetKeepsSameSlugSourcesDistinctAndEmptyBodyHonest(t *testing.T) {
	first := newTestWikiPage("kb-a", "entity/taptap")
	second := newTestWikiPage("kb-b", "entity/taptap")
	pages := []pendingWikiPage{
		{page: first, kbID: "kb-a", outLinks: []string{strings.Repeat("关系", 2200)}, sources: []string{`<source knowledge_id="a"/>`}, body: "甲知识正文" + strings.Repeat("甲", 1000)},
		{page: second, kbID: "kb-b", outLinks: []string{strings.Repeat("关系", 2200)}, sources: []string{`<source knowledge_id="b"/>`}, body: "乙知识正文" + strings.Repeat("乙", 1000)},
	}
	out, _, omitted := renderWikiPagesWithinBudget(pages, 6000)
	require.Empty(t, omitted)
	for _, want := range []string{"kb-a", "kb-b", `knowledge_id="a"`, `knowledge_id="b"`, "甲知识正文", "乙知识正文"} {
		require.Contains(t, out, want)
	}
	require.LessOrEqual(t, utf8.RuneCountInString(out), 6000)
	out, truncated, _ := renderWikiPagesWithinBudget([]pendingWikiPage{{page: first, kbID: "kb-a", body: ""}}, 5000)
	require.Empty(t, truncated)
	require.NotContains(t, out, "body omitted")
}

func TestWikiBudgetKeepsFirstBodyWhenManyPagesExceedBudget(t *testing.T) {
	var pages []pendingWikiPage
	for i := 0; i < 12; i++ {
		slug := fmt.Sprintf("entity/page-%d", i)
		page := newTestWikiPage("kb-a", slug)
		pages = append(pages, pendingWikiPage{page: page, kbID: "kb-a", body: "PAGE EVIDENCE " + strings.Repeat("正", 1000), outLinks: []string{strings.Repeat("关系", 3000)}, sources: []string{`<source knowledge_id="doc-1"/>`}})
	}
	out, _, omitted := renderWikiPagesWithinBudget(pages, 5000)
	require.Contains(t, out, "PAGE EVIDENCE")
	require.Contains(t, out, `knowledge_id="doc-1"`)
	require.NotEmpty(t, omitted)
	require.LessOrEqual(t, utf8.RuneCountInString(out), 5000)
}

func TestWikiSeenSummariesOnlyAfterRenderedForCorrectKB(t *testing.T) {
	parentA := newTestWikiPage("kb-a", "entity/parent-a")
	parentA.Content = strings.Repeat("正文", 900)
	parentA.OutLinks = []string{"entity/target"}
	for i := 0; i < 14; i++ {
		parentA.OutLinks = append(parentA.OutLinks, fmt.Sprintf("entity/very-long-neighbour-%03d-%s", i, strings.Repeat("x", 35)))
	}
	parentB := newTestWikiPage("kb-a", "entity/parent-b")
	parentB.OutLinks = []string{"entity/target"}
	parentD := newTestWikiPage("kb-a", "entity/parent-d")
	parentD.OutLinks = []string{"entity/target"}
	parentC := newTestWikiPage("kb-b", "entity/parent-c")
	parentC.OutLinks = []string{"entity/target"}
	targetA := newTestWikiPage("kb-a", "entity/target")
	targetA.Summary = "TARGET A VISIBLE SUMMARY"
	targetB := newTestWikiPage("kb-b", "entity/target")
	targetB.Summary = "TARGET B VISIBLE SUMMARY"
	pages := map[string]*types.WikiPage{}
	for _, p := range []*types.WikiPage{parentA, parentB, parentC, parentD, targetA, targetB} {
		pages[wikiPageKey(p.KnowledgeBaseID, p.Slug)] = p
	}
	tool := NewWikiReadPageTool(&fakeWikiPageService{pages: pages}, nil, NewWikiScopesFromKBIDs([]string{"kb-a", "kb-b"}), NewWikiRouteResolver())
	first := readPageOutput(t, WithOutputBudget(context.Background(), 1400), tool, []string{"entity/parent-a"})
	require.Contains(t, first.Output, "relationships omitted for output budget")
	require.NotContains(t, first.Output, targetA.Summary)
	second := readPageOutput(t, context.Background(), tool, []string{"entity/parent-b"})
	require.Contains(t, second.Output, targetA.Summary, "a suppressed summary must not become seen")
	third := readPageOutput(t, context.Background(), tool, []string{"entity/parent-c"})
	require.Contains(t, third.Output, targetB.Summary, "a different KB must not inherit seen state")
	fourth := readPageOutput(t, context.Background(), tool, []string{"entity/parent-d"})
	require.Contains(t, fourth.Output, "summary omitted, already seen", "visible summaries should still be deduplicated")
}

func TestWikiOmittedPageDoesNotMarkSummarySeen(t *testing.T) {
	first := newTestWikiPage("kb-a", "entity/first")
	first.Content = strings.Repeat("A", 1500)
	second := newTestWikiPage("kb-a", "entity/second")
	second.OutLinks = []string{"entity/target"}
	second.Summary = "OMITTED PAGE SUMMARY"
	second.Content = strings.Repeat("B", 1500)
	parent := newTestWikiPage("kb-a", "entity/later")
	parent.OutLinks = []string{"entity/target", second.Slug}
	target := newTestWikiPage("kb-a", "entity/target")
	target.Summary = "NOT YET VISIBLE"
	pages := map[string]*types.WikiPage{}
	for _, p := range []*types.WikiPage{first, second, parent, target} {
		pages[wikiPageKey(p.KnowledgeBaseID, p.Slug)] = p
	}
	tool := NewWikiReadPageTool(&fakeWikiPageService{pages: pages}, nil, NewWikiScopesFromKBIDs([]string{"kb-a"}), NewWikiRouteResolver())
	result := readPageOutput(t, WithOutputBudget(context.Background(), 1200), tool, []string{first.Slug, second.Slug})
	require.Contains(t, result.Output, "<omitted_pages")
	require.NotContains(t, result.Output, target.Summary)
	next := readPageOutput(t, context.Background(), tool, []string{parent.Slug})
	require.Contains(t, next.Output, target.Summary)
	require.Contains(t, next.Output, second.Summary, "a wholly omitted page must not be marked seen")
}

func TestWikiRegistryTruncationDoesNotMarkSummariesSeen(t *testing.T) {
	first := newTestWikiPage("kb-a", "entity/first")
	first.OutLinks = []string{"entity/target"}
	later := newTestWikiPage("kb-a", "entity/later")
	later.OutLinks = []string{"entity/target"}
	target := newTestWikiPage("kb-a", "entity/target")
	target.Summary = "SUMMARY MUST REMAIN AVAILABLE"
	pages := map[string]*types.WikiPage{}
	for _, p := range []*types.WikiPage{first, later, target} {
		pages[wikiPageKey(p.KnowledgeBaseID, p.Slug)] = p
	}
	tool := NewWikiReadPageTool(&fakeWikiPageService{pages: pages}, nil, NewWikiScopesFromKBIDs([]string{"kb-a"}), NewWikiRouteResolver())
	slugs := []string{first.Slug}
	for i := 0; i < 80; i++ {
		slugs = append(slugs, fmt.Sprintf("entity/missing-%03d", i))
	}
	result := readPageOutput(t, WithOutputBudget(context.Background(), 1200), tool, slugs)
	require.Greater(t, utf8.RuneCountInString(result.Output), 1200, "error trailer must trigger registry truncation")
	require.Contains(t, result.Output, target.Summary, "the raw output alone would otherwise mark this summary seen")
	next := readPageOutput(t, context.Background(), tool, []string{later.Slug})
	require.Contains(t, next.Output, target.Summary, "a possibly truncated result must not commit seen state")
}
