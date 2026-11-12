package session

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestFAQClarificationUsesNormalAgentStreamAndPersistsState(t *testing.T) {
	ctx := context.Background()
	bus := event.NewEventBus()
	stream := &completionEventRecorder{}
	message := &types.Message{ID: "m"}
	h := NewAgentStreamHandler(ctx, "s", "m", "r", 1, time.Now(), message, stream, bus, nil, nil, nil)
	h.Subscribe()
	steps := []types.AgentStep{{ToolCalls: []types.ToolCall{{ID: types.FAQClarificationToolCallID, Args: map[string]interface{}{"state": "saved"}}}}}
	require.NoError(t, bus.Emit(ctx, event.Event{ID: "answer", Type: event.EventAgentFinalAnswer, Data: event.AgentFinalAnswerData{Content: "国内还是海外？", Done: true}}))
	require.NoError(t, bus.Emit(ctx, event.Event{Type: event.EventAgentComplete, Data: event.AgentCompleteData{MessageID: "m", FinalAnswer: "国内还是海外？", AgentSteps: steps}}))
	require.True(t, message.IsCompleted)
	require.Equal(t, "国内还是海外？", message.Content)
	require.Equal(t, types.FAQClarificationToolCallID, message.AgentSteps[0].ToolCalls[0].ID)
	require.Equal(t, types.ResponseTypeAnswer, stream.events[0].Type)
}
