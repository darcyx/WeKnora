package im

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/golang-migrate/migrate/v4"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite3"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type resolutionSessionService struct {
	interfaces.SessionService
	db          *gorm.DB
	afterCreate func()
}

func (s *resolutionSessionService) CreateSession(ctx context.Context, session *types.Session) (*types.Session, error) {
	if err := s.db.WithContext(ctx).Create(session).Error; err != nil {
		return nil, err
	}
	if hook := s.afterCreate; hook != nil {
		s.afterCreate = nil
		hook()
	}
	return session, nil
}

// Use the shipped migrations: AutoMigrate does not install the partial unique
// indexes that determine whether these resolutions succeed in production.
func newResolutionService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "sessions.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	driver, err := sqlitemigrate.WithInstance(sqlDB, &sqlitemigrate.Config{})
	require.NoError(t, err)
	root, err := filepath.Abs(filepath.Join("..", "..", "migrations", "sqlite"))
	require.NoError(t, err)
	m, err := migrate.NewWithDatabaseInstance("file://"+root, "sqlite3", driver)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	require.NoError(t, m.Up())
	return &Service{db: db, sessionService: &resolutionSessionService{db: db}}
}

func resolveTestMessage(t *testing.T, s *Service, user, thread, agent, mode string) *ChannelSession {
	t.Helper()
	cs, err := s.resolveSession(context.Background(), &IncomingMessage{
		Platform: PlatformFeishu, UserID: user, ChatID: "group", ThreadID: thread, Content: "hello",
	}, 1, agent, "channel", mode)
	require.NoError(t, err)
	return cs
}

func TestSessionResolutionSeparatesThreadsForTheSameUser(t *testing.T) {
	s := newResolutionService(t)
	first := resolveTestMessage(t, s, "alice", "topic-1", "agent", "thread")
	second := resolveTestMessage(t, s, "alice", "topic-2", "agent", "thread")
	require.NotEqual(t, first.SessionID, second.SessionID)
	require.Equal(t, first.SessionID, resolveTestMessage(t, s, "alice", "topic-1", "agent", "thread").SessionID)
	require.Equal(t, first.SessionID, resolveTestMessage(t, s, "bob", "topic-1", "agent", "thread").SessionID)
}

func TestSessionResolutionKeepsModesSeparate(t *testing.T) {
	for _, firstMode := range []string{"user", "thread"} {
		t.Run(firstMode+" first", func(t *testing.T) {
			s := newResolutionService(t)
			resolveTestMessage(t, s, "alice", "topic", "agent", firstMode)
			user := resolveTestMessage(t, s, "alice", "topic", "agent", "user")
			thread := resolveTestMessage(t, s, "alice", "topic", "agent", "thread")
			require.Empty(t, user.ThreadID)
			require.NotEqual(t, user.SessionID, thread.SessionID)
			require.Equal(t, user.SessionID, resolveTestMessage(
				t, s, "alice", "other-topic", "agent", "user").SessionID)
			require.Equal(t, thread.SessionID, resolveTestMessage(t, s, "alice", "topic", "agent", "thread").SessionID)
			// Platforms without a thread ID retain user-mode continuity.
			require.Equal(t, user.SessionID, resolveTestMessage(t, s, "alice", "", "agent", "thread").SessionID)
		})
	}
}

func TestSessionResolutionIsolatesAgents(t *testing.T) {
	for _, mode := range []string{"user", "thread"} {
		t.Run(mode, func(t *testing.T) {
			s := newResolutionService(t)
			first := resolveTestMessage(t, s, "alice", "topic", "agent-1", mode)
			second := resolveTestMessage(t, s, "alice", "topic", "agent-2", mode)
			require.NotEqual(t, first.SessionID, second.SessionID)
		})
	}
}

func TestSessionResolutionRecoversConcurrentCreation(t *testing.T) {
	for _, mode := range []string{"user", "thread"} {
		t.Run(mode, func(t *testing.T) {
			s := newResolutionService(t)
			wantLive := int64(1)
			if mode == "user" {
				thread := resolveTestMessage(t, s, "alice", "old-topic", "agent", "thread")
				// GORM's First orders by primary key. Make the thread the first
				// candidate so an unscoped fallback cannot pass by chance.
				require.NoError(t, s.db.Model(&ChannelSession{}).Where("id = ?", thread.ID).
					Update("id", "000-thread").Error)
				wantLive++
			}
			var winner *ChannelSession
			// Force another request to win between the first request's miss and
			// mapping insert, without depending on goroutine/SQLite lock timing.
			s.sessionService.(*resolutionSessionService).afterCreate = func() {
				winner = resolveTestMessage(t, s, "alice", "topic", "agent", mode)
			}
			got := resolveTestMessage(t, s, "alice", "topic", "agent", mode)
			require.Equal(t, winner.ID, got.ID)
			var liveSessions, liveMappings int64
			require.NoError(t, s.db.Model(&types.Session{}).Count(&liveSessions).Error)
			require.NoError(t, s.db.Model(&ChannelSession{}).Count(&liveMappings).Error)
			require.Equal(t, wantLive, liveSessions, "losing creation must clean up its session")
			require.Equal(t, wantLive, liveMappings)
		})
	}
}

func TestSessionResolutionOwnersIsolateChatsAndRebindLegacy(t *testing.T) {
	s := newResolutionService(t)
	makeMsg := func(chat string) *IncomingMessage {
		return &IncomingMessage{Platform: PlatformFeishu, UserID: "alice", ChatID: chat, ThreadID: "same", Content: "hello"}
	}
	a, err := s.resolveSession(context.Background(), makeMsg("chat-a"), 1, "agent", "channel", "thread")
	require.NoError(t, err)
	b, err := s.resolveSession(context.Background(), makeMsg("chat-b"), 1, "agent", "channel", "thread")
	require.NoError(t, err)
	var first, second types.Session
	require.NoError(t, s.db.First(&first, "id = ?", a.SessionID).Error)
	require.NoError(t, s.db.First(&second, "id = ?", b.SessionID).Error)
	require.NotEqual(t, first.UserID, second.UserID)
	old := "im_thread:1:channel:feishu:same"
	require.NoError(t, s.db.Model(&types.Session{}).Where("id = ?", a.SessionID).Update("user_id", old).Error)
	_, err = s.resolveSession(context.Background(), makeMsg("chat-a"), 1, "agent", "channel", "thread")
	require.NoError(t, err)
	require.NoError(t, s.db.First(&first, "id = ?", a.SessionID).Error)
	require.Equal(t, types.IMThreadSessionOwnerID(1, "channel", "feishu", "chat-a", "same"), first.UserID)
	other, err := s.resolveSession(context.Background(), makeMsg("chat-a"), 1, "agent", "different-channel", "thread")
	require.NoError(t, err)
	require.NotEqual(t, a.SessionID, other.SessionID)
}

func TestSessionResolutionRebindsSameUserHistory(t *testing.T) {
	s := newResolutionService(t)
	msg := func(chat, user string) *IncomingMessage {
		return &IncomingMessage{Platform: PlatformFeishu, UserID: user, ChatID: chat}
	}
	a, err := s.resolveSession(context.Background(), msg("a", "alice"), 1, "agent", "channel", "user")
	require.NoError(t, err)
	b, err := s.resolveSession(context.Background(), msg("b", "alice"), 1, "agent", "channel", "user")
	require.NoError(t, err)
	c, err := s.resolveSession(context.Background(), msg("c", "bob"), 1, "agent", "channel", "user")
	require.NoError(t, err)
	require.NoError(t, s.db.Model(&types.Session{}).Where("id IN ?", []string{a.SessionID, b.SessionID, c.SessionID}).Update("user_id", "").Error)
	require.NoError(t, s.db.Create(&IMChannel{ID: "channel", TenantID: 1, AgentID: "agent", Platform: "feishu"}).Error)
	require.NoError(t, s.db.Model(&ChannelSession{}).Where("id = ?", c.ID).Update("im_channel_id", "").Error)
	migration, err := os.ReadFile("../../migrations/sqlite/000054_im_session_channel_scope.up.sql")
	require.NoError(t, err)
	require.NoError(t, s.db.Exec(string(migration)).Error)
	_, err = s.resolveSession(context.Background(), msg("a", "alice"), 1, "agent", "channel", "user")
	require.NoError(t, err)
	var sessions []types.Session
	require.NoError(t, s.db.Where("id IN ?", []string{a.SessionID, b.SessionID, c.SessionID}).Find(&sessions).Error)
	for _, session := range sessions {
		if session.ID == c.SessionID {
			require.Empty(t, session.UserID)
		} else {
			require.Equal(t, "im_user:1:channel:feishu:alice", session.UserID)
		}
	}
}

func TestSessionResolutionAllowsStaleMappingRecovery(t *testing.T) {
	s := newResolutionService(t)
	msg := &IncomingMessage{Platform: PlatformFeishu, UserID: "alice", ChatID: "a"}
	cs, err := s.resolveSession(context.Background(), msg, 1, "agent", "channel", "user")
	require.NoError(t, err)
	require.NoError(t, s.db.Delete(&types.Session{}, "id = ?", cs.SessionID).Error)
	existing, err := s.resolveSession(context.Background(), msg, 1, "agent", "channel", "user")
	require.NoError(t, err)
	require.Equal(t, cs.ID, existing.ID)
}

func TestSessionResolutionBindsOnlyUnambiguousBlankChannel(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%v", ambiguous), func(t *testing.T) {
			s := newResolutionService(t)
			msg := &IncomingMessage{Platform: PlatformFeishu, UserID: "alice", ChatID: "a"}
			cs, err := s.resolveSession(context.Background(), msg, 1, "agent", "channel", "user")
			require.NoError(t, err)
			require.NoError(t, s.db.Model(&ChannelSession{}).Where("id = ?", cs.ID).Update("im_channel_id", "").Error)
			require.NoError(t, s.db.Model(&types.Session{}).Where("id = ?", cs.SessionID).Update("user_id", "").Error)
			require.NoError(t, s.db.Create(&IMChannel{ID: "channel", TenantID: 1, AgentID: "agent", Platform: "feishu"}).Error)
			if ambiguous {
				require.NoError(t, s.db.Create(&IMChannel{ID: "other", TenantID: 1, AgentID: "agent", Platform: "feishu"}).Error)
				require.NoError(t, s.db.Delete(&IMChannel{}, "id = ?", "other").Error)
			}
			resolved, err := s.resolveSession(context.Background(), msg, 1, "agent", "channel", "user")
			require.NoError(t, err)
			if ambiguous {
				require.NotEqual(t, cs.SessionID, resolved.SessionID)
			} else {
				require.Equal(t, cs.SessionID, resolved.SessionID)
			}
			var session types.Session
			require.NoError(t, s.db.First(&session, "id = ?", cs.SessionID).Error)
			if ambiguous {
				require.Empty(t, session.UserID)
			} else {
				require.Equal(t, "im_user:1:channel:feishu:alice", session.UserID)
			}
		})
	}
}

func TestSessionResolutionSeparateChannels(t *testing.T) {
	for _, mode := range []string{"user", "thread"} {
		t.Run(mode, func(t *testing.T) {
			s := newResolutionService(t)
			msg := &IncomingMessage{Platform: PlatformFeishu, UserID: "alice", ChatID: "group", ThreadID: "same"}
			a, err := s.resolveSession(context.Background(), msg, 1, "agent", "a", mode)
			require.NoError(t, err)
			b, err := s.resolveSession(context.Background(), msg, 1, "agent", "b", mode)
			require.NoError(t, err)
			require.NotEqual(t, a.SessionID, b.SessionID)
			again, err := s.resolveSession(context.Background(), msg, 1, "agent", "a", mode)
			require.NoError(t, err)
			require.Equal(t, a.SessionID, again.SessionID)
		})
	}
}

func TestSessionChannelMigrationDownRejectsDuplicatesBeforeChangingIndexes(t *testing.T) {
	s := newResolutionService(t)
	msg := &IncomingMessage{Platform: PlatformFeishu, UserID: "alice", ChatID: "group"}
	_, err := s.resolveSession(context.Background(), msg, 1, "agent", "a", "user")
	require.NoError(t, err)
	_, err = s.resolveSession(context.Background(), msg, 1, "agent", "b", "user")
	require.NoError(t, err)
	down, err := os.ReadFile("../../migrations/sqlite/000054_im_session_channel_scope.down.sql")
	require.NoError(t, err)
	require.Error(t, s.db.Exec(string(down)).Error)
	var count int64
	require.NoError(t, s.db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('idx_channel_lookup', 'idx_channel_thread_lookup')").Scan(&count).Error)
	require.EqualValues(t, 2, count)
}
