package router

import (
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/gin-gonic/gin"
)

func RegisterFeedbackRoutes(r *gin.RouterGroup, h *handler.FeedbackAdminHandler, g *rbacGuards) {
	feedback := g.apiKeyGroup(r.Group("/feedback", g.Admin()), apiKeyFullAccess())
	feedback.GET("", h.List)
	feedback.GET("/faq-summary", h.Summary)
	feedback.GET("/export", h.Export)
	feedback.GET("/:source/:session_id/:target_id", h.Detail)
}
