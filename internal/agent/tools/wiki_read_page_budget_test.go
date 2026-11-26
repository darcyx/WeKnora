package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
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

// <truncated_pages> and <omitted_pages> name one slug per line, so a wide batch
// outgrows the fixed wikiBudgetReserve the pages are rendered against. The tool
// then overshot the ceiling it was handed and the registry's fallback cut a
// head/tail window through the result, unbalancing <wiki_page> pairs — the very
// outcome the budget code exists to prevent. The ceiling is per agent
// (MaxToolOutputChars), so a batch that fits the default can still overflow a
// smaller one: the trailer's lines must be budgeted, not assumed.
func TestWikiReadPageKeepsManyTrimmedSlugsInsideTheCeiling(t *testing.T) {
	slugs := []string{
		"concept/open-source-accessibility",
		"entity/open-source-accessibility-community-day",
		"concept/assistive-technology",
		"entity/github-multilingual-repositories-dataset",
		"concept/multilingual-ai",
		"entity/github-actions-self-hosted-runner",
		"concept/open-source-license-compatibility",
		"entity/document-layout-parsing-toolkit",
		"concept/retrieval-augmented-generation",
		"entity/vector-database-benchmark-suite",
		"concept/knowledge-graph-construction",
		"entity/assistive-technology-vendor-map",
	}
	const budget = 2000 // a small per-agent ceiling, far below the default 24000
	pages := map[string]*types.WikiPage{}
	for _, slug := range slugs {
		pages[wikiPageKey("kb-1", slug)] = newBulkyWikiPage("kb-1", slug, 8000)
	}
	service := &fakeWikiPageService{pages: pages}
	args, err := json.Marshal(map[string]any{"slugs": slugs})
	require.NoError(t, err)

	// The tool is told the same ceiling the registry falls back to, and must
	// shape pages plus trailers to fit it.
	tool := NewWikiReadPageTool(service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver())
	result := readPageOutput(t, WithOutputBudget(context.Background(), budget), tool, slugs)
	truncated, _ := result.Data["truncated_slugs"].([]string)
	omitted, _ := result.Data["omitted_slugs"].([]string)
	require.Len(t, append(append([]string{}, truncated...), omitted...), len(slugs),
		"this batch must cut or drop every page")
	assert.GreaterOrEqual(t, len(truncated)+len(omitted), 8, "the trailers must name many slugs")
	assert.LessOrEqual(t, utf8.RuneCountInString(result.Output), budget,
		"the trailer's slug lines are output too and must be charged to the budget")

	// The same read through the registry must arrive with every rendered
	// <wiki_page> still paired; its head/tail fallback is what breaks them.
	registry := NewToolRegistry()
	registry.SetMaxToolOutputSize(budget)
	registry.RegisterTool(NewWikiReadPageTool(
		service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver()))
	regResult, err := registry.ExecuteTool(context.Background(), ToolWikiReadPage, args)
	require.NoError(t, err)
	require.True(t, regResult.Success, "wiki_read_page failed: %s", regResult.Error)

	assertWellFormedPages(t, regResult.Output, len(slugs)-len(omitted))
	assert.Contains(t, regResult.Output, "</truncated_pages>", "the trailer must survive whole")
	assert.Contains(t, regResult.Output, "</omitted_pages>", "the trailer must survive whole")
	for _, slug := range slugs {
		assert.Contains(t, regResult.Output, slug, "every cut or dropped slug must still be named")
	}
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

// The head/tail window hides the middle of a trimmed page. Because
// wiki_write_page replaces the whole page, a rewrite based on a truncated read
// would silently drop every hidden line, so the output must say which pages
// were cut — and only those.
func TestWikiReadPageMarksTruncatedPagesInOutput(t *testing.T) {
	small := newTestWikiPage("kb-1", "concept/small")
	small.Content = "a compact body that easily fits the budget"
	service := &fakeWikiPageService{pages: map[string]*types.WikiPage{
		wikiPageKey("kb-1", "concept/small"): small,
		wikiPageKey("kb-1", "concept/huge"):  newBulkyWikiPage("kb-1", "concept/huge", 40000),
	}}
	tool := NewWikiReadPageTool(service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver())

	result := readPageOutput(t, context.Background(), tool, []string{"concept/small", "concept/huge"})

	require.Equal(t, []string{"concept/huge"}, result.Data["truncated_slugs"])
	require.Contains(t, result.Output, "<truncated_pages")
	marker := result.Output[strings.Index(result.Output, "<truncated_pages"):]
	marker = marker[:strings.Index(marker, "</truncated_pages>")]
	assert.Contains(t, marker, "concept/huge", "the trimmed page must be named")
	assert.NotContains(t, marker, "concept/small", "an intact page must not be named")
	assert.Contains(t, result.Output, "wiki_write_page",
		"the hint must warn against rewriting the page from a partial read")
}

// A read that fits the budget is the common case and must stay noise-free.
func TestWikiReadPageHasNoTruncationMarkerWhenNothingIsCut(t *testing.T) {
	service := &fakeWikiPageService{pages: map[string]*types.WikiPage{
		wikiPageKey("kb-1", "concept/small"): newTestWikiPage("kb-1", "concept/small"),
	}}
	tool := NewWikiReadPageTool(service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver())

	result := readPageOutput(t, context.Background(), tool, []string{"concept/small"})

	assert.Empty(t, result.Data["truncated_slugs"])
	assert.NotContains(t, result.Output, "<truncated_pages")
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

// wikiPageBatch resolves one bulky page per slug for the ceiling tests below.
func wikiPageBatch(slugs []string, bodyRunes int) map[string]*types.WikiPage {
	pages := make(map[string]*types.WikiPage, len(slugs))
	for _, slug := range slugs {
		pages[wikiPageKey("kb-1", slug)] = newBulkyWikiPage("kb-1", slug, bodyRunes)
	}
	return pages
}

// readPageThroughRegistry executes the tool the way the agent does, with the
// ceiling set on the registry itself, and returns both the registry's result and
// the same call's result when the tool is executed directly with the same
// ceiling. The two must agree: the registry only falls back to its head/tail
// truncation when the tool overshot.
func readPageThroughRegistry(
	t *testing.T,
	service *fakeWikiPageService,
	slugs []string,
	budget int,
) (*types.ToolResult, *types.ToolResult) {
	t.Helper()
	args, err := json.Marshal(map[string]any{"slugs": slugs})
	require.NoError(t, err)

	direct := readPageOutput(t, WithOutputBudget(context.Background(), budget), NewWikiReadPageTool(
		service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver()), slugs)

	registry := NewToolRegistry()
	registry.SetMaxToolOutputSize(budget)
	registry.RegisterTool(NewWikiReadPageTool(
		service, nil, NewWikiScopesFromKBIDs([]string{"kb-1"}), NewWikiRouteResolver()))
	result, err := registry.ExecuteTool(context.Background(), ToolWikiReadPage, args)
	require.NoError(t, err)
	require.True(t, result.Success, "wiki_read_page failed: %s", result.Error)
	return direct, result
}

// tinyCeilingSlugs is a wide batch: 20 pages is enough for the trailer bound
// (one line per slug) to exceed these ceilings on its own, which is the shape
// where keeping the first render overshot the ceiling.
func tinyCeilingSlugs() []string {
	return []string{
		"concept/open-source-accessibility",
		"entity/open-source-accessibility-community-day",
		"concept/assistive-technology",
		"entity/github-multilingual-repositories-dataset",
		"concept/multilingual-ai",
		"entity/github-actions-self-hosted-runner",
		"concept/open-source-license-compatibility",
		"entity/document-layout-parsing-toolkit",
		"concept/retrieval-augmented-generation",
		"entity/vector-database-benchmark-suite",
		"concept/knowledge-graph-construction",
		"entity/assistive-technology-vendor-map",
		"entity/open-source-hardware-license",
		"concept/data-provenance-tracking",
		"entity/multilingual-speech-corpus",
		"concept/federated-knowledge-graph",
		"entity/assistive-technology-standards-body",
		"concept/vector-index-quantization",
		"entity/open-access-publishing-agreement",
		"concept/retrieval-evaluation-harness",
	}
}

// A ceiling this small cannot hold the pages plus the full trailers, and the
// batch-wide trailer bound is larger than the ceiling itself. Whatever the
// render chooses, it must be one result that already fits: pages paired, the
// trailers closed, and every page either shown, trimmed or counted. Handing the
// registry a blob that overshoots is what lets its head/tail fallback cut a
// <wiki_page> pair in half.
func TestWikiReadPageTinyCeilingsKeepTheResultWhole(t *testing.T) {
	slugs := tinyCeilingSlugs()
	service := &fakeWikiPageService{pages: wikiPageBatch(slugs, 8000)}

	for _, budget := range []int{800, 1000, 1200} {
		t.Run(fmt.Sprintf("ceiling-%d", budget), func(t *testing.T) {
			direct, result := readPageThroughRegistry(t, service, slugs, budget)
			output := result.Output

			assert.LessOrEqual(t, utf8.RuneCountInString(direct.Output), budget,
				"the tool must render inside the ceiling it was handed")
			assert.Equal(t, direct.Output, output,
				"the registry's head/tail fallback must not have touched the result")

			open := strings.Count(output, "<wiki_page>")
			assertWellFormedPages(t, output, open)
			for _, tag := range []string{"truncated_pages", "omitted_pages"} {
				assert.Equal(t, strings.Count(output, "<"+tag), strings.Count(output, "</"+tag+">"),
					"the %s trailer must survive whole", tag)
			}

			truncated, _ := result.Data["truncated_slugs"].([]string)
			omitted, _ := result.Data["omitted_slugs"].([]string)
			reported := append(append([]string{}, truncated...), omitted...)
			require.Len(t, reported, len(slugs), "every page must be shown, trimmed or dropped")
			assert.Equal(t, len(slugs)-open, len(omitted),
				"a page that is not rendered must be reported as omitted")

			// The trailer must still tell the model that more pages are waiting:
			// it names them, or it counts what it could not name.
			named := 0
			for _, slug := range reported {
				if strings.Contains(output, slug) {
					named++
				}
			}
			if named < len(reported) {
				assert.Contains(t, output, "more", "the trailer must count the pages it cannot name")
			}
			assert.NotEmpty(t, reported, "a 1200 rune ceiling cannot show 20 pages of 8000 runes")
		})
	}
}

// The compact trailer form is what keeps a page on screen when the full trailer
// text does not fit: at 1200 runes the batch-wide bound leaves no page room,
// while the capped lists still do.
func TestWikiReadPageCompactTrailersKeepAPageOnScreen(t *testing.T) {
	slugs := []string{
		"concept/open-source-accessibility",
		"entity/open-source-accessibility-community-day",
		"concept/assistive-technology",
		"entity/github-multilingual-repositories-dataset",
		"concept/multilingual-ai",
		"entity/github-actions-self-hosted-runner",
		"concept/open-source-license-compatibility",
		"entity/document-layout-parsing-toolkit",
		"concept/retrieval-augmented-generation",
		"entity/vector-database-benchmark-suite",
		"concept/knowledge-graph-construction",
		"entity/assistive-technology-vendor-map",
	}
	service := &fakeWikiPageService{pages: wikiPageBatch(slugs, 8000)}
	for i, slug := range slugs {
		service.pages[wikiPageKey("kb-1", slug)].SourceRefs = []string{fmt.Sprintf("doc-%d|Source %d", i, i)}
	}

	for _, budget := range []int{1000, 1200} {
		t.Run(fmt.Sprintf("ceiling-%d", budget), func(t *testing.T) {
			direct, result := readPageThroughRegistry(t, service, slugs, budget)

			assert.LessOrEqual(t, utf8.RuneCountInString(direct.Output), budget)
			assert.Equal(t, direct.Output, result.Output)
			assertWellFormedPages(t, result.Output, strings.Count(result.Output, "<wiki_page>"))
			require.GreaterOrEqual(t, strings.Count(result.Output, "<wiki_page>"), 1,
				"the compact trailers must leave room for a page")
			var expected []types.PublishedSourceRef
			for i := range slugs {
				id := fmt.Sprintf("doc-%d", i)
				if strings.Contains(result.Output, `knowledge_id="`+id+`"`) {
					expected = append(expected, types.PublishedSourceRef{ID: id, Citable: false})
				}
			}
			require.NotEmpty(t, expected)
			require.Less(t, len(expected), len(slugs))
			require.Equal(t, expected, result.PublishedSources)

			truncated, _ := result.Data["truncated_slugs"].([]string)
			require.NotEmpty(t, truncated)
			assert.Contains(t, result.Output, "<truncated_pages")
			assert.Contains(t, result.Output, "</truncated_pages>")
			assert.Contains(t, result.Output, "wiki_write_page",
				"the hint must still warn against rewriting a trimmed page")
			if named := strings.Count(result.Output, truncated[0]); named > 0 {
				assert.Contains(t, result.Output, "more",
					"a trailer that does not name every trimmed page must count them")
			}
		})
	}
}

// Below the compact trailers' own fixed text there is nothing left to pair, so
// the tool answers with a clipped sentence and no markup at all. The point is
// that it still answers inside the ceiling instead of handing the registry a
// blob whose head/tail fallback would leave a half-open tag behind.
func TestWikiReadPageReportsCeilingsTooSmallForAnyTrailer(t *testing.T) {
	slugs := tinyCeilingSlugs()
	service := &fakeWikiPageService{pages: wikiPageBatch(slugs, 8000)}

	for _, budget := range []int{40, 100, 160} {
		t.Run(fmt.Sprintf("ceiling-%d", budget), func(t *testing.T) {
			direct, result := readPageThroughRegistry(t, service, slugs, budget)

			assert.LessOrEqual(t, utf8.RuneCountInString(direct.Output), budget)
			assert.Equal(t, direct.Output, result.Output)
			assertWellFormedPages(t, result.Output, 0)
			assert.Equal(t, 0, strings.Count(result.Output, "<truncated_pages"))
			assert.Equal(t, 0, strings.Count(result.Output, "<omitted_pages"))
			assert.NotEmpty(t, result.Output, "the tool must still say something")
		})
	}
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
	out, _, omitted, _ := renderWikiPagesWithinBudget([]pendingWikiPage{pending}, 5000, wikiBudgetReserve)
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
	out, _, omitted, _ := renderWikiPagesWithinBudget(pages, 6000, wikiBudgetReserve)
	require.Empty(t, omitted)
	for _, want := range []string{"kb-a", "kb-b", `knowledge_id="a"`, `knowledge_id="b"`, "甲知识正文", "乙知识正文"} {
		require.Contains(t, out, want)
	}
	require.LessOrEqual(t, utf8.RuneCountInString(out), 6000)
	out, truncated, _, _ := renderWikiPagesWithinBudget([]pendingWikiPage{{page: first, kbID: "kb-a", body: ""}}, 5000, wikiBudgetReserve)
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
	out, _, omitted, _ := renderWikiPagesWithinBudget(pages, 5000, wikiBudgetReserve)
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

func TestWikiBudgetTracksVisibleSummaries(t *testing.T) {
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
	require.LessOrEqual(t, utf8.RuneCountInString(result.Output), 1200)
	next := readPageOutput(t, context.Background(), tool, []string{later.Slug})
	if strings.Contains(result.Output, target.Summary) {
		require.NotContains(t, next.Output, target.Summary)
	} else {
		require.Contains(t, next.Output, target.Summary)
	}
}

// A compact error trailer leaves the visible page and its sources intact.
func TestWikiPublishedSourcesWithCompactErrorTrailer(t *testing.T) {
	page := newTestWikiPage("kb-a", "entity/valid")
	page.SourceRefs = []string{"doc-1|登录文档"}
	tool := NewWikiReadPageTool(&fakeWikiPageService{pages: map[string]*types.WikiPage{
		wikiPageKey("kb-a", page.Slug): page,
	}}, nil, NewWikiScopesFromKBIDs([]string{"kb-a"}), NewWikiRouteResolver())
	slugs := []string{page.Slug}
	for i := 0; i < 80; i++ {
		slugs = append(slugs, fmt.Sprintf("entity/missing-%03d", i))
	}
	result := readPageOutput(t, WithOutputBudget(context.Background(), 1200), tool, slugs)
	require.LessOrEqual(t, utf8.RuneCountInString(result.Output), 1200)
	require.Contains(t, result.Output, `knowledge_id="doc-1"`)
	require.Equal(t, []string{"doc-1"}, result.Data["source_knowledge_ids"])
	require.Equal(t, []types.PublishedSourceRef{{ID: "doc-1", Citable: false}}, result.PublishedSources)
}

type fakeWikiSourceTitleKnowledgeService struct {
	interfaces.KnowledgeService
	docs       map[string]*types.Knowledge
	batchCalls int
	lastIDs    []string
}

func (f *fakeWikiSourceTitleKnowledgeService) GetKnowledgeBatchWithSharedAccess(_ context.Context, _ uint64, ids []string) ([]*types.Knowledge, error) {
	f.batchCalls++
	f.lastIDs = append([]string(nil), ids...)
	out := make([]*types.Knowledge, 0, len(ids))
	for _, id := range ids {
		if doc := f.docs[id]; doc != nil {
			out = append(out, doc)
		}
	}
	return out, nil
}

func TestWikiReadPageResolvesMissingSourceTitlesInSingleBatch(t *testing.T) {
	pageA := newTestWikiPage("kb-a", "entity/shanyan")
	pageA.SourceRefs = []string{"doc-1", "doc-2", "doc-3|已有标题"}
	pageB := newTestWikiPage("kb-a", "entity/aliyun")
	pageB.SourceRefs = []string{"doc-2", "doc-missing"}
	ks := &fakeWikiSourceTitleKnowledgeService{
		docs: map[string]*types.Knowledge{
			"doc-1": {ID: "doc-1", Title: "打包文档 <AppID & AppKey>"},
			"doc-2": {ID: "doc-2", FileName: "channel-config.md"},
		},
	}
	tool := NewWikiReadPageTool(
		&fakeWikiPageService{pages: map[string]*types.WikiPage{
			wikiPageKey("kb-a", pageA.Slug): pageA,
			wikiPageKey("kb-a", pageB.Slug): pageB,
		}},
		ks,
		NewWikiScopesFromKBIDs([]string{"kb-a"}),
		NewWikiRouteResolver(),
	)

	result := readPageOutput(t, context.Background(), tool, []string{pageA.Slug, pageB.Slug})

	require.Equal(t, 1, ks.batchCalls)
	require.ElementsMatch(t, []string{"doc-1", "doc-2", "doc-missing"}, ks.lastIDs)
	require.Contains(t, result.Output, `<source knowledge_id="doc-1">打包文档 &lt;AppID &amp; AppKey&gt;</source>`)
	require.Contains(t, result.Output, `<source knowledge_id="doc-2">channel-config.md</source>`)
	require.Contains(t, result.Output, `<source knowledge_id="doc-3">已有标题</source>`)
	require.Contains(t, result.Output, `<source knowledge_id="doc-missing"/>`)
	require.Contains(t, tool.Description(), `read_document(id="doc://N", query=...)`)
}

func TestWikiReadPageBudgetRetainsUpToFiveSourcesBeforeSingleFallback(t *testing.T) {
	page := newBulkyWikiPage("kb-a", "concept/login", 2200)
	sources := make([]string, 0, 30)
	for i := 1; i <= 30; i++ {
		sources = append(sources, fmt.Sprintf(`<source knowledge_id="doc-%d">SDK渠道登录配置与参数说明文档-%02d</source>`, i, i))
	}
	pending := pendingWikiPage{
		page:     page,
		kbID:     "kb-a",
		body:     "LOGIN EVIDENCE " + strings.Repeat("正", 2200),
		outLinks: []string{strings.Repeat("related ", 1000)},
		inLinks:  []string{strings.Repeat("incoming ", 500)},
		sources:  sources,
	}
	out, _, omitted, _ := renderWikiPagesWithinBudget([]pendingWikiPage{pending}, 1800, wikiBudgetReserve)
	require.Empty(t, omitted)
	require.Contains(t, out, "LOGIN EVIDENCE")
	for i := 1; i <= 5; i++ {
		require.Contains(t, out, fmt.Sprintf(`<source knowledge_id="doc-%d">SDK渠道登录配置与参数说明文档-%02d</source>`, i, i))
	}
	require.NotContains(t, out, `knowledge_id="doc-6"`)
	require.Contains(t, out, "(+25 more sources omitted for output budget)")
	require.LessOrEqual(t, utf8.RuneCountInString(out), 1800)
}

func TestWikiPublishedSourcesRespectBudget(t *testing.T) {
	page := newBulkyWikiPage("kb-a", "concept/login", 2200)
	for i := 1; i <= 30; i++ {
		page.SourceRefs = append(page.SourceRefs, fmt.Sprintf("doc-%d|SDK渠道登录配置与参数说明文档-%02d", i, i))
	}
	tool := NewWikiReadPageTool(&fakeWikiPageService{pages: map[string]*types.WikiPage{
		wikiPageKey("kb-a", page.Slug): page,
	}}, nil, NewWikiScopesFromKBIDs([]string{"kb-a"}), NewWikiRouteResolver())
	result := readPageOutput(t, WithOutputBudget(context.Background(), 1800), tool, []string{page.Slug})
	ids := result.Data["source_knowledge_ids"].([]string)
	for _, ref := range page.SourceRefs {
		id, _ := types.ParseWikiSourceRef(ref)
		if strings.Contains(result.Output, `knowledge_id="`+id+`"`) {
			require.Contains(t, ids, id)
		} else {
			require.NotContains(t, ids, id)
		}
	}
	require.Less(t, len(ids), len(page.SourceRefs))
}

func TestWikiPublishedSourcesIgnorePageTagsInBody(t *testing.T) {
	for _, body := range []string{"ordinary text", `示例 <wiki_page><source knowledge_id="fake"/></wiki_page> 用法`} {
		t.Run(body, func(t *testing.T) {
			page := newTestWikiPage("kb-a", "entity/example")
			page.Content = body
			page.SourceRefs = []string{"doc-1|登录文档"}
			other := newTestWikiPage("kb-a", "entity/other")
			other.SourceRefs = []string{"doc-2|其他文档"}
			tool := NewWikiReadPageTool(&fakeWikiPageService{pages: map[string]*types.WikiPage{
				wikiPageKey("kb-a", page.Slug):  page,
				wikiPageKey("kb-a", other.Slug): other,
			}}, nil, NewWikiScopesFromKBIDs([]string{"kb-a"}), NewWikiRouteResolver())

			result := readPageOutput(t, context.Background(), tool, []string{page.Slug, other.Slug})
			require.Contains(t, result.Output, `<source knowledge_id="doc-1">`)
			require.Equal(t, []types.PublishedSourceRef{{ID: "doc-1", Citable: false}, {ID: "doc-2", Citable: false}}, result.PublishedSources)
		})
	}
}

func TestWikiPublishedSourcesExcludeOmittedPage(t *testing.T) {
	page := newTestWikiPage("kb-a", "entity/partial")
	page.Content = `示例 <wiki_page><source knowledge_id="doc-1"/></wiki_page>`
	page.SourceRefs = []string{"doc-1|登录文档"}
	tool := NewWikiReadPageTool(&fakeWikiPageService{pages: map[string]*types.WikiPage{
		wikiPageKey("kb-a", page.Slug): page,
	}}, nil, NewWikiScopesFromKBIDs([]string{"kb-a"}), NewWikiRouteResolver())

	result := readPageOutput(t, WithOutputBudget(context.Background(), 200), tool, []string{page.Slug})
	require.LessOrEqual(t, utf8.RuneCountInString(result.Output), 200)
	require.Empty(t, result.PublishedSources)
}

func TestWikiReadPageExactFitPreservesPageAndSources(t *testing.T) {
	page := newTestWikiPage("kb-a", "entity/exact")
	page.SourceRefs = []string{"doc-1|source"}
	tool := NewWikiReadPageTool(&fakeWikiPageService{pages: map[string]*types.WikiPage{wikiPageKey("kb-a", page.Slug): page}}, nil, NewWikiScopesFromKBIDs([]string{"kb-a"}), NewWikiRouteResolver())
	full := readPageOutput(t, context.Background(), tool, []string{page.Slug})
	budget := utf8.RuneCountInString(full.Output)
	exact := readPageOutput(t, WithOutputBudget(context.Background(), budget), tool, []string{page.Slug})
	require.Equal(t, full.Output, exact.Output)
	require.Equal(t, []types.PublishedSourceRef{{ID: "doc-1", Citable: false}}, exact.PublishedSources)
	require.Empty(t, exact.Data["truncated_slugs"])
	require.Empty(t, exact.Data["omitted_slugs"])
}
