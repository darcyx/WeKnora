package service

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestBuildMessageFeedbackLikeIgnoresReasons(t *testing.T) {
	feedback, err := buildMessageFeedback("like", []string{types.MessageFeedbackReasonOther}, "should be ignored")
	if err != nil {
		t.Fatalf("buildMessageFeedback() error = %v", err)
	}
	if feedback.Type != types.MessageFeedbackLike {
		t.Fatalf("Type = %q, want like", feedback.Type)
	}
	if len(feedback.Reasons) != 0 || feedback.ReasonText != "" {
		t.Fatalf("like vote must ignore reasons, got %#v", feedback)
	}
}

func TestBuildMessageFeedbackDislikeRequiresReasons(t *testing.T) {
	_, err := buildMessageFeedback("dislike", nil, "")
	assertBadRequest(t, err, "reasons is required")
}

func TestBuildMessageFeedbackDislikeRejectsUnknownReason(t *testing.T) {
	_, err := buildMessageFeedback("dislike", []string{"made_up_reason"}, "")
	assertBadRequest(t, err, "invalid feedback reason")
}

func TestBuildMessageFeedbackDislikeMultiSelectDeduplicates(t *testing.T) {
	feedback, err := buildMessageFeedback("dislike", []string{
		types.MessageFeedbackReasonInaccurate,
		types.MessageFeedbackReasonIncomplete,
		types.MessageFeedbackReasonInaccurate,
	}, "")
	if err != nil {
		t.Fatalf("buildMessageFeedback() error = %v", err)
	}
	if len(feedback.Reasons) != 2 {
		t.Fatalf("Reasons = %#v, want 2 deduplicated entries", feedback.Reasons)
	}
}

func TestBuildMessageFeedbackOtherRequiresReasonText(t *testing.T) {
	_, err := buildMessageFeedback("dislike", []string{types.MessageFeedbackReasonOther}, "  ")
	assertBadRequest(t, err, `reason_text is required when "other" is selected`)
}

func TestBuildMessageFeedbackOtherWithTextSucceeds(t *testing.T) {
	feedback, err := buildMessageFeedback(
		"dislike",
		[]string{types.MessageFeedbackReasonInaccurate, types.MessageFeedbackReasonOther},
		"  the citation was wrong  ",
	)
	if err != nil {
		t.Fatalf("buildMessageFeedback() error = %v", err)
	}
	if feedback.ReasonText != "the citation was wrong" {
		t.Fatalf("ReasonText = %q, want trimmed text", feedback.ReasonText)
	}
}

// A reason_text without "other" selected is stray input, not an error: it is
// silently dropped so the API stays forgiving of clients that always send
// the field.
func TestBuildMessageFeedbackTextWithoutOtherIsDropped(t *testing.T) {
	feedback, err := buildMessageFeedback("dislike", []string{types.MessageFeedbackReasonOffTopic}, "unused text")
	if err != nil {
		t.Fatalf("buildMessageFeedback() error = %v", err)
	}
	if feedback.ReasonText != "" {
		t.Fatalf("ReasonText = %q, want empty when \"other\" is not selected", feedback.ReasonText)
	}
}

func TestBuildMessageFeedbackOtherTextTooLong(t *testing.T) {
	longText := strings.Repeat("a", types.MessageFeedbackReasonTextMaxRunes+1)
	_, err := buildMessageFeedback("dislike", []string{types.MessageFeedbackReasonOther}, longText)
	assertBadRequest(t, err, "exceeds the maximum length")
}

func TestBuildMessageFeedbackRejectsUnknownType(t *testing.T) {
	_, err := buildMessageFeedback("neutral", nil, "")
	assertBadRequest(t, err, `feedback type must be "like" or "dislike"`)
}

func assertBadRequest(t *testing.T, err error, wantSubstring string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	appErr, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("error = %v (%T), want *errors.AppError", err, err)
	}
	if appErr.Code != errors.ErrValidation && appErr.Code != errors.ErrBadRequest {
		t.Fatalf("AppError code = %v, want a bad-request/validation code", appErr.Code)
	}
	if !strings.Contains(appErr.Message, wantSubstring) {
		t.Fatalf("message = %q, want it to contain %q", appErr.Message, wantSubstring)
	}
}
