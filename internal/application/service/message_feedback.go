package service

import (
	"context"
	"slices"
	"strings"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

// SubmitMessageFeedback records a like/dislike vote on an assistant message.
// GetMessage both authorizes the caller (session ownership) and fetches the
// message the vote applies to, so a non-owner or wrong-tenant call surfaces
// the same ErrSessionNotFound every other message endpoint uses.
func (s *messageService) SubmitMessageFeedback(
	ctx context.Context,
	sessionID string,
	messageID string,
	feedbackType string,
	reasons []string,
	reasonText string,
) (*types.Message, error) {
	message, err := s.GetMessage(ctx, sessionID, messageID)
	if err != nil {
		return nil, err
	}
	if message.Role != "assistant" {
		return nil, apperrors.NewBadRequestError("feedback can only be submitted for assistant messages")
	}
	feedback, err := buildMessageFeedback(feedbackType, reasons, reasonText)
	if err != nil {
		return nil, err
	}
	if err := s.messageRepo.UpdateMessageFeedback(ctx, sessionID, messageID, *feedback); err != nil {
		return nil, err
	}
	message.Feedback = feedback
	return message, nil
}

// buildMessageFeedback validates and normalizes a feedback submission. It has
// no side effects, so it is unit-tested directly without a database.
func buildMessageFeedback(feedbackType string, reasons []string, reasonText string) (*types.MessageFeedback, error) {
	switch types.MessageFeedbackType(strings.TrimSpace(feedbackType)) {
	case types.MessageFeedbackLike:
		// A like vote carries no reasons; ignore anything the caller sent
		// rather than rejecting an otherwise-valid request over it.
		return &types.MessageFeedback{Type: types.MessageFeedbackLike, CreatedAt: time.Now()}, nil
	case types.MessageFeedbackDislike:
		normalizedReasons, err := normalizeFeedbackReasons(reasons)
		if err != nil {
			return nil, err
		}
		text, err := normalizeFeedbackReasonText(normalizedReasons, reasonText)
		if err != nil {
			return nil, err
		}
		return &types.MessageFeedback{
			Type:       types.MessageFeedbackDislike,
			Reasons:    normalizedReasons,
			ReasonText: text,
			CreatedAt:  time.Now(),
		}, nil
	default:
		return nil, apperrors.NewBadRequestError(`feedback type must be "like" or "dislike"`)
	}
}

// normalizeFeedbackReasons validates the multi-select reason codes for a
// dislike vote, deduplicating while preserving the caller's order.
func normalizeFeedbackReasons(reasons []string) ([]string, error) {
	if len(reasons) == 0 {
		return nil, apperrors.NewBadRequestError("reasons is required for a dislike vote")
	}
	seen := make(map[string]struct{}, len(reasons))
	normalized := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		reason = strings.TrimSpace(reason)
		if _, ok := types.ValidMessageFeedbackReasons[reason]; !ok {
			return nil, apperrors.NewBadRequestError("invalid feedback reason: " + reason)
		}
		if _, dup := seen[reason]; dup {
			continue
		}
		seen[reason] = struct{}{}
		normalized = append(normalized, reason)
	}
	return normalized, nil
}

// normalizeFeedbackReasonText enforces the "other" reason's free-text rule:
// required when "other" is selected, ignored (cleared) otherwise, and
// length-capped so a dislike vote can't smuggle in unbounded text.
func normalizeFeedbackReasonText(reasons []string, reasonText string) (string, error) {
	text := strings.TrimSpace(reasonText)
	if !slices.Contains(reasons, types.MessageFeedbackReasonOther) {
		return "", nil
	}
	if text == "" {
		return "", apperrors.NewBadRequestError(`reason_text is required when "other" is selected`)
	}
	if len([]rune(text)) > types.MessageFeedbackReasonTextMaxRunes {
		return "", apperrors.NewBadRequestError("reason_text exceeds the maximum length")
	}
	return text, nil
}
