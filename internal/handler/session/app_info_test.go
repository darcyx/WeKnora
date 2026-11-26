package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarshalAppInfoDropsCredentialFields(t *testing.T) {
	ext := map[string]interface{}{
		"platform":      "h5",
		"token_count":   5,
		"max_tokens":    42,
		"refresh_token": "r",
		"bearerToken":   "b",
		"idToken":       "i",
		"token_value":   "v",
		"token":         "backend-token",
		"gameInfo": map[string]interface{}{
			"appId":       "123",
			"token":       "eyJhbGciOi.secret",
			"accessToken": "a",
			"servers":     []interface{}{map[string]interface{}{"name": "s1", "password": "p"}},
		},
	}
	got, err := marshalAppInfo(ext)
	require.NoError(t, err)
	require.JSONEq(t, `{"platform":"h5","token":"backend-token","token_count":5,"max_tokens":42,"gameInfo":{"appId":"123","servers":[{"name":"s1"}]}}`, got)
	require.Contains(t, ext["gameInfo"], "token", "input must not be mutated")
}
