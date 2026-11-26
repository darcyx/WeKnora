package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFeedbackCSVNeutralizesFormulas(t *testing.T) {
	for _, value := range []string{"=SUM(A1:A2)", "  =1+1", "\t@cmd", "-1+2", "+cmd", "\rhello", "\nhello", "\ufeff=1"} {
		require.Equal(t, "'"+value, feedbackCSVCell(value))
	}
	require.Equal(t, "普通文本", feedbackCSVCell("普通文本"))
}

type exportFeedbackStub struct{ feedbackAdminService }

func (exportFeedbackStub) Export(context.Context, types.FeedbackQuery) ([]*types.FeedbackRecord, error) {
	return []*types.FeedbackRecord{{Source: "faq", Question: "=1+1", Answers: []string{"答案一", "答案二"}, Feedback: &types.MessageFeedback{Type: types.MessageFeedbackDislike, Reasons: []string{"other"}, ReasonText: "a,b\nnext", CreatedAt: time.Now()}}}, nil
}
func TestFeedbackAdminExportContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &FeedbackAdminHandler{service: exportFeedbackStub{}}
	r.GET("/feedback/export", h.Export)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/feedback/export", nil))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "text/csv")
	require.Contains(t, w.Body.String(), "'=1+1")
	require.Contains(t, w.Body.String(), "答案一\n\n答案二")
}
func TestFeedbackAdminParsesFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/feedback?source=faq&type=dislike&reasons=other,inaccurate&page=2&from=2026-09-01T00:00:00Z&to=2026-09-11T00:00:00Z", nil)
	q, err := feedbackQuery(c)
	require.NoError(t, err)
	require.Equal(t, "faq", q.Source)
	require.Equal(t, 2, q.Page)
	require.Len(t, q.Reasons, 2)
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/feedback?from=invalid", nil)
	_, err = feedbackQuery(c)
	require.Error(t, err)
}
