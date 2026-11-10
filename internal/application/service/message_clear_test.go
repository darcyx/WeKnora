package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type clearMessagesRepository struct {
	interfaces.MessageRepository
	lookupErr, deleteErr error
	deleted              bool
	deleteCalls          int
}

func (r *clearMessagesRepository) GetKnowledgeIDsBySessionID(context.Context, string) ([]string, error) {
	if r.deleted {
		return nil, nil
	}
	return []string{"index-1"}, r.lookupErr
}

func (r *clearMessagesRepository) DeleteMessagesBySessionID(context.Context, string) error {
	r.deleteCalls++
	if r.deleteErr == nil {
		r.deleted = true
	}
	return r.deleteErr
}

type clearKnowledgeRepository struct{ interfaces.KnowledgeRepository }

func (clearKnowledgeRepository) GetKnowledgeBatch(context.Context, uint64, []string) ([]*types.Knowledge, error) {
	return []*types.Knowledge{{ID: "index-1", TenantID: 7, KnowledgeBaseID: "history"}}, nil
}

type clearKnowledgeService struct {
	interfaces.KnowledgeService
	deleted chan []string
}

func (s clearKnowledgeService) GetRepository() interfaces.KnowledgeRepository {
	return clearKnowledgeRepository{}
}

func (s clearKnowledgeService) DeleteKnowledgeList(_ context.Context, ids []string) error {
	s.deleted <- ids
	return nil
}

func TestClearSessionCapturesKnowledgeReferencesBeforeDeletingMessages(t *testing.T) {
	repo := &clearMessagesRepository{}
	knowledge := clearKnowledgeService{deleted: make(chan []string, 1)}
	s := &messageService{sessionRepo: &deleteSessionRepo{}, messageRepo: repo, knowService: knowledge}
	require.NoError(t, s.ClearSessionMessages(deleteCallerCtx(), "session"))
	require.True(t, repo.deleted)
	select {
	case ids := <-knowledge.deleted:
		require.Equal(t, []string{"index-1"}, ids, "cleanup must use references captured before rows disappear")
	case <-time.After(time.Second):
		t.Fatal("knowledge cleanup lost the deleted message references")
	}
}

func TestClearSessionFailurePreservesKnowledge(t *testing.T) {
	for _, lookupFails := range []bool{true, false} {
		repo := &clearMessagesRepository{}
		failure := errors.New("database unavailable")
		if lookupFails {
			repo.lookupErr = failure
		} else {
			repo.deleteErr = failure
		}
		knowledge := clearKnowledgeService{deleted: make(chan []string, 1)}
		s := &messageService{sessionRepo: &deleteSessionRepo{}, messageRepo: repo, knowService: knowledge}
		require.ErrorIs(t, s.ClearSessionMessages(deleteCallerCtx(), "session"), failure)
		require.False(t, repo.deleted)
		require.Empty(t, knowledge.deleted)
		if lookupFails {
			require.Zero(t, repo.deleteCalls, "failed reference lookup must not discard the only cleanup relationship")
		}
	}
}
