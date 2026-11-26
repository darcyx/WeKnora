package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeAppInfoJSONAndText(t *testing.T) {
	raw := `{"app_id":"10001","token":"secret-top","passwd":"p1","db_pwd":"p2","credential":"c1","private_key":"pk","jwt":"j1","session_id":"s1","token_count":3,"max_tokens":10,"nested":{"userPwd":"up","secretKey":"sk","name":"ok"}}`
	got := SanitizeAppInfoJSON(raw)
	require.JSONEq(t, `{"app_id":"10001","token_count":3,"max_tokens":10,"nested":{"name":"ok"}}`, got)

	wrapped := `<runtime_context>
  <app_info type="static_project_metadata" live_config_verified="false">{"app_id":"10001","token":"leaked-tok","jwt":"leaked-jwt"}</app_info>
</runtime_context>`
	cleaned := SanitizeAppInfoInText(wrapped)
	require.NotContains(t, cleaned, "leaked-tok")
	require.NotContains(t, cleaned, "leaked-jwt")
	require.Contains(t, cleaned, `"app_id":"10001"`)
}

func TestLiveAppInfoRetainsOnlyTopLevelToken(t *testing.T) {
	raw := `{"token":"live-value","api_key":"key","game":{"token":"nested","name":"game"}}`
	live := SanitizeLiveAppInfoJSON(raw)
	require.JSONEq(t, `{"token":"live-value","game":{"name":"game"}}`, live)
	require.JSONEq(t, `{"game":{"name":"game"}}`, SanitizeAppInfoJSON(live))
	require.NotContains(t, SanitizeAppInfoInText("<app_info>"+live+"</app_info>"), "live-value")
	require.JSONEq(t, `{"token":{"name":"visible"}}`, SanitizeLiveAppInfoJSON(`{"token":{"name":"visible","password":"hidden"}}`))
	require.JSONEq(t, `[{"name":"visible"}]`, SanitizeLiveAppInfoJSON(`[{"name":"visible","token":"hidden"}]`))
}
