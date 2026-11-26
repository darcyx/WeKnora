package tools

import (
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestWebSearchPrivacyPreFilter(t *testing.T) {
	appInfo := `{"app_id":"10008899","app_name":"星辰战纪","gameInfo":{"device_type":"手游"}}`

	blockedQueries := []string{
		"星辰战纪 支付回调失败",
		"app_id=10008899 登录报错",
		"SDK 10008899 渠道配置",
		"order_id=ORD20261125001 掉单",
		"app_secret=mysecretvalue123 验签",
		"sk-abcdefghij1234567890 api",
		`{"api_key": "plainvalue12345"} build error`,
		`{"app_id": "99887766"} timeout`,
	}
	for _, q := range blockedQueries {
		t.Run("blocks_"+q, func(t *testing.T) {
			client := &fakeWebSearchProvider{
				results: []*types.WebSearchResult{{Title: "Doc", URL: "https://developer.apple.com/storekit", Snippet: "ok"}},
			}
			tool := NewWebSearchTool(client, 5).WithAppInfo(appInfo)
			raw, _ := json.Marshal(map[string]any{"query": q})
			res, err := tool.Execute(t.Context(), raw)
			require.NoError(t, err)
			require.False(t, res.Success)
			require.Contains(t, res.Error, "blocked")
			require.Zero(t, client.calls)
		})
	}

	t.Run("allows public technical query", func(t *testing.T) {
		client := &fakeWebSearchProvider{
			results: []*types.WebSearchResult{{Title: "Doc", URL: "https://developer.apple.com/storekit", Snippet: "ok"}},
		}
		tool := NewWebSearchTool(client, 5).WithAppInfo(appInfo)
		raw, _ := json.Marshal(map[string]any{"query": "Apple StoreKit 2 sandbox testing guide"})
		res, err := tool.Execute(t.Context(), raw)
		require.NoError(t, err)
		require.True(t, res.Success)
		require.Equal(t, 1, client.calls)
	})

	t.Run("short 3-char app_id boundary matching and short key-value blocking", func(t *testing.T) {
		shortAppInfo := `{"app_id":"101","app_name":"XY"}`
		client := &fakeWebSearchProvider{
			results: []*types.WebSearchResult{{Title: "Doc", URL: "https://developer.apple.com/storekit", Snippet: "ok"}},
		}
		tool := NewWebSearchTool(client, 5).WithAppInfo(shortAppInfo)

		// Standalone 3-char app_id is blocked.
		rawBlocked, _ := json.Marshal(map[string]any{"query": "SDK 101 渠道报错"})
		res, err := tool.Execute(t.Context(), rawBlocked)
		require.NoError(t, err)
		require.False(t, res.Success)
		require.Zero(t, client.calls)

		// Short key-value app_id=12 is blocked.
		rawKV, _ := json.Marshal(map[string]any{"query": "app_id=12 登录超时"})
		res, err = tool.Execute(t.Context(), rawKV)
		require.NoError(t, err)
		require.False(t, res.Success)
		require.Zero(t, client.calls)

		// Substring inside longer identifier (e.g. HTTP 1010 or Unity202101) is allowed.
		rawAllowed, _ := json.Marshal(map[string]any{"query": "Unity 202101 Android Gradle build error"})
		res, err = tool.Execute(t.Context(), rawAllowed)
		require.NoError(t, err)
		require.True(t, res.Success)
		require.Equal(t, 1, client.calls)
	})

	t.Run("web_fetch blocks private query params raw and percent-encoded while allowing clean public URLs", func(t *testing.T) {
		const cleanURL = "https://developer.apple.com/documentation/storekit"
		fetcher := newStubWebContentFetcher(map[string]string{cleanURL: "StoreKit docs"}, nil)
		fetchTool := NewWebFetchTool(fetcher).WithAppInfo(appInfo)

		blockedURLs := []string{
			"https://developer.apple.com/docs?app_id=10008899",
			"https://developer.apple.com/docs?q=%E6%98%9F%E8%BE%B0%E6%88%98%E7%BA%AA",
			"https://developer.apple.com/docs/10008899/guide",
			"https://developer.apple.com/docs?order_id=ORD99",
		}
		for _, u := range blockedURLs {
			res, err := fetchTool.Execute(t.Context(), webFetchArgs(WebFetchItem{URL: u}))
			require.NoError(t, err)
			require.False(t, res.Success, "expected blocked URL: %s", u)
			rows := res.Data["results"].([]map[string]interface{})
			require.Equal(t, "privacy_blocked", rows[0]["error_code"])
		}
		require.Empty(t, fetcher.callCount)

		// Clean public URL succeeds.
		res, err := fetchTool.Execute(t.Context(), webFetchArgs(WebFetchItem{URL: cleanURL}))
		require.NoError(t, err)
		require.True(t, res.Success)
		require.Equal(t, 1, fetcher.callCount[cleanURL])
	})
}

func TestNumericAppIDMatchesWholeTokenForSearchAndFetch(t *testing.T) {
	long, short := extractSensitiveAppInfoTokens(`{"app_id":"1000","gameInfo":{"app_secret":"1000"}}`)
	require.Error(t, checkWebSearchPrivacy("HTTP 10000 error", long, short), "a secret retains strict substring matching")
	long, short = extractSensitiveAppInfoTokens(`{"app_name":"abc","gameInfo":{"app_secret":"abc"}}`)
	require.Error(t, checkWebSearchPrivacy("xabcx", long, short), "stronger name matching survives duplicate values")
	long, short = extractSensitiveAppInfoTokens(`{"app_id":"1000"}`)
	require.NoError(t, checkWebSearchPrivacy("HTTP 10000 error", long, short))
	require.Error(t, checkWebSearchPrivacy("SDK 1000 error", long, short))
	require.NoError(t, checkWebURLPrivacy("https://example.com/docs/10000", long, short))
	require.Error(t, checkWebURLPrivacy("https://example.com/docs/1000", long, short))
	require.Error(t, checkWebURLPrivacy("https://example.com/docs?app_id=10000", long, short))
}

func TestWebURLPrivacyBlocksHostUserinfoFragmentAndExtendedAppInfoTokens(t *testing.T) {
	appInfo := `{"token":"tok_secret_9876","pay_callback":"https://pay.internal.example/cb","refund_callback":"https://refund.internal.example/cb","subject_name":"星辉科技","studio_name":"苍穹工作室","app_alias":"starfall_cn"}`
	long, short := extractSensitiveAppInfoTokens(appInfo)

	for _, u := range []string{
		"https://tok_secret_9876.evil.com/docs",
		"https://tok_secret_9876@evil.com/docs",
		"https://user:anypass@evil.com/docs",
		"https://docs.example.com/guide#tok_secret_9876",
		"https://docs.example.com/guide#starfall_cn",
		"https://starfall_cn.evil.com/",
		"https://docs.example.com/?cb=https%3A%2F%2Fpay.internal.example%2Fcb",
		"https://docs.example.com/?rcb=https%3A%2F%2Frefund.internal.example%2Fcb",
	} {
		require.Error(t, checkWebURLPrivacy(u, long, short), "expected blocked URL: %s", u)
	}

	for _, q := range []string{
		"tok_secret_9876 error",
		"星辉科技 实名认证",
		"苍穹工作室 渠道",
		"starfall_cn 充值",
		"https://pay.internal.example/cb timeout",
		"https://refund.internal.example/cb timeout",
	} {
		require.Error(t, checkWebSearchPrivacy(q, long, short), "expected blocked query: %s", q)
	}

	require.NoError(t, checkWebURLPrivacy("https://developer.apple.com/documentation/storekit#overview", long, short))
}
