package web_search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	webfetch "github.com/Tencent/WeKnora/internal/infrastructure/web_fetch"
	"github.com/stretchr/testify/require"
)

func TestTavilyFetchValidatesTargetAndRequiresMatchingContent(t *testing.T) {
	const target = "https://example.com/page?hl=en"
	response := `{"results":[{"url":"https://example.com/page?hl=en","raw_content":"full page"}]}`
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		var request struct {
			URLs []string `json:"urls"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, []string{target}, request.URLs)
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()
	provider := &TavilyProvider{client: server.Client(), extractURL: server.URL, apiKey: "test-key", checkTarget: func(string) error { return nil }}
	page, err := provider.Fetch(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, "full page", page.Content)

	response = `{"results":[],"failed_results":[{"url":"https://example.com/page?hl=en","error":"failed"}]}`
	_, err = provider.Fetch(context.Background(), target)
	require.Error(t, err)
	code, _, _ := webfetch.ErrorDetails(err)
	require.Equal(t, webfetch.ErrorProviderFetch, code)
	require.True(t, providerSideFailure(err))

	response = `{"results":[{"url":"https://example.com/page?hl=zh","raw_content":"different page"}]}`
	_, err = provider.Fetch(context.Background(), target)
	require.Error(t, err)
	provider.checkTarget = func(string) error { return errors.New("restricted") }
	_, err = provider.Fetch(context.Background(), target)
	require.ErrorContains(t, err, "restricted")
	provider.checkTarget = nil
	_, err = provider.Fetch(context.Background(), "http://127.0.0.1/private")
	require.Error(t, err)
	code, _, _ = webfetch.ErrorDetails(err)
	require.Equal(t, webfetch.ErrorSSRFRejected, code)
	require.Equal(t, 3, calls)

	// HTTP 432/433 provider quota/plan errors map to ErrorProviderFetch so WithLocalFallback can recover locally.
	for _, status := range []int{402, 432, 433} {
		errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "plan limit", status)
		}))
		p := &TavilyProvider{client: errServer.Client(), extractURL: errServer.URL, apiKey: "test-key", checkTarget: func(string) error { return nil }}
		_, err = p.Fetch(context.Background(), target)
		errServer.Close()
		require.Error(t, err)
		code, _, _ = webfetch.ErrorDetails(err)
		require.Equal(t, webfetch.ErrorProviderFetch, code, "status %d should map to ErrorProviderFetch", status)
		require.True(t, providerSideFailure(err), "status %d should trigger local fallback", status)
	}
}
