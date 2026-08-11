package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubmitMessageFeedbackPostsExpectedRequest(t *testing.T) {
	var capturedBody SubmitMessageFeedbackRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/messages/sess-1/msg-1/feedback" {
			t.Fatalf("path = %s, want /api/v1/messages/sess-1/msg-1/feedback", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &capturedBody); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"type":        "dislike",
				"reasons":     []string{"inaccurate", "other"},
				"reason_text": "missed the point",
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKey("sk-test"))
	feedback, err := c.SubmitMessageFeedback(context.Background(), "sess-1", "msg-1", &SubmitMessageFeedbackRequest{
		Type:       MessageFeedbackDislike,
		Reasons:    []string{MessageFeedbackReasonInaccurate, MessageFeedbackReasonOther},
		ReasonText: "missed the point",
	})
	if err != nil {
		t.Fatalf("SubmitMessageFeedback() error = %v", err)
	}

	if capturedBody.Type != MessageFeedbackDislike {
		t.Fatalf("request type sent = %q, want dislike", capturedBody.Type)
	}
	if len(capturedBody.Reasons) != 2 {
		t.Fatalf("request reasons sent = %v, want 2 entries", capturedBody.Reasons)
	}

	if feedback.Type != MessageFeedbackDislike {
		t.Fatalf("response Type = %q, want dislike", feedback.Type)
	}
	if feedback.ReasonText != "missed the point" {
		t.Fatalf("response ReasonText = %q, want %q", feedback.ReasonText, "missed the point")
	}
}

func TestSubmitMessageFeedbackAlreadySubmittedReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error": map[string]interface{}{
				"code":    1005,
				"message": "message feedback already submitted",
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKey("sk-test"))
	_, err := c.SubmitMessageFeedback(context.Background(), "sess-1", "msg-1", &SubmitMessageFeedbackRequest{
		Type: MessageFeedbackLike,
	})
	if err == nil {
		t.Fatal("expected an error for a repeated vote, got nil")
	}
}
