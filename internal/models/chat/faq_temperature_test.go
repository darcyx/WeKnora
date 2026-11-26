package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/api/openaicompletions"
	"github.com/stretchr/testify/require"
)

type captureFAQTransport func(*http.Request) (*http.Response, error)

func (f captureFAQTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestIsolatedTemperatureOnWire(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "8.8.8.8")
	var body map[string]any
	transport := captureFAQTransport(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))}, nil
	})
	for _, supported := range []bool{true, false} {
		settings := api.DefaultOpenAICompletions()
		settings.SupportsTemperature = supported
		settings.ExtraBody = map[string]any{"temperature": 0.7}
		var model Chat = openaicompletions.New(openaicompletions.Config{Endpoint: api.Endpoint{BaseURL: "https://8.8.8.8", Model: "m", Client: &http.Client{Transport: transport}}, Settings: settings})
		body = nil
		_, err := model.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, &ChatOptions{Isolated: true, Temperature: 0})
		require.NoError(t, err)
		value, exists := body["temperature"]
		require.Equal(t, supported, exists)
		if supported {
			require.Equal(t, float64(0), value)
		}
	}
}
