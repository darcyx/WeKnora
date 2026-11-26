package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// Synthetic data only: the run's total deadline expiring must turn gathered
// evidence into a partial answer, while a user stop stays silent.
func TestExecuteLoopDeadlineSynthesis(t *testing.T) {
	evidence := []types.AgentStep{{ToolCalls: []types.ToolCall{{ID: "c1", Name: "knowledge_search"}}}}
	expired := func(t *testing.T) context.Context {
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	}
	cancelled := func(t *testing.T) context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}
	for _, tc := range []struct {
		name       string
		ctx        func(*testing.T) context.Context
		steps      []types.AgentStep
		synthErr   error
		wantErr    error
		wantFinals int
		wantAnswer string
	}{
		{name: "deadline with evidence synthesizes", ctx: expired, steps: evidence,
			wantFinals: 1, wantAnswer: "Partial answer."},
		{name: "deadline without evidence keeps error", ctx: expired, wantErr: context.DeadlineExceeded},
		{name: "deadline synthesis failure keeps error", ctx: expired, steps: evidence,
			synthErr: errors.New("synthetic failure"), wantErr: context.DeadlineExceeded, wantFinals: 1},
		{name: "user cancel stays silent", ctx: cancelled, steps: evidence, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &finalSynthesisChat{}
			var liveCtx bool
			model.synthesize = func(ctx context.Context, _ []chat.Message, _ *chat.ChatOptions) (<-chan types.StreamResponse, error) {
				liveCtx = ctx.Err() == nil
				if tc.synthErr != nil {
					return nil, tc.synthErr
				}
				return finalSynthesisChunks(types.StreamResponse{
					ResponseType: types.ResponseTypeAnswer, Content: "Partial answer.", FinishReason: "stop", Done: true,
				}), nil
			}
			engine := newTestEngine(t, model, withMaxIterations(8))
			var answered bool
			engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
				answered = answered || ev.Data.(event.AgentFinalAnswerData).Done
				return nil
			})
			state := &types.AgentState{RoundSteps: tc.steps}
			_, err := engine.executeLoop(tc.ctx(t), state, "q", emptyMessages(), nil, "session", "message")
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, model.finals, tc.wantFinals)
			require.Equal(t, tc.wantAnswer, state.FinalAnswer)
			require.Equal(t, tc.wantAnswer != "", answered)
			require.Equal(t, tc.wantAnswer != "", state.IsComplete)
			if tc.wantFinals > 0 {
				require.True(t, liveCtx, "synthesis must run on a non-expired context")
				last := model.finals[0][len(model.finals[0])-1]
				require.Contains(t, last.Content, "time limit")
			}
		})
	}
}

func TestEmitExecutionErrorAfterDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ctx       func(*testing.T) context.Context
		wantEvent bool
	}{
		{"deadline emits error on a live context", func(t *testing.T) context.Context {
			ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		}, true},
		{"user cancel stays silent", func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newTestEngine(t, &finalSynthesisChat{})
			var got, live bool
			engine.eventBus.On(event.EventError, func(ctx context.Context, _ event.Event) error {
				got, live = true, ctx.Err() == nil
				return nil
			})
			engine.emitExecutionError(tc.ctx(t), "session", context.DeadlineExceeded)
			require.Equal(t, tc.wantEvent, got)
			require.Equal(t, tc.wantEvent, live)
		})
	}
}
