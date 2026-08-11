package repository

import (
	"context"
	stderrors "errors"
	"testing"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newMessageFeedbackDB(t *testing.T, name string) (*gorm.DB, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&types.Session{}, &types.Message{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	session := &types.Session{TenantID: 1, UserID: "web_user:alice", Title: "test session"}
	if err := db.Create(session).Error; err != nil {
		t.Fatalf("insert session: %v", err)
	}
	message := &types.Message{SessionID: session.ID, Role: "assistant", Content: "hi"}
	if err := db.Create(message).Error; err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return db, message.ID
}

func TestUpdateMessageFeedbackRecordsFirstVote(t *testing.T) {
	db, messageID := newMessageFeedbackDB(t, "feedback-first-vote")
	repo := NewMessageRepository(db)

	var session types.Session
	if err := db.First(&session).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}

	err := repo.UpdateMessageFeedback(context.Background(), session.ID, messageID, types.MessageFeedback{
		Type: types.MessageFeedbackDislike,
		Reasons: []string{
			types.MessageFeedbackReasonInaccurate,
		},
	})
	if err != nil {
		t.Fatalf("UpdateMessageFeedback() error = %v", err)
	}

	var stored types.Message
	if err := db.First(&stored, "id = ?", messageID).Error; err != nil {
		t.Fatalf("reload message: %v", err)
	}
	if stored.Feedback == nil {
		t.Fatal("feedback was not persisted")
	}
	if stored.Feedback.Type != types.MessageFeedbackDislike {
		t.Fatalf("feedback type = %q, want dislike", stored.Feedback.Type)
	}
}

// Feedback is one-shot per the product decision: a second vote on the same
// message must be rejected rather than overwriting the first.
func TestUpdateMessageFeedbackRejectsSecondVote(t *testing.T) {
	db, messageID := newMessageFeedbackDB(t, "feedback-second-vote")
	repo := NewMessageRepository(db)

	var session types.Session
	if err := db.First(&session).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}

	first := types.MessageFeedback{Type: types.MessageFeedbackLike}
	if err := repo.UpdateMessageFeedback(context.Background(), session.ID, messageID, first); err != nil {
		t.Fatalf("first vote: unexpected error = %v", err)
	}

	second := types.MessageFeedback{Type: types.MessageFeedbackDislike, Reasons: []string{types.MessageFeedbackReasonOffTopic}}
	err := repo.UpdateMessageFeedback(context.Background(), session.ID, messageID, second)
	if !stderrors.Is(err, apperrors.ErrMessageFeedbackAlreadySubmitted) {
		t.Fatalf("second vote error = %v, want ErrMessageFeedbackAlreadySubmitted", err)
	}

	var stored types.Message
	if err := db.First(&stored, "id = ?", messageID).Error; err != nil {
		t.Fatalf("reload message: %v", err)
	}
	if stored.Feedback.Type != types.MessageFeedbackLike {
		t.Fatalf("feedback was overwritten: got %q, want the original like vote", stored.Feedback.Type)
	}
}

func TestUpdateMessageFeedbackUnknownMessageReturnsNotFound(t *testing.T) {
	db, _ := newMessageFeedbackDB(t, "feedback-unknown-message")
	repo := NewMessageRepository(db)

	var session types.Session
	if err := db.First(&session).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}

	err := repo.UpdateMessageFeedback(context.Background(), session.ID, "does-not-exist", types.MessageFeedback{
		Type: types.MessageFeedbackLike,
	})
	if !stderrors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("error = %v, want gorm.ErrRecordNotFound", err)
	}
}
