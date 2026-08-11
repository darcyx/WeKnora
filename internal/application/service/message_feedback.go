package service

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

// SubmitMessageFeedback records a vote on a FAQ (numeric ID) or assistant message.
// FAQ votes validate the session owner and tenant before resolving the entry.
// Other IDs identify a request; resolve its assistant reply within the session.
func (s *messageService) SubmitMessageFeedback(
	ctx context.Context,
	sessionID string,
	messageID string,
	feedbackType string,
	reasons []string,
	reasonText string,
) (*types.Message, error) {
	if isFAQFeedbackID(messageID) {
		entryID, err := strconv.ParseInt(messageID, 10, 64)
		if err != nil || entryID <= 0 {
			return nil, apperrors.NewBadRequestError("invalid FAQ entry ID")
		}
		tenantID := types.MustTenantIDFromContext(ctx)
		userID := types.SessionOwnerIDFromContext(ctx)
		if userID == "" {
			return nil, apperrors.NewForbiddenError("feedback requires a user identity")
		}
		if _, err := s.sessionRepo.Get(ctx, tenantID, userID, sessionID); err != nil {
			return nil, err
		}
		chunk, err := s.chunkRepo.GetChunkBySeqID(ctx, tenantID, entryID)
		if err != nil {
			return nil, err
		}
		if chunk == nil || chunk.TenantID != tenantID || chunk.ChunkType != types.ChunkTypeFAQ {
			return nil, apperrors.NewNotFoundError("FAQ entry not found")
		}
		entry, err := s.knowService.GetFAQEntry(ctx, chunk.KnowledgeBaseID, entryID)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			return nil, apperrors.NewNotFoundError("FAQ entry not found")
		}
		feedback, err := buildMessageFeedback(feedbackType, reasons, reasonText)
		if err != nil {
			return nil, err
		}
		if err := s.messageRepo.CreateFAQFeedback(ctx, &types.FAQFeedback{
			TenantID: tenantID, SessionID: sessionID, UserID: userID, EntryID: entryID, Feedback: feedback,
			ChunkID:           entry.ChunkID,
			KnowledgeID:       entry.KnowledgeID,
			KnowledgeBaseID:   entry.KnowledgeBaseID,
			TagID:             entry.TagID,
			TagName:           entry.TagName,
			StandardQuestion:  entry.StandardQuestion,
			SimilarQuestions:  entry.SimilarQuestions,
			NegativeQuestions: entry.NegativeQuestions,
			Answers:           entry.Answers,
			AnswerStrategy:    entry.AnswerStrategy,
		}); err != nil {
			return nil, err
		}
		return &types.Message{Feedback: feedback}, nil
	}
	message, err := s.getAssistantMessageForFeedback(ctx, sessionID, messageID)
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

// getAssistantMessageForFeedback checks ownership before looking up a request's
// assistant reply. Feedback is a write operation, so no admin read fallback.
func (s *messageService) getAssistantMessageForFeedback(ctx context.Context, sessionID, requestID string) (*types.Message, error) {
	tenantID := types.MustTenantIDFromContext(ctx)
	if _, err := s.sessionRepo.Get(ctx, tenantID, sessionUserIDForLookup(ctx), sessionID); err != nil {
		return nil, err
	}
	return s.messageRepo.GetAssistantMessageByRequestID(ctx, sessionID, requestID)
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

// Only ASCII decimal digits select FAQ feedback. Overflow remains a FAQ error.
func isFAQFeedbackID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
