package collaboration

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBindingStartReusesSingleHeartbeat(t *testing.T) {
	store, scope := testStore(t)
	rt := New(store, func(context.Context, Work) (Result, error) { return Result{}, nil })
	rt.Lease = 200 * time.Millisecond

	b, err := rt.AcquireParent(context.Background(), scope)
	require.NoError(t, err)

	ctx1, stop1 := b.Start(context.Background())
	ctx2, stop2 := b.Start(ctx1)
	ctx3, stop3 := b.Start(ctx2)

	b.heartbeatMu.Lock()
	refs := b.heartbeatRefs
	doneCh := b.heartbeatDone
	b.heartbeatMu.Unlock()
	require.Equal(t, 3, refs)
	require.NotNil(t, doneCh)

	stop3()
	stop2()
	b.heartbeatMu.Lock()
	require.Equal(t, 1, b.heartbeatRefs)
	b.heartbeatMu.Unlock()

	stop1()
	b.heartbeatMu.Lock()
	require.Equal(t, 0, b.heartbeatRefs)
	b.heartbeatMu.Unlock()
	require.Error(t, ctx1.Err())
	require.Error(t, ctx2.Err())
	require.Error(t, ctx3.Err())
}

func TestWakeParentReleasesLeaseOnTransientErrorInsteadOfCancelling(t *testing.T) {
	store, scope := testStore(t)
	rt := New(store, func(context.Context, Work) (Result, error) { return Result{}, nil })
	rt.Lease = time.Second

	var attempts atomic.Int32
	rt.WakeParent = func(_ context.Context, _ *Binding, _ []byte) error {
		attempts.Add(1)
		return errors.New("transient redis error")
	}

	b, err := rt.AcquireParent(context.Background(), scope)
	require.NoError(t, err)
	require.NoError(t, b.SaveWait(context.Background(), []byte(`{"v":1}`), time.Now().Add(-time.Second)))
	// Clear lease so wakeParent claims it.
	require.NoError(t, store.Change(context.Background(), scope, func(g *Group) error {
		g.ParentOwner = ""
		g.ParentLease = time.Time{}
		return nil
	}))

	rt.wakeParent(context.Background(), scope)
	rt.workers.Wait()
	require.Equal(t, int32(1), attempts.Load())

	g := read(t, store, scope)
	require.Equal(t, "active", g.Status, "transient wakeParent error must not cancel group")
	require.NotNil(t, g.ParentWait, "checkpoint must stay intact for retry")
	require.Empty(t, g.ParentOwner, "lease should be released immediately for retry")
}

func TestWakeParentFinalizesPermanentCheckpointFailure(t *testing.T) {
	store, scope := testStore(t)
	rt := New(store, func(context.Context, Work) (Result, error) { return Result{}, nil })
	rt.Lease = time.Second
	var closes atomic.Int32
	rt.WakeParent = func(_ context.Context, b *Binding, _ []byte) error {
		if b.Closing {
			closes.Add(1)
			return nil
		}
		return fmt.Errorf("%w: corrupt checkpoint", ErrPermanentRecovery)
	}
	b, err := rt.AcquireParent(context.Background(), scope)
	require.NoError(t, err)
	require.NoError(t, b.SaveWait(context.Background(), []byte("bad"), time.Now().Add(-time.Second)))
	require.NoError(t, store.Change(context.Background(), scope, func(g *Group) error { g.ParentOwner = ""; g.ParentLease = time.Time{}; return nil }))
	rt.wakeParent(context.Background(), scope)
	rt.workers.Wait()
	g := read(t, store, scope)
	require.True(t, g.ParentClosePending)
	require.Nil(t, g.ParentWait)
	rt.wakeParent(context.Background(), scope)
	rt.workers.Wait()
	require.EqualValues(t, 1, closes.Load())
	require.False(t, read(t, store, scope).ParentClosePending)
}
