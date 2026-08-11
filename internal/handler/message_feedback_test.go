package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// stubFeedbackMessageService satisfies just SubmitMessageFeedback, mirroring
// stubMessageService's embedding trick in message_session_not_found_test.go.
type stubFeedbackMessageService struct {
	interfaces.MessageService
	submit func(ctx context.Context, sessionID, messageID, feedbackType string, reasons []string, reasonText string) (*types.Message, error)
}

func (s *stubFeedbackMessageService) SubmitMessageFeedback(
	ctx context.Context, sessionID, messageID, feedbackType string, reasons []string, reasonText string,
) (*types.Message, error) {
	return s.submit(ctx, sessionID, messageID, feedbackType, reasons, reasonText)
}

func newFeedbackTestRouter(svc interfaces.MessageService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	h := &MessageHandler{MessageService: svc}
	r.POST("/messages/:session_id/:id/feedback", h.SubmitMessageFeedback)
	return r
}

func TestSubmitMessageFeedbackSuccessReturnsFeedback(t *testing.T) {
	svc := &stubFeedbackMessageService{
		submit: func(_ context.Context, sessionID, messageID, feedbackType string, reasons []string, reasonText string) (*types.Message, error) {
			if sessionID != "sess1" || messageID != "msg1" {
				t.Fatalf("unexpected ids: session=%q message=%q", sessionID, messageID)
			}
			if feedbackType != "dislike" || len(reasons) != 1 || reasons[0] != types.MessageFeedbackReasonOther || reasonText != "wrong" {
				t.Fatalf("unexpected request forwarded: type=%q reasons=%v text=%q", feedbackType, reasons, reasonText)
			}
			return &types.Message{
				ID: messageID,
				Feedback: &types.MessageFeedback{
					Type:       types.MessageFeedbackDislike,
					Reasons:    reasons,
					ReasonText: reasonText,
				},
			}, nil
		},
	}
	body := `{"type":"dislike","reasons":["other"],"reason_text":"wrong"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/sess1/msg1/feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	newFeedbackTestRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"reason_text":"wrong"`) {
		t.Fatalf("response body missing feedback payload: %s", w.Body.String())
	}
}

func TestSubmitMessageFeedbackMapsSessionNotFoundTo404(t *testing.T) {
	svc := &stubFeedbackMessageService{
		submit: func(context.Context, string, string, string, []string, string) (*types.Message, error) {
			return nil, apperrors.ErrSessionNotFound
		},
	}
	body := `{"type":"like"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/sess1/msg1/feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	newFeedbackTestRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", w.Code, w.Body.String())
	}
}

func TestSubmitMessageFeedbackMapsUnknownMessageTo404(t *testing.T) {
	svc := &stubFeedbackMessageService{
		submit: func(context.Context, string, string, string, []string, string) (*types.Message, error) {
			return nil, gorm.ErrRecordNotFound
		},
	}
	body := `{"type":"like"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/sess1/msg1/feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	newFeedbackTestRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", w.Code, w.Body.String())
	}
}

// The one-shot rule (confirmed product decision: feedback cannot be
// resubmitted or edited) must surface as 409, not a generic 500.
func TestSubmitMessageFeedbackMapsAlreadySubmittedTo409(t *testing.T) {
	svc := &stubFeedbackMessageService{
		submit: func(context.Context, string, string, string, []string, string) (*types.Message, error) {
			return nil, apperrors.ErrMessageFeedbackAlreadySubmitted
		},
	}
	body := `{"type":"like"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/sess1/msg1/feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	newFeedbackTestRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%s", w.Code, w.Body.String())
	}
}

// Validation errors (bad type, missing reasons, missing reason_text) come
// back from the service as *errors.AppError and must pass through as-is
// (400), not get swallowed into a generic 500.
func TestSubmitMessageFeedbackMapsValidationErrorTo400(t *testing.T) {
	svc := &stubFeedbackMessageService{
		submit: func(context.Context, string, string, string, []string, string) (*types.Message, error) {
			return nil, apperrors.NewBadRequestError("reasons is required for a dislike vote")
		},
	}
	body := `{"type":"dislike"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/sess1/msg1/feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	newFeedbackTestRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestSubmitMessageFeedbackRejectsMissingType(t *testing.T) {
	svc := &stubFeedbackMessageService{
		submit: func(context.Context, string, string, string, []string, string) (*types.Message, error) {
			t.Fatal("service must not be called when the body fails binding")
			return nil, nil
		},
	}
	body := `{}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/sess1/msg1/feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	newFeedbackTestRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}
