package stream

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

func newTestRedisStreamManager(t *testing.T, ttl time.Duration) (*RedisStreamManager, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	mgr, err := NewRedisStreamManager(mini.Addr(), "", "", 0, "test", ttl)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr, mini
}

// A corrupt live-run marker used to decode as "no run is live". Steer then
// answered new_run and the client started a second AgentQA on top of the
// still-generating turn. Lookup failures must surface as errors instead.
func TestGetLiveRunCorruptJSONIsError(t *testing.T) {
	mgr, mini := newTestRedisStreamManager(t, time.Hour)
	ctx := context.Background()

	require.NoError(t, mini.Set(mgr.buildLiveRunKey("sess-1"), "not-json"))

	id, req, err := mgr.GetLiveRun(ctx, "sess-1")
	require.Error(t, err)
	require.Empty(t, id)
	require.Empty(t, req)
}

func TestGetLiveRunMissingKeyIsEmpty(t *testing.T) {
	mgr, _ := newTestRedisStreamManager(t, time.Hour)

	id, req, err := mgr.GetLiveRun(context.Background(), "sess-missing")
	require.NoError(t, err)
	require.Empty(t, id)
	require.Empty(t, req)
}

// The live-run key is written once with the StreamManager TTL. A turn that
// runs longer than that TTL used to look idle, and /steer answered new_run.
// Streaming (AppendEvent / GetEvents) must refresh the marker.
func TestAppendEventRefreshesLiveRunTTL(t *testing.T) {
	ttl := 2 * time.Second
	mgr, mini := newTestRedisStreamManager(t, ttl)
	ctx := context.Background()

	require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
	mini.FastForward(ttl - 200*time.Millisecond)

	require.NoError(t, mgr.AppendEvent(ctx, "sess-1", "assist-1", interfaces.StreamEvent{
		ID:      "e1",
		Type:    types.ResponseTypeAnswer,
		Content: "chunk",
	}))
	mini.FastForward(ttl - 200*time.Millisecond)

	id, _, err := mgr.GetLiveRun(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "assist-1", id)
}

func TestReadPathsDoNotExtendLiveRunLeaseWhileReplayListSurvives(t *testing.T) {
	t.Run("redis", func(t *testing.T) {
		mgr, mini := newTestRedisStreamManager(t, time.Hour)
		mgr.WithLiveRunTTL(2 * time.Second)
		ctx := context.Background()

		require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
		require.NoError(t, mgr.AppendEvent(ctx, "sess-1", "assist-1", interfaces.StreamEvent{
			ID: "e1", Type: types.ResponseTypeAnswer, Content: "hello",
		}))

		// Advance past half the lease and hammer read/steer paths: none may renew live-run.
		mini.FastForward(1200 * time.Millisecond)
		_, _, err := mgr.GetEvents(ctx, "sess-1", "assist-1", 0)
		require.NoError(t, err)
		_, _, err = mgr.GetLiveRun(ctx, "sess-1")
		require.NoError(t, err)
		require.NoError(t, mgr.AppendSteerEvents(ctx, "sess-1", "assist-1", []interfaces.StreamEvent{{
			ID: "s1", Type: types.ResponseTypeSteer, Content: "queued", Done: true,
		}}))
		_, _, err = mgr.GetSteerEvents(ctx, "sess-1", "assist-1", 0)
		require.NoError(t, err)

		// Advance past the original 2s lease: dead worker marker must expire while event list stays readable.
		mini.FastForward(1000 * time.Millisecond)
		id, _, err := mgr.GetLiveRun(ctx, "sess-1")
		require.NoError(t, err)
		require.Empty(t, id, "read/steer traffic must not keep a dead live-run marker alive")

		events, _, err := mgr.GetEvents(ctx, "sess-1", "assist-1", 0)
		require.NoError(t, err)
		require.Len(t, events, 1, "event replay list keeps its longer replay TTL after live-run expires")
	})

	t.Run("memory", func(t *testing.T) {
		mgr := NewMemoryStreamManager().WithLiveRunTTL(80 * time.Millisecond)
		ctx := context.Background()
		require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
		time.Sleep(45 * time.Millisecond)
		_, _, err := mgr.GetEvents(ctx, "sess-1", "assist-1", 0)
		require.NoError(t, err)
		_, _, err = mgr.GetLiveRun(ctx, "sess-1")
		require.NoError(t, err)
		time.Sleep(55 * time.Millisecond)
		id, _, err := mgr.GetLiveRun(ctx, "sess-1")
		require.NoError(t, err)
		require.Empty(t, id)
	})
}

func TestStartLiveRunHeartbeatKeepsActiveRunAliveAndStopsOnTransfer(t *testing.T) {
	mgr := NewMemoryStreamManager().WithLiveRunTTL(90 * time.Millisecond)
	ctx := context.Background()

	require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
	stop := StartLiveRunHeartbeatWithInterval(ctx, mgr, "sess-1", "assist-1", 25*time.Millisecond)
	defer stop()

	// Stay alive well past the 90ms lease while heartbeat runs.
	time.Sleep(160 * time.Millisecond)
	id, _, err := mgr.GetLiveRun(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "assist-1", id)

	// Transfer ownership via CAS: old heartbeat must exit without touching assist-2.
	require.NoError(t, mgr.ClaimLiveRun(ctx, "sess-1", "assist-1", "assist-2", "req-2"))
	time.Sleep(120 * time.Millisecond)
	id, _, err = mgr.GetLiveRun(ctx, "sess-1")
	require.NoError(t, err)
	require.Empty(t, id, "old heartbeat must not renew assist-2's lease after transfer")
}

func TestSetLiveRunRejectsADifferentAssistant(t *testing.T) {
	mgr, _ := newTestRedisStreamManager(t, time.Hour)
	ctx := context.Background()

	require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
	require.ErrorIs(t, mgr.SetLiveRun(ctx, "sess-1", "assist-2", "req-2"), ErrLiveRunExists)

	id, req, err := mgr.GetLiveRun(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "assist-1", id)
	require.Equal(t, "req-1", req)
}

func TestSetLiveRunIsIdempotentForTheSameAssistant(t *testing.T) {
	mgr, _ := newTestRedisStreamManager(t, time.Hour)
	ctx := context.Background()

	require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
	require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
}

func TestClaimLiveRunCASRejectsWrongExpectedOwner(t *testing.T) {
	redisMgr, _ := newTestRedisStreamManager(t, time.Hour)
	for name, mgr := range map[string]interfaces.StreamManager{
		"redis":  redisMgr,
		"memory": NewMemoryStreamManager(),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			// Empty marker rejects ClaimLiveRun.
			require.ErrorIs(t, mgr.ClaimLiveRun(ctx, "sess-1", "assist-1", "assist-2", "req-2"), ErrLiveRunExists)

			require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
			// Wrong expected owner cannot steal the marker.
			require.ErrorIs(t, mgr.ClaimLiveRun(ctx, "sess-1", "assist-other", "assist-2", "req-2"), ErrLiveRunExists)

			// Matching expected owner atomically transfers ownership.
			require.NoError(t, mgr.ClaimLiveRun(ctx, "sess-1", "assist-1", "assist-2", "req-2"))
			// Idempotent retry by the new owner succeeds.
			require.NoError(t, mgr.ClaimLiveRun(ctx, "sess-1", "assist-1", "assist-2", "req-2"))

			id, req, err := mgr.GetLiveRun(ctx, "sess-1")
			require.NoError(t, err)
			require.Equal(t, "assist-2", id)
			require.Equal(t, "req-2", req)
		})
	}
}

func TestMemorySetLiveRunRejectsADifferentAssistant(t *testing.T) {
	mgr := NewMemoryStreamManager()
	ctx := context.Background()

	require.NoError(t, mgr.SetLiveRun(ctx, "sess-1", "assist-1", "req-1"))
	require.ErrorIs(t, mgr.SetLiveRun(ctx, "sess-1", "assist-2", "req-2"), ErrLiveRunExists)
	require.NoError(t, mgr.ClaimLiveRun(ctx, "sess-1", "assist-1", "assist-2", "req-2"))
	id, _, err := mgr.GetLiveRun(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "assist-2", id)
}

func TestAppendSteerEventsDeduplicatesClientIDs(t *testing.T) {
	redisManager, _ := newTestRedisStreamManager(t, time.Hour)
	for name, manager := range map[string]interfaces.StreamManager{
		"memory": NewMemoryStreamManager(), "redis": redisManager,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			evt := interfaces.StreamEvent{
				ID: "same-client-id", Content: "do this next", Data: map[string]interface{}{"delivery": "after"},
			}
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				go func() { errs <- manager.AppendSteerEvents(ctx, "session", "run", []interfaces.StreamEvent{evt}) }()
			}
			for i := 0; i < 8; i++ {
				require.NoError(t, <-errs)
			}
			events, _, err := manager.GetSteerEvents(ctx, "session", "run", 0)
			require.NoError(t, err)
			require.Len(t, events, 1)
			_, err = manager.UpdateSteerEventData(ctx, "session", "run", evt.ID,
				map[string]interface{}{"consumed": true})
			require.NoError(t, err)
			require.NoError(t, manager.AppendSteerEvents(ctx, "session", "run", []interfaces.StreamEvent{evt}))
			events, _, err = manager.GetSteerEvents(ctx, "session", "run", 0)
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, true, events[0].Data["consumed"])
		})
	}
}

func TestDropMessageStreamsRemovesEventsSteerAndLiveRun(t *testing.T) {
	mgr := NewMemoryStreamManager()
	ctx := context.Background()
	require.NoError(t, mgr.AppendEvent(ctx, "sess", "a-1", interfaces.StreamEvent{ID: "e1"}))
	require.NoError(t, mgr.AppendSteerEvents(ctx, "sess", "a-1", []interfaces.StreamEvent{{ID: "s1"}}))
	require.NoError(t, mgr.SetLiveRun(ctx, "sess", "a-1", "req"))

	require.NoError(t, mgr.DropMessageStreams(ctx, "sess", []string{"a-1"}))

	events, _, err := mgr.GetEvents(ctx, "sess", "a-1", 0)
	require.NoError(t, err)
	require.Empty(t, events)
	steer, _, err := mgr.GetSteerEvents(ctx, "sess", "a-1", 0)
	require.NoError(t, err)
	require.Empty(t, steer)
	liveID, _, err := mgr.GetLiveRun(ctx, "sess")
	require.NoError(t, err)
	require.Empty(t, liveID)
}

func TestRedisDropMessageStreamsRemovesEventsSteerAndLiveRun(t *testing.T) {
	mgr, _ := newTestRedisStreamManager(t, time.Hour)
	ctx := context.Background()
	require.NoError(t, mgr.AppendEvent(ctx, "sess", "a-1", interfaces.StreamEvent{ID: "e1"}))
	require.NoError(t, mgr.AppendSteerEvents(ctx, "sess", "a-1", []interfaces.StreamEvent{{ID: "s1"}}))
	require.NoError(t, mgr.SetLiveRun(ctx, "sess", "a-1", "req"))

	require.NoError(t, mgr.DropMessageStreams(ctx, "sess", []string{"a-1"}))

	events, _, err := mgr.GetEvents(ctx, "sess", "a-1", 0)
	require.NoError(t, err)
	require.Empty(t, events)
	steer, _, err := mgr.GetSteerEvents(ctx, "sess", "a-1", 0)
	require.NoError(t, err)
	require.Empty(t, steer)
	liveID, _, err := mgr.GetLiveRun(ctx, "sess")
	require.NoError(t, err)
	require.Empty(t, liveID)
}

func TestResetEventsDropsEventsAndKeepsSteer(t *testing.T) {
	redisManager, _ := newTestRedisStreamManager(t, time.Hour)
	for name, manager := range map[string]interfaces.StreamManager{
		"memory": NewMemoryStreamManager(), "redis": redisManager,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			require.NoError(t, manager.AppendEvent(ctx, "session", "run",
				interfaces.StreamEvent{ID: "old", Content: "before"}))
			require.NoError(t, manager.AppendSteerEvents(ctx, "session", "run",
				[]interfaces.StreamEvent{{ID: "steer-1", Content: "next"}}))

			require.NoError(t, manager.ResetEvents(ctx, "session", "run"))
			events, _, err := manager.GetEvents(ctx, "session", "run", 0)
			require.NoError(t, err)
			require.Empty(t, events)
			steer, _, err := manager.GetSteerEvents(ctx, "session", "run", 0)
			require.NoError(t, err)
			require.Len(t, steer, 1)

			require.NoError(t, manager.AppendEvent(ctx, "session", "run",
				interfaces.StreamEvent{ID: "new", Content: "after"}))
			events, _, err = manager.GetEvents(ctx, "session", "run", 0)
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, "new", events[0].ID)
		})
	}
}
