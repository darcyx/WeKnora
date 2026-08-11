package router

import (
	"net/http"
	"testing"

	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFeedbackAdminRoutesRequireFullAPIKeyAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g := &rbacGuards{}
	r := gin.New()
	RegisterFeedbackRoutes(r.Group("/api/v1"), &handler.FeedbackAdminHandler{}, g)
	for _, path := range []string{"/api/v1/feedback", "/api/v1/feedback/faq-summary", "/api/v1/feedback/export", "/api/v1/feedback/:source/:session_id/:target_id"} {
		policy := mustLookupAPIKeyPolicy(t, g, http.MethodGet, path)
		require.True(t, policy.RequireFullAccess)
	}
}
