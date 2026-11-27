package types

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObservationRedactsEscapedTokenWithoutChangingPayload(t *testing.T) {
	secret := "synthetic-\"quote\\slash<&"
	raw, err := json.Marshal(map[string]any{"token": secret})
	require.NoError(t, err)
	ctx := WithObservationAppInfo(context.Background(), string(raw))
	payload := map[string]any{"content": "<app_info>" + string(raw) + "</app_info>", "argument": secret}
	original, err := json.Marshal(payload)
	require.NoError(t, err)
	wrapped, err := json.Marshal(map[string]string{"nested": string(original)})
	require.NoError(t, err)
	cleaned := SanitizeObservationText(ctx, string(wrapped))
	escaped, _ := json.Marshal(secret)
	require.NotContains(t, cleaned, string(escaped[1:len(escaped)-1]))
	require.NotContains(t, cleaned, secret)
	require.Contains(t, cleaned, "[REDACTED]")
	require.Equal(t, secret, payload["argument"])
	sanitized := SanitizeObservationValue(ctx, payload)
	bytes, err := json.Marshal(sanitized)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(bytes), secret))
}

func TestObservationRedactionDoesNotCorruptNonStringJSON(t *testing.T) {
	ctx := WithObservationAppInfo(context.Background(), `{"token":"1"}`)
	got := SanitizeObservationValue(ctx, map[string]any{"number": 1, "flag": true, "text": "token=1"}).(map[string]any)
	require.Equal(t, json.Number("1"), got["number"])
	require.Equal(t, true, got["flag"])
	require.Equal(t, "token=[REDACTED]", got["text"])
}
