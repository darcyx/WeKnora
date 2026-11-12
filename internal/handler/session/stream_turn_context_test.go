package session

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// cancelAfterFirstReadStream mimics Redis: a canceled context fails the read.
// The first successful read cancels the caller's context, the way agent mode
// releases the sandbox turn lease before the completion event is published.
type cancelAfterFirstReadStream struct {
	interfaces.StreamManager
	events []interfaces.StreamEvent
	cancel context.CancelFunc
	reads  int
}

func (s *cancelAfterFirstReadStream) GetEvents(
	ctx context.Context, _, _ string, fromOffset int,
) ([]interfaces.StreamEvent, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, fromOffset, fmt.Errorf("failed to get events from Redis: %w", err)
	}
	s.reads++
	if s.reads == 1 {
		s.cancel()
		return s.events[:1], 1, nil
	}
	if fromOffset >= len(s.events) {
		return nil, len(s.events), nil
	}
	return s.events[fromOffset:], len(s.events), nil
}

func TestHandleAgentEventsForSSE_DrainsCompletionAfterTurnContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{streamManager: &cancelAfterFirstReadStream{
		cancel: cancel,
		events: []interfaces.StreamEvent{
			{ID: "answer-1", Type: types.ResponseTypeAnswer, Content: "你好", Done: true},
			{ID: "complete-1", Type: types.ResponseTypeComplete, Done: true},
		},
	}}
	c, recorder := newTestGinContext(t, "")

	done := make(chan struct{})
	go func() {
		h.handleAgentEventsForSSE(
			ctx, c, "sess1", "msg1", "req-1", nil, false, publicStreamRewriter())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE poll kept using the cancelled turn context and never delivered completion")
	}

	body := recorder.Body.String()
	require.Contains(t, body, "你好")
	assert.Contains(t, body, `"response_type":"complete"`)
}

func TestAgentSSEEndsOnTerminalEvent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, last types.ResponseType
	}{
		{"early title", types.ResponseTypeSessionTitle, types.ResponseTypeComplete},
		{"late title", types.ResponseTypeComplete, types.ResponseTypeSessionTitle},
		{"standalone failure", types.ResponseTypeAnswer, types.ResponseTypeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{streamManager: &growingStreamManager{stubStreamManager: stubStreamManager{events: []interfaces.StreamEvent{
				{Type: tc.first, Done: true}, {Type: tc.last, Done: true},
			}}, visible: 1}}
			c, recorder := newTestGinContext(t, "")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			h.handleAgentEventsForSSE(ctx, c, "s", "m", "r", nil, true, nil)
			require.NoError(t, ctx.Err())
			require.Contains(t, recorder.Body.String(), `"response_type":"`+string(tc.last)+`"`)
		})
	}
}

func TestLegacyToolErrorKeepsStreamOpen(t *testing.T) {
	for _, key := range []string{"tool_name", "tool_call_id"} {
		require.False(t, endsQAStream(interfaces.StreamEvent{
			Type: types.ResponseTypeError, Done: true, Data: map[string]interface{}{key: "lookup"},
		}))
	}
}

func TestContinueStreamEndsOnStopOrError(t *testing.T) {
	for _, terminal := range []types.ResponseType{"stop", types.ResponseTypeError} {
		for _, visible := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/initial-%d", terminal, visible), func(t *testing.T) {
				h := &Handler{sessionService: &stubSessionService{}, messageService: &stubMessageServiceForStream{},
					streamManager: &growingStreamManager{stubStreamManager: stubStreamManager{events: []interfaces.StreamEvent{
						{ID: "a", Type: types.ResponseTypeAnswer, Content: "pending"}, {Type: terminal, Done: true},
					}}, visible: visible},
				}
				r := gin.New()
				r.GET("/stream/:session_id", h.ContinueStream)
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/stream/s?message_id=m", nil)
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				require.NoError(t, ctx.Err())
				require.Contains(t, rec.Body.String(), `"response_type":"`+string(terminal)+`"`)
			})
		}
	}
}
