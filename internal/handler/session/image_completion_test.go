package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/access"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type imageCompletionMessages struct {
	interfaces.MessageService
	saved      *types.Message
	tenant     uint64
	err        error
	beforeSave func()
}

func (s *imageCompletionMessages) UpdateMessage(ctx context.Context, message *types.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.beforeSave != nil {
		s.beforeSave()
	}
	if s.err != nil {
		return s.err
	}
	s.tenant = types.MustTenantIDFromContext(ctx)
	snapshot := *message
	s.saved = &snapshot
	return nil
}

func TestQuickAnswerCompletionPersistsAfterGenerationCancellation(t *testing.T) {
	messages := &imageCompletionMessages{}
	stream := &imageCompletionStream{}
	h := &Handler{messageService: messages, streamManager: stream}
	bus := event.NewEventBus()
	message := &types.Message{
		ID: "m", SessionID: "s", Role: "assistant", Content: "already streamed answer", AgentTenantID: 2,
	}
	ctx, cancel := context.WithCancel(types.WithExecutionTenant(context.Background(), 1))
	streamHandler := h.setupStreamHandler(ctx, "s", "m", "req", 1, time.Now(), message, bus)
	cancel()
	released := false
	h.completeQuickAnswerTurn(ctx, &sseStreamContext{
		eventBus: bus, streamHandler: streamHandler, assistantMessage: message,
		releaseTurn: func() { released = true },
	}, "", "")
	require.NotNil(t, messages.saved)
	require.Equal(t, "already streamed answer", messages.saved.Content)
	require.Equal(t, uint64(1), messages.tenant)
	require.True(t, released)
	require.Equal(t, types.ResponseTypeComplete, stream.events[len(stream.events)-1].Type)
}

type imageCompletionStream struct {
	completionEventRecorder
	onComplete func()
}

func (s *imageCompletionStream) AppendEvent(
	ctx context.Context, session, message string, e interfaces.StreamEvent,
) error {
	if e.Type == types.ResponseTypeComplete && s.onComplete != nil {
		s.onComplete()
	}
	return s.completionEventRecorder.AppendEvent(ctx, session, message, e)
}

func TestSharedImageCompletionWaitsForPersistedReferences(t *testing.T) {
	const image = "resource://AbCdEfGhIjKlMnOpQrStUv"
	for _, quick := range []bool{false, true} {
		name := "agent"
		if quick {
			name = "quick answer"
		}
		t.Run(name, func(t *testing.T) {
			for _, fail := range []bool{false, true} {
				t.Run(map[bool]string{false: "saved", true: "save failed"}[fail], func(t *testing.T) {
					messages := &imageCompletionMessages{}
					if fail {
						messages.err = errors.New("database unavailable")
					}
					stream := &imageCompletionStream{}
					h := &Handler{messageService: messages, streamManager: stream}
					bus := event.NewEventBus()
					message := &types.Message{
						ID: "m", SessionID: "s", Role: "assistant", AgentTenantID: 2,
					}
					// Execution uses the host workspace; the conversation belongs to
					// the visitor's workspace and must be saved there before notifying it.
					runCtx := types.WithExecutionTenant(context.Background(), 2)
					saveCtx := types.WithExecutionTenant(runCtx, 1)
					streamHandler := h.setupStreamHandler(runCtx, "s", "m", "req", 1, time.Now(), message, bus)
					streamCtx := &sseStreamContext{
						eventBus: bus, streamHandler: streamHandler, assistantMessage: message,
					}
					completed := 0
					stream.onComplete = func() {
						completed++
						require.NotNil(t, messages.saved, "a completion-triggered image GET must see persisted output")
						require.True(t, access.MessageReferencesFile(messages.saved, image))
						require.True(t, messages.saved.IsCompleted)
						require.Equal(t, uint64(1), messages.tenant)
					}
					messages.beforeSave = func() {
						require.Zero(t, completed, "completion must not escape while the database write is pending")
					}
					answer := "![host image](" + image + ")"
					if quick {
						message.Content = answer
						h.completeQuickAnswerTurn(saveCtx, streamCtx, "", "")
					} else {
						require.NoError(t, bus.Emit(runCtx, event.Event{
							Type: event.EventAgentComplete,
							Data: event.AgentCompleteData{MessageID: "m", FinalAnswer: answer},
						}))
						require.Zero(t, completed)
						h.completeStreamAssistantMessage(saveCtx, streamCtx, "", "")
					}
					last := stream.events[len(stream.events)-1]
					if fail {
						require.Zero(t, completed)
						require.Equal(t, types.ResponseTypeError, last.Type)
						require.Equal(t, "message_persistence", last.Data["stage"])
					} else {
						require.Equal(t, 1, completed)
						require.Equal(t, types.ResponseTypeComplete, last.Type)
						require.Equal(t, answer, last.Data["final_content"])
						require.NoError(t, streamHandler.publishCompletion(saveCtx))
						require.Equal(t, 1, completed, "completion must only be published once")
					}
				})
			}
		})
	}
}

func TestAgentExecutionFailurePersistsBeforeClosingStream(t *testing.T) {
	for _, answer := range []string{"", "partial answer"} {
		t.Run("prior answer "+answer, func(t *testing.T) {
			messages := &imageCompletionMessages{}
			stream := &imageCompletionStream{}
			h := &Handler{messageService: messages, streamManager: stream}
			ctx := types.WithExecutionTenant(t.Context(), 1)
			runCtx, cancel := context.WithCancel(ctx)
			bus := event.NewEventBus()
			msg := &types.Message{ID: "m", SessionID: "s", Content: answer}
			handler := h.setupStreamHandler(runCtx, "s", "m", "r", 1, time.Now(), msg, bus)
			if answer != "" {
				require.NoError(t, bus.Emit(ctx, event.Event{Type: event.EventAgentComplete,
					Data: event.AgentCompleteData{MessageID: "m"}}))
			}
			cancel()
			require.NoError(t, bus.Emit(ctx, event.Event{Type: event.EventError,
				Data: event.ErrorData{Stage: "agent_execution", Error: "model unavailable"}}))
			for _, frame := range stream.events {
				require.False(t, endsQAStream(frame))
			}
			h.completeStreamAssistantMessage(ctx, &sseStreamContext{assistantMessage: msg, streamHandler: handler}, "query", "u")
			require.NotNil(t, messages.saved)
			require.True(t, messages.saved.IsCompleted)
			if answer == "" {
				require.Equal(t, "model unavailable", messages.saved.Content)
			} else {
				require.Equal(t, answer, messages.saved.Content)
			}
			require.True(t, messages.saved.IsFallback)
			var failures int
			for _, frame := range stream.events {
				if frame.Type == types.ResponseTypeError {
					failures++
				}
			}
			require.Equal(t, 1, failures)
		})
	}
}

func TestAgentStopLetsWorkerPersistPartialAnswer(t *testing.T) {
	messages := &imageCompletionMessages{}
	h := &Handler{messageService: messages}
	ctx := types.WithExecutionTenant(t.Context(), 1)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	bus := event.NewEventBus()
	msg := &types.Message{ID: "m", SessionID: "s"}
	h.setupStopEventHandler(bus, "s", 1, msg, cancel, qaModeAgent)
	require.NoError(t, bus.Emit(ctx, event.Event{Type: event.EventStop}))
	require.ErrorIs(t, runCtx.Err(), context.Canceled)
	require.Nil(t, messages.saved, "the worker is still collecting its partial answer")
}
