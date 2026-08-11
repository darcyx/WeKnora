package handler

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

type feedbackAdminService interface {
	List(context.Context, types.FeedbackQuery) (*types.FeedbackPage, error)
	Summary(context.Context, types.FeedbackQuery) (*types.FAQFeedbackSummaryPage, error)
	Detail(context.Context, string, string, string, string) (*types.FeedbackRecord, error)
	Export(context.Context, types.FeedbackQuery) ([]*types.FeedbackRecord, error)
}
type FeedbackAdminHandler struct{ service feedbackAdminService }

func NewFeedbackAdminHandler(s *service.FeedbackAdminService) *FeedbackAdminHandler {
	return &FeedbackAdminHandler{service: s}
}
func feedbackQuery(c *gin.Context) (types.FeedbackQuery, error) {
	q := types.FeedbackQuery{Source: c.Query("source"), Vote: c.Query("type"), Search: c.Query("q"), SessionID: c.Query("session_id"), TargetID: c.Query("target_id"), UserID: c.Query("user_id"), KnowledgeBaseID: c.Query("knowledge_base_id"), TagName: c.Query("tag_name"), Sort: c.Query("sort")}
	if reasons := c.Query("reasons"); reasons != "" {
		q.Reasons = strings.Split(reasons, ",")
	}
	var err error
	q.Page, err = strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil {
		return q, apperrors.NewBadRequestError("invalid page")
	}
	q.PageSize, err = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil {
		return q, apperrors.NewBadRequestError("invalid page size")
	}
	if v := c.Query("from"); v != "" {
		q.From, err = time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return q, apperrors.NewBadRequestError("invalid from date")
		}
	}
	if v := c.Query("to"); v != "" {
		q.To, err = time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return q, apperrors.NewBadRequestError("invalid to date")
		}
	}
	return q, nil
}
func feedbackResponse(c *gin.Context, data any, err error) {
	if err != nil {
		if app, ok := apperrors.IsAppError(err); ok {
			c.Error(app)
		} else {
			c.Error(apperrors.NewInternalServerError("feedback query failed"))
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}
func (h *FeedbackAdminHandler) List(c *gin.Context) {
	q, err := feedbackQuery(c)
	if err != nil {
		feedbackResponse(c, nil, err)
		return
	}
	data, err := h.service.List(c.Request.Context(), q)
	feedbackResponse(c, data, err)
}
func (h *FeedbackAdminHandler) Summary(c *gin.Context) {
	q, err := feedbackQuery(c)
	if err != nil {
		feedbackResponse(c, nil, err)
		return
	}
	data, err := h.service.Summary(c.Request.Context(), q)
	feedbackResponse(c, data, err)
}
func (h *FeedbackAdminHandler) Detail(c *gin.Context) {
	data, err := h.service.Detail(c.Request.Context(), c.Param("source"), c.Param("session_id"), c.Param("target_id"), c.Query("message_id"))
	feedbackResponse(c, data, err)
}

// Neutralize spreadsheet formulas even when preceded by whitespace/control bytes.
func feedbackCSVCell(value string) string {
	trimmed := strings.TrimLeftFunc(value, func(r rune) bool { return r <= 32 || r == '\ufeff' })
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") {
		return "'" + value
	}
	return value
}
func (h *FeedbackAdminHandler) Export(c *gin.Context) {
	q, err := feedbackQuery(c)
	if err != nil {
		feedbackResponse(c, nil, err)
		return
	}
	rows, err := h.service.Export(c.Request.Context(), q)
	if err != nil {
		feedbackResponse(c, nil, err)
		return
	}
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf")
	writer := csv.NewWriter(&buf)
	writer.Write([]string{"source", "session_id", "target_id", "message_id", "user_id", "user_label", "question", "answers", "knowledge_base_id", "knowledge_base_name", "tag_name", "type", "reasons", "reason_text", "feedback_at"})
	for _, r := range rows {
		row := []string{r.Source, r.SessionID, r.TargetID, r.MessageID, r.UserID, r.UserLabel, r.Question, strings.Join(r.Answers, "\n\n"), r.KnowledgeBaseID, r.KnowledgeBaseName, r.TagName, string(r.Feedback.Type), strings.Join(r.Feedback.Reasons, ","), r.Feedback.ReasonText, r.Feedback.CreatedAt.Format(time.RFC3339Nano)}
		for i := range row {
			row[i] = feedbackCSVCell(row[i])
		}
		writer.Write(row)
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		feedbackResponse(c, nil, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="feedback.csv"`)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}
