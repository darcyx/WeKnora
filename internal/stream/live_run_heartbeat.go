package stream

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// defaultLiveRunHeartbeatInterval renews a 2-minute live-run lease three times
// before expiry, keeping active long-running turns alive while dead workers
// expire promptly without read traffic extending their lease.
const defaultLiveRunHeartbeatInterval = 40 * time.Second

// StartLiveRunHeartbeat periodically refreshes the live-run marker owned by
// assistantMessageID until the returned stop function is called, ctx ends, or
// ownership transfers to another run.
func StartLiveRunHeartbeat(
	ctx context.Context,
	mgr interfaces.StreamManager,
	sessionID, assistantMessageID string,
) func() {
	return StartLiveRunHeartbeatWithInterval(ctx, mgr, sessionID, assistantMessageID, defaultLiveRunHeartbeatInterval)
}

// StartLiveRunHeartbeatWithInterval is the configurable variant of
// StartLiveRunHeartbeat, primarily used by tests with short lease TTLs.
func StartLiveRunHeartbeatWithInterval(
	ctx context.Context,
	mgr interfaces.StreamManager,
	sessionID, assistantMessageID string,
	interval time.Duration,
) func() {
	if mgr == nil || sessionID == "" || assistantMessageID == "" {
		return func() {}
	}
	if interval <= 0 {
		interval = defaultLiveRunHeartbeatInterval
	}
	hbCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				refreshed, err := mgr.RefreshLiveRun(hbCtx, sessionID, assistantMessageID)
				if err == nil && !refreshed {
					return
				}
			}
		}
	}()
	return cancel
}
