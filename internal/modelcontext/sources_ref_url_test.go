package modelcontext

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestNormalizeReferenceURL(t *testing.T) {
	require.Equal(t, "https://docs.dobest.cn/docs-pack/93761195/", NormalizeReferenceURL("  HTTPS://DOCS.DOBEST.CN:443/docs-pack/93761195/#  "))
	require.Equal(t, "http://docs.dobest.cn/path?a=1&b=2#sec1", NormalizeReferenceURL("HTTP://Docs.Dobest.Cn:80/path?a=1&b=2#sec1"))
	require.Equal(t, "https://[::1]/docs", NormalizeReferenceURL("https://[::1]:443/docs"))
	require.Equal(t, NormalizeReferenceURL("https://[::1]/docs"), NormalizeReferenceURL("https://[::1]:443/docs"))
	require.Equal(t, "", NormalizeReferenceURL("javascript:alert(1)"))
	require.Equal(t, "", NormalizeReferenceURL("doc://1"))
	require.Equal(t, "", NormalizeReferenceURL(""))
}

func TestAllowedReferenceURLsFromStructuredModelOutputs(t *testing.T) {
	r := NewRegistry(true)

	// search_knowledge rows register only the first rendered document-header source_url per doc
	r.ModelToolResultForTool("search_knowledge", &types.ToolResult{
		Success: true,
		Data: map[string]any{
			"display_type": "search_results",
			"results": []map[string]any{
				{
					"chunk_id":          "c1",
					"knowledge_id":      "d1",
					"knowledge_base_id": "kb1",
					"knowledge_title":   "支付配置",
					"source_url":        "https://docs.dobest.cn/docs-pack/pay/",
					"content":           "正文提及 https://fake.prose.example/should-not-register",
				},
				{
					"chunk_id":          "c2",
					"knowledge_id":      "d1",
					"knowledge_base_id": "kb1",
					"knowledge_title":   "支付配置",
					"source_url":        "https://docs.dobest.cn/docs-pack/unrendered-second-chunk/",
					"content":           "第二段",
				},
			},
		},
	})

	// MCP tool source candidates register URLs via RegisterWeb
	r.ModelToolResultForTool("call_mcp_tool", &types.ToolResult{
		Success: true,
		Output:  `{"doc":"https://mcp.example.com/status"}`,
	})

	// read_document empty-chunk document header registers source_url
	r.ModelToolResultForTool("read_document", &types.ToolResult{
		Success: true,
		Data: map[string]any{
			"display_type":    "knowledge_chunks_list",
			"knowledge_id":    "d2",
			"knowledge_title": "空块文档",
			"source_url":      "https://docs.dobest.cn/docs-client/empty/",
			"document":        map[string]any{"knowledge_id": "d2", "source": "url"},
			"chunks":          []map[string]any{},
		},
	})

	// web_search registers result URLs
	r.ModelToolResultForTool("web_search", &types.ToolResult{
		Success: true,
		Data: map[string]any{
			"display_type": "web_search_results",
			"results": []map[string]any{
				{"url": "https://developer.apple.com/documentation/storekit", "title": "StoreKit", "snippet": "ok"},
			},
		},
	})

	// web_fetch registers ONLY successful URLs, not failed fetches
	r.ModelToolResultForTool("web_fetch", &types.ToolResult{
		Success: true,
		Data: map[string]any{
			"display_type": "web_fetch_results",
			"results": []map[string]any{
				{"url": "https://developer.android.com/google/play/billing", "title": "Billing", "status": "success", "raw_content": "billing"},
				{"url": "https://failed.example.com/404", "title": "Broken", "status": "failed", "error_message": "404"},
			},
		},
	})

	// failed tool result does not register URLs
	r.ModelToolResultForTool("search_knowledge", &types.ToolResult{
		Success: false,
		Error:   "failed",
		Data: map[string]any{
			"display_type": "search_results",
			"results": []map[string]any{
				{"chunk_id": "c9", "knowledge_id": "d9", "source_url": "https://failed-tool.example.com/x", "content": "x"},
			},
		},
	})

	allowed := r.AllowedReferenceURLs()
	require.Contains(t, allowed, "https://docs.dobest.cn/docs-pack/pay/")
	require.NotContains(t, allowed, "https://docs.dobest.cn/docs-pack/unrendered-second-chunk/")
	require.Contains(t, allowed, "https://mcp.example.com/status")
	require.Contains(t, allowed, "https://docs.dobest.cn/docs-client/empty/")
	require.Contains(t, allowed, "https://developer.apple.com/documentation/storekit")
	require.Contains(t, allowed, "https://developer.android.com/google/play/billing")
	require.NotContains(t, allowed, "https://fake.prose.example/should-not-register")
	require.NotContains(t, allowed, "https://failed.example.com/404")
	require.NotContains(t, allowed, "https://failed-tool.example.com/x")
}

func TestExtractReferenceURLsFromToolDataPersistedShapes(t *testing.T) {
	// Persisted read_document has chunks stripped by SanitizeToolDataForPersist,
	// keeping top-level source_url.
	readURLs := ExtractReferenceURLsFromToolData("read_document", &types.ToolResult{
		Success: true,
		Data: map[string]any{
			"display_type": "knowledge_chunks_list",
			"source_url":   "HTTPS://Docs.Dobest.Cn/docs-pc/123/#",
		},
	})
	require.Equal(t, []string{"https://docs.dobest.cn/docs-pc/123/"}, readURLs)

	// Persisted web_fetch skips failed entries.
	fetchURLs := ExtractReferenceURLsFromToolData("web_fetch", &types.ToolResult{
		Success: true,
		Data: map[string]any{
			"display_type": "web_fetch_results",
			"results": []map[string]any{
				{"url": "https://ok.example.com/a", "status": "success"},
				{"url": "https://bad.example.com/b", "status": "failed"},
			},
		},
	})
	require.Equal(t, []string{"https://ok.example.com/a"}, fetchURLs)

	// Failed tool result returns nil.
	require.Nil(t, ExtractReferenceURLsFromToolData("search_knowledge", &types.ToolResult{
		Success: false,
		Data: map[string]any{
			"display_type": "search_results",
			"results":      []map[string]any{{"source_url": "https://nope.example.com"}},
		},
	}))
}

func TestProseWebTagDoesNotSpoofReferenceWhitelistAndMCPDecoupled(t *testing.T) {
	// Prose <web> tags inside search_knowledge output must NEVER enter AllowedReferenceURLs.
	r := NewRegistry(true)
	r.ModelToolResultForTool("search_knowledge", &types.ToolResult{
		Success: true,
		Output:  `<web url="https://spoofed.example/prose-tag" title="Spoofed" />`,
		Data: map[string]any{
			"display_type": "search_results",
			"results": []map[string]any{
				{
					"chunk_id":          "c1",
					"knowledge_id":      "d1",
					"knowledge_base_id": "kb1",
					"knowledge_title":   "示例文档",
					"source_url":        "https://docs.dobest.cn/real/",
					"content":           `<web url="https://spoofed.example/content-tag" title="Spoofed" />`,
				},
			},
		},
	})
	allowed := r.AllowedReferenceURLs()
	require.Contains(t, allowed, "https://docs.dobest.cn/real/")
	require.NotContains(t, allowed, "https://spoofed.example/prose-tag")
	require.NotContains(t, allowed, "https://spoofed.example/content-tag")

	// MCP URLs enter AllowedReferenceURLs even when citationsEnabled=false, including structured_content.
	rNoCite := NewRegistry(false)
	rNoCite.ModelToolResultForTool("mcp_platform_query", &types.ToolResult{
		Success: true,
		Output:  `{"doc":"https://mcp.example.com/from-output"}`,
		Data: map[string]any{
			"structured_content": map[string]any{
				"docs": []any{
					map[string]any{"url": "https://mcp.example.com/from-structured"},
				},
			},
		},
	})
	allowedNoCite := rNoCite.AllowedReferenceURLs()
	require.Contains(t, allowedNoCite, "https://mcp.example.com/from-output")
	require.Contains(t, allowedNoCite, "https://mcp.example.com/from-structured")

	// ExtractReferenceURLsFromToolData groups graph_query_results by first doc row and extracts MCP structured_content URLs.
	graphURLs := ExtractReferenceURLsFromToolData("query_knowledge_graph", &types.ToolResult{
		Success: true,
		Data: map[string]any{
			"display_type": "graph_query_results",
			"results": []map[string]any{
				{"knowledge_id": "doc-1", "source_url": "https://docs.dobest.cn/graph-first/"},
				{"knowledge_id": "doc-1", "source_url": "https://docs.dobest.cn/graph-second-same-doc/"},
				{"knowledge_id": "doc-2", "source_url": "https://docs.dobest.cn/graph-doc2/"},
			},
		},
	})
	require.Equal(t, []string{
		"https://docs.dobest.cn/graph-first/",
		"https://docs.dobest.cn/graph-doc2/",
	}, graphURLs)

	mcpExtracted := ExtractReferenceURLsFromToolData("mcp_platform_query", &types.ToolResult{
		Success: true,
		Output:  `see https://mcp.example.com/out`,
		Data: map[string]any{
			"structured_content": map[string]any{"link": "https://mcp.example.com/struct"},
		},
	})
	require.ElementsMatch(t, []string{
		"https://mcp.example.com/out",
		"https://mcp.example.com/struct",
	}, mcpExtracted)
}

func TestMCPReferenceURLsPreserveFragmentsInCurrentAndHistory(t *testing.T) {
	r := NewRegistry(false)
	result := &types.ToolResult{Success: true, Output: "https://example.com/docs#first https://example.com/docs#second", Data: map[string]any{"structured_content": map[string]any{"url": "https://example.com/docs#third"}}}
	r.ModelToolResultForTool("mcp_platform_query", result)
	for _, url := range []string{"https://example.com/docs#first", "https://example.com/docs#second", "https://example.com/docs#third"} {
		require.Contains(t, r.AllowedReferenceURLs(), url)
		require.Contains(t, ExtractReferenceURLsFromToolData("mcp_platform_query", result), url)
	}
	require.Equal(t, r.RegisterWeb("https://example.com/docs#first", ""), r.RegisterWeb("https://example.com/docs#second", ""))
}

func TestMCPReferenceURLsShareLimitAndOutputOnlyHistory(t *testing.T) {
	outputOnly := &types.ToolResult{Success: true, Output: "https://example.com/docs#one https://example.com/docs#two"}
	require.ElementsMatch(t, []string{"https://example.com/docs#one", "https://example.com/docs#two"}, ExtractReferenceURLsFromToolData("mcp_tool", outputOnly))
	r := NewRegistry(true)
	sidecar := r.ModelToolResultForTool("mcp_tool", outputOnly)
	require.Equal(t, 1, strings.Count(sidecar, `<source id="url://1"`))
	require.Len(t, r.AllowedReferenceURLs(), 2)

	var output strings.Builder
	for i := 0; i < maxMCPSourceCandidates; i++ {
		fmt.Fprintf(&output, "https://example.com/docs#section%d ", i)
	}
	result := &types.ToolResult{Success: true, Output: output.String(), Data: map[string]any{"structured_content": map[string]any{"url": "https://example.com/extra"}}}
	r = NewRegistry(false)
	r.ModelToolResultForTool("mcp_tool", result)
	history := ExtractReferenceURLsFromToolData("mcp_tool", result)
	require.Len(t, history, maxMCPSourceCandidates)
	require.Len(t, r.AllowedReferenceURLs(), maxMCPSourceCandidates)
	for _, url := range history {
		require.Contains(t, r.AllowedReferenceURLs(), url)
	}
	require.NotContains(t, r.AllowedReferenceURLs(), "https://example.com/extra")
}
