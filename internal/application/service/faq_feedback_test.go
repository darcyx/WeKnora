package service

import (
	"context"
	"testing"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type faqFeedbackChunks struct {
	interfaces.ChunkRepository
	chunk *types.Chunk
	err   error
}

func (r *faqFeedbackChunks) GetChunkBySeqID(_ context.Context, tenant uint64, id int64) (*types.Chunk, error) {
	return r.chunk, r.err
}

type faqFeedbackKnowledge struct{ interfaces.KnowledgeService }

func (*faqFeedbackKnowledge) GetFAQEntry(context.Context, string, int64) (*types.FAQEntry, error) {
	return &types.FAQEntry{ID: 123, KnowledgeBaseID: "kb", StandardQuestion: "如何办理？", Answers: []string{"在线办理", "柜台办理"}}, nil
}

type faqFeedbackSessions struct {
	interfaces.SessionRepository
	tenant         uint64
	owner, session string
	err            error
}

func (r *faqFeedbackSessions) Get(_ context.Context, tenant uint64, owner, id string) (*types.Session, error) {
	r.tenant, r.owner, r.session = tenant, owner, id
	return &types.Session{}, r.err
}
func (*faqFeedbackSessions) GetIMPlatform(context.Context, uint64, string) (string, error) {
	return "", nil
}

type faqFeedbackMessages struct {
	interfaces.MessageRepository
	saved       *types.FAQFeedback
	session, id string
}

func (r *faqFeedbackMessages) CreateFAQFeedback(_ context.Context, f *types.FAQFeedback) error {
	r.saved = f
	return nil
}
func (r *faqFeedbackMessages) GetAssistantMessageByRequestID(_ context.Context, session, id string) (*types.Message, error) {
	r.session, r.id = session, id
	return &types.Message{Role: "assistant"}, nil
}
func (r *faqFeedbackMessages) UpdateMessageFeedback(context.Context, string, string, types.MessageFeedback) error {
	return nil
}

func TestSubmitFAQFeedbackRouting(t *testing.T) {
	for _, id := range []string{"123", "000123", "message-123", "12a", "-123", "1.2"} {
		t.Run(id, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
			ctx = context.WithValue(ctx, types.UserIDContextKey, "alice")
			messages := &faqFeedbackMessages{}
			sessions := &faqFeedbackSessions{}
			s := &messageService{messageRepo: messages, sessionRepo: sessions, chunkRepo: &faqFeedbackChunks{chunk: &types.Chunk{SeqID: 123, TenantID: 1, ChunkType: types.ChunkTypeFAQ, KnowledgeBaseID: "kb"}}, knowService: &faqFeedbackKnowledge{}}
			got, err := s.SubmitMessageFeedback(ctx, "session-1", id, "dislike", []string{"other"}, " explanation ")
			require.NoError(t, err)
			require.Equal(t, "explanation", got.Feedback.ReasonText)
			require.Equal(t, "session-1", sessions.session)
			require.Equal(t, "alice", sessions.owner)
			if id == "123" || id == "000123" {
				require.NotNil(t, messages.saved)
				require.Equal(t, int64(123), messages.saved.EntryID)
				require.Equal(t, "session-1", messages.saved.SessionID)
				require.Equal(t, "alice", messages.saved.UserID)
				require.Empty(t, messages.id)
				require.Equal(t, "如何办理？", messages.saved.StandardQuestion)
				require.Equal(t, []string{"在线办理", "柜台办理"}, messages.saved.Answers)
				require.Equal(t, "kb", messages.saved.KnowledgeBaseID)
			} else {
				require.Nil(t, messages.saved)
				require.Equal(t, id, messages.id)
				require.Equal(t, "session-1", messages.session)
			}
		})
	}
}

func TestSubmitFAQFeedbackRejectsInvalidTargets(t *testing.T) {
	for _, tc := range []struct {
		name, id             string
		sessionErr, chunkErr error
		tenant               uint64
		kind                 string
		reasons              []string
	}{
		{name: "zero", id: "0"}, {name: "overflow", id: "999999999999999999999"},
		{name: "session denied", id: "123", sessionErr: apperrors.ErrSessionNotFound},
		{name: "missing FAQ", id: "123", chunkErr: gorm.ErrRecordNotFound},
		{name: "wrong tenant", id: "123", tenant: 2, kind: types.ChunkTypeFAQ},
		{name: "not FAQ", id: "123", tenant: 1, kind: "text"},
		{name: "invalid reasons", id: "123", tenant: 1, kind: types.ChunkTypeFAQ, reasons: []string{"unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
			ctx = context.WithValue(ctx, types.UserIDContextKey, "alice")
			messages := &faqFeedbackMessages{}
			s := &messageService{messageRepo: messages, sessionRepo: &faqFeedbackSessions{err: tc.sessionErr}, chunkRepo: &faqFeedbackChunks{err: tc.chunkErr, chunk: &types.Chunk{TenantID: tc.tenant, ChunkType: tc.kind}}, knowService: &faqFeedbackKnowledge{}}
			_, err := s.SubmitMessageFeedback(ctx, "session-1", tc.id, "dislike", tc.reasons, "")
			require.Error(t, err)
			require.Nil(t, messages.saved)
		})
	}
}

func TestMessageFeedbackRequestLookupRequiresOwnedSession(t *testing.T) {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "alice")
	messages := &faqFeedbackMessages{}
	sessions := &faqFeedbackSessions{err: apperrors.ErrSessionNotFound}
	s := &messageService{messageRepo: messages, sessionRepo: sessions}
	_, err := s.SubmitMessageFeedback(ctx, "someone-elses-session", "request-1", "like", nil, "")
	require.ErrorIs(t, err, apperrors.ErrSessionNotFound)
	require.Empty(t, messages.id)
	require.Equal(t, "alice", sessions.owner)
}
