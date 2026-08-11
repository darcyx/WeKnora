package repository

import (
	"context"
	"os"
	"testing"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestFAQFeedbackPersistenceAndSessionScope(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	migration, err := os.ReadFile("../../../migrations/sqlite/999003_faq_feedback.up.sql")
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(migration)).Error)
	repo := NewMessageRepository(db)
	ctx := context.Background()
	f := types.FAQFeedback{TenantID: 1, SessionID: "session-1", UserID: "alice", EntryID: 123, Feedback: &types.MessageFeedback{Type: types.MessageFeedbackDislike, Reasons: []string{"other"}, ReasonText: "incorrect", CreatedAt: time.Now().UTC()}}
	f.KnowledgeBaseID = "kb"
	f.StandardQuestion = "如何办理？"
	f.Answers = []string{"在线办理", "柜台办理"}
	f.SimilarQuestions = []string{"办理方式"}
	f.TagName = "业务办理"
	require.NoError(t, repo.CreateFAQFeedback(ctx, &f))
	duplicate := f
	duplicate.StandardQuestion = "changed"
	duplicate.Answers = []string{"changed"}
	duplicate.Feedback = &types.MessageFeedback{Type: types.MessageFeedbackLike, CreatedAt: time.Now()}
	require.ErrorIs(t, repo.CreateFAQFeedback(ctx, &duplicate), apperrors.ErrMessageFeedbackAlreadySubmitted)
	var stored types.FAQFeedback
	require.NoError(t, db.First(&stored).Error)
	require.Equal(t, f.Feedback, stored.Feedback)
	require.Equal(t, f.ChunkID, stored.ChunkID)
	require.Equal(t, f.KnowledgeID, stored.KnowledgeID)
	require.Equal(t, f.KnowledgeBaseID, stored.KnowledgeBaseID)
	require.Equal(t, f.TagID, stored.TagID)
	require.Equal(t, f.TagName, stored.TagName)
	require.Equal(t, f.StandardQuestion, stored.StandardQuestion)
	require.Equal(t, f.SimilarQuestions, stored.SimilarQuestions)
	require.Equal(t, f.NegativeQuestions, stored.NegativeQuestions)
	require.Equal(t, f.Answers, stored.Answers)
	require.Equal(t, f.AnswerStrategy, stored.AnswerStrategy)
	require.Equal(t, "alice", stored.UserID)
	nextSession := f
	nextSession.SessionID = "session-2"
	require.NoError(t, repo.CreateFAQFeedback(ctx, &nextSession))
	nextEntry := f
	nextEntry.EntryID = 124
	require.NoError(t, repo.CreateFAQFeedback(ctx, &nextEntry))
	nextTenant := f
	nextTenant.TenantID = 2
	require.NoError(t, repo.CreateFAQFeedback(ctx, &nextTenant))
	var count int64
	require.NoError(t, db.Model(&types.FAQFeedback{}).Count(&count).Error)
	require.Equal(t, int64(4), count)
	down, err := os.ReadFile("../../../migrations/sqlite/999003_faq_feedback.down.sql")
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(down)).Error)
	require.False(t, db.Migrator().HasTable("faq_feedbacks"))
}
