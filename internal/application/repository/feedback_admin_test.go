package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func feedbackAdminFixture(t *testing.T) (*FeedbackRepository, types.FeedbackQuery) {
	t.Helper()
	var dialector gorm.Dialector = sqlite.Open(":memory:")
	dialect := "sqlite"
	if dsn := os.Getenv("WEKNORA_FEEDBACK_TEST_POSTGRES_DSN"); dsn != "" {
		dialector = postgres.Open(dsn)
		dialect = "versioned"
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if dialect == "versioned" {
		schema := fmt.Sprintf("feedback_test_%d", time.Now().UnixNano())
		require.NoError(t, db.Exec("CREATE SCHEMA "+schema).Error)
		require.NoError(t, db.Exec("SET search_path TO "+schema).Error)
		t.Cleanup(func() { db.Exec("DROP SCHEMA " + schema + " CASCADE") })
	}
	migration, err := os.ReadFile("../../../migrations/" + dialect + "/999003_faq_feedback.up.sql")
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(migration)).Error)
	for _, sql := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, tenant_id INTEGER, user_id TEXT, title TEXT, deleted_at DATETIME)`,
		`CREATE TABLE users (id TEXT PRIMARY KEY, username TEXT, deleted_at DATETIME)`,
		`CREATE TABLE knowledge_bases (id TEXT PRIMARY KEY, tenant_id INTEGER, name TEXT, deleted_at DATETIME)`,
		`CREATE TABLE messages (id TEXT PRIMARY KEY, session_id TEXT, request_id TEXT, role TEXT, content TEXT, knowledge_references TEXT, feedback TEXT, created_at DATETIME, deleted_at DATETIME)`,
		`INSERT INTO sessions VALUES ('s1',1,'alice','one',NULL),('s2',1,'bob','two',NULL),('s3',2,'eve','secret',NULL)`,
		`INSERT INTO users VALUES ('alice','Alice',NULL),('bob','Bob',NULL),('eve','Eve',NULL)`,
		`INSERT INTO knowledge_bases VALUES ('kb1',1,'Support',NULL)`,
	} {
		if dialect == "versioned" {
			sql = strings.ReplaceAll(sql, "DATETIME", "TIMESTAMPTZ")
		}
		require.NoError(t, db.Exec(sql).Error)
	}
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	feedback := func(vote string, offset int) *types.MessageFeedback {
		return &types.MessageFeedback{Type: types.MessageFeedbackType(vote), Reasons: []string{"other"}, ReasonText: "100% accurate?", CreatedAt: at.Add(time.Duration(offset) * time.Hour)}
	}
	f := NewMessageRepository(db)
	for _, v := range []struct {
		session        string
		entry          int64
		tenant         uint64
		question, vote string
		offset         int
	}{{"s1", 10, 1, "old FAQ", "dislike", 0}, {"s2", 10, 1, "new FAQ", "like", 1}, {"s1", 11, 1, "another FAQ", "dislike", 2}, {"s3", 10, 2, "secret FAQ", "dislike", 4}} {
		require.NoError(t, f.CreateFAQFeedback(context.Background(), &types.FAQFeedback{SessionID: v.session, EntryID: v.entry, TenantID: v.tenant, UserID: "alice", KnowledgeBaseID: "kb1", TagName: "Billing", StandardQuestion: v.question, Answers: []string{"first answer", "second answer"}, Feedback: feedback(v.vote, v.offset)}))
	}
	for _, v := range []struct {
		id, session, role, content, vote string
		offset                           int
	}{{"u1", "s1", "user", "real question", "", 0}, {"u2", "s2", "user", "second question", "", 0}, {"m1", "s1", "assistant", "first reply", "dislike", 0}, {"m2", "s2", "assistant", "second reply", "like", 3}, {"m3", "s1", "assistant", "third reply", "like", 2}, {"m4", "s3", "assistant", "secret reply", "like", 5}} {
		var payload any
		if v.vote != "" {
			b, err := json.Marshal(feedback(v.vote, v.offset))
			require.NoError(t, err)
			payload = string(b)
		}
		require.NoError(t, db.Exec("INSERT INTO messages (id,session_id,request_id,role,content,knowledge_references,feedback,created_at) VALUES (?,?,?,?,?,?,?,?)", v.id, v.session, "req1", v.role, v.content, `[{"id":"ref1","content":"citation"}]`, payload, at).Error)
	}
	return NewFeedbackRepository(db), types.FeedbackQuery{From: at.Add(-time.Hour), To: at.Add(6 * time.Hour), Page: 1, PageSize: 2}
}
func TestFeedbackAdminUnifiedPaginationAndDetail(t *testing.T) {
	r, q := feedbackAdminFixture(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		q.Page = page
		p, err := r.List(ctx, 1, q, false)
		require.NoError(t, err)
		require.Equal(t, int64(6), p.Stats.Total)
		require.Equal(t, int64(3), p.Stats.Dislikes)
		require.Len(t, p.Items, 2)
		for _, v := range p.Items {
			key := v.Source + v.SessionID + v.TargetID + v.MessageID
			require.False(t, seen[key])
			seen[key] = true
			require.NotContains(t, v.Question, "secret")
			require.Empty(t, v.Answers)
		}
	}
	rows, err := r.Detail(ctx, 1, "message", "s1", "req1", "m3")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "real question", rows[0].Question)
	require.Equal(t, []string{"third reply"}, rows[0].Answers)
	require.Len(t, rows[0].References, 1)
	rows, err = r.Detail(ctx, 1, "message", "s3", "req1", "m4")
	require.NoError(t, err)
	require.Empty(t, rows)
	rows, err = r.Detail(ctx, 1, "faq", "s1", "10", "")
	require.NoError(t, err)
	require.Equal(t, "old FAQ", rows[0].Question)
	require.Len(t, rows[0].Answers, 2)
}
func TestFeedbackAdminFiltersAndSummary(t *testing.T) {
	r, q := feedbackAdminFixture(t)
	ctx := context.Background()
	q.PageSize = 20
	summary, err := r.Summary(ctx, 1, q)
	require.NoError(t, err)
	require.Equal(t, int64(2), summary.Total)
	require.Equal(t, int64(3), summary.Stats.Total)
	for _, g := range summary.Items {
		if g.EntryID == "10" {
			require.Equal(t, "new FAQ", g.StandardQuestion)
			require.Equal(t, int64(2), g.Total)
		}
	}
	q.Source = "faq"
	q.Vote = "dislike"
	q.Reasons = []string{"other"}
	q.Search = "100%"
	q.KnowledgeBaseID = "kb1"
	q.TagName = "Billing"
	p, err := r.List(ctx, 1, q, false)
	require.NoError(t, err)
	require.Equal(t, int64(2), p.Stats.Total)
	q.Reasons = []string{"inaccurate"}
	p, err = r.List(ctx, 1, q, false)
	require.NoError(t, err)
	require.Zero(t, p.Stats.Total)
	q.Reasons = nil
	q.Search = "100_"
	p, err = r.List(ctx, 1, q, false)
	require.NoError(t, err)
	require.Zero(t, p.Stats.Total)
}

func TestFeedbackAdminExportLimitAndSoftDeletes(t *testing.T) {
	r, q := feedbackAdminFixture(t)
	ctx := context.Background()
	q.PageSize = 3
	p, err := r.List(ctx, 1, q, true)
	require.NoError(t, err)
	require.Equal(t, int64(6), p.Stats.Total)
	require.Empty(t, p.Items, "over-limit exports must not load full answer payloads")
	require.NoError(t, r.db.Exec("UPDATE messages SET deleted_at=? WHERE id='m3'", time.Now()).Error)
	require.NoError(t, r.db.Exec("UPDATE sessions SET deleted_at=? WHERE id='s2'", time.Now()).Error)
	q.PageSize = 20
	p, err = r.List(ctx, 1, q, true)
	require.NoError(t, err)
	require.Equal(t, int64(4), p.Stats.Total)
	// The FAQ's independent record still carries historical content even when
	// its session/knowledge source has been deleted.
	require.NoError(t, r.db.Exec("UPDATE knowledge_bases SET deleted_at=? WHERE id='kb1'", time.Now()).Error)
	rows, err := r.Detail(ctx, 1, "faq", "s2", "10", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "new FAQ", rows[0].Question)
	require.NotEmpty(t, rows[0].Answers)
}
