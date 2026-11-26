package web_search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	webfetch "github.com/Tencent/WeKnora/internal/infrastructure/web_fetch"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

const (
	// defaultTavilySearchURL is the hardcoded Tavily API URL.
	// Not configurable by tenants — prevents SSRF.
	defaultTavilySearchURL  = "https://api.tavily.com/search"
	defaultTavilyExtractURL = "https://api.tavily.com/extract"
)

// tavilyMaxResults is the largest max_results value the Tavily search API accepts.
const tavilyMaxResults = 20

var (
	defaultTavilyTimeout = 15 * time.Second
)

// TavilyProvider implements web search using Tavily Search API
type TavilyProvider struct {
	client      *http.Client
	baseURL     string
	apiKey      string
	extractURL  string
	checkTarget func(string) error
}

// NewTavilyProvider creates a new Tavily provider from parameters (no environment variables).
func NewTavilyProvider(params types.WebSearchProviderParameters) (interfaces.WebSearcher, error) {
	if params.APIKey == "" {
		return nil, fmt.Errorf("API key is required for Tavily provider")
	}
	client, err := NewSearchHTTPClient(defaultTavilyTimeout, params.ProxyURL)
	if err != nil {
		return nil, err
	}
	return &TavilyProvider{
		client:     client,
		baseURL:    defaultTavilySearchURL,
		apiKey:     params.APIKey,
		extractURL: defaultTavilyExtractURL,
	}, nil
}

func NewTavilyFetch(params types.WebSearchProviderParameters) (interfaces.WebFetcher, error) {
	provider, err := NewTavilyProvider(params)
	if err != nil {
		return nil, err
	}
	return provider.(*TavilyProvider), nil
}

func (p *TavilyProvider) Fetch(ctx context.Context, pageURL string) (*types.WebPage, error) {
	pageURL = strings.TrimSpace(pageURL)
	if _, err := validateDeepSeekFetchURL(pageURL); err != nil {
		return nil, err
	}
	checkTarget := p.checkTarget
	if checkTarget == nil {
		checkTarget = utils.ValidateURLForSSRF
	}
	if err := checkTarget(pageURL); err != nil {
		return nil, &webfetch.FetchError{Code: webfetch.ErrorSSRFRejected, Err: fmt.Errorf("URL rejected: %w", err)}
	}
	body, err := json.Marshal(map[string]any{"urls": []string{pageURL}, "format": "markdown"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.extractURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		code, retryable := webfetch.ErrorHTTPStatus, false
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			code = webfetch.ErrorProviderAuth
		case resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == 432 || resp.StatusCode == 433:
			code = webfetch.ErrorProviderFetch
		case resp.StatusCode == http.StatusTooManyRequests:
			code, retryable = webfetch.ErrorHTTP429, true
		case resp.StatusCode >= 500:
			code, retryable = webfetch.ErrorHTTP5xx, true
		}
		return nil, &webfetch.FetchError{Code: code, Retryable: retryable, Err: fmt.Errorf("Tavily extract returned HTTP %d", resp.StatusCode)}
	}
	raw, err := readCapped(resp.Body, 0)
	if err != nil {
		return nil, err
	}
	var result struct {
		Results []struct {
			URL        string `json:"url"`
			RawContent string `json:"raw_content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	for _, page := range result.Results {
		if page.URL == pageURL && strings.TrimSpace(page.RawContent) != "" {
			return &types.WebPage{Content: page.RawContent, Source: p.Name()}, nil
		}
	}
	return nil, &webfetch.FetchError{Code: webfetch.ErrorProviderFetch, Retryable: true, Err: fmt.Errorf("Tavily did not return page content")}
}

// Name returns the provider name
func (p *TavilyProvider) Name() string {
	return "tavily"
}

// Search performs a web search using Tavily Search API
func (p *TavilyProvider) Search(
	ctx context.Context,
	request types.WebSearchRequest,
) ([]*types.WebSearchResult, error) {
	query, maxResults, includeDate := request.Query, request.MaxResults, request.IncludeDate
	if len(query) == 0 {
		return nil, fmt.Errorf("query is empty")
	}
	logger.Infof(ctx, "[WebSearch][Tavily] query=%q maxResults=%d url=%s", query, maxResults, p.baseURL)

	// Tavily accepts max_results up to 20; zero or negative asks for none.
	if maxResults <= 0 {
		maxResults = 5
	}
	maxResults = min(maxResults, tavilyMaxResults)

	reqBody := tavilySearchRequest{
		APIKey:     p.apiKey,
		Query:      query,
		MaxResults: maxResults,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.baseURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		preview := readErrorPreview(resp.Body)
		logger.Warnf(ctx, "[WebSearch][Tavily] API returned status %d: %s", resp.StatusCode, preview)
		return nil, fmt.Errorf("tavily API returned status %d: %s", resp.StatusCode, preview)
	}

	respBody, err := readCapped(resp.Body, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var respData tavilySearchResponse
	if err := json.Unmarshal(respBody, &respData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	results := make([]*types.WebSearchResult, 0, len(respData.Results))
	for _, item := range respData.Results {
		result := &types.WebSearchResult{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: item.Content,
			Source:  "tavily",
		}
		if includeDate && item.PublishedDate != "" {
			if t, err := time.Parse(time.RFC3339, item.PublishedDate); err == nil {
				result.PublishedAt = &t
			}
		}
		results = append(results, result)
	}
	logger.Infof(ctx, "[WebSearch][Tavily] returned %d results", len(results))
	return results, nil
}

// tavilySearchRequest defines the request body for Tavily search API
type tavilySearchRequest struct {
	APIKey     string `json:"api_key"`
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// tavilySearchResponse defines the response structure for Tavily search API
type tavilySearchResponse struct {
	Query   string `json:"query"`
	Results []struct {
		Title         string  `json:"title"`
		URL           string  `json:"url"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"published_date,omitempty"`
	} `json:"results"`
}
