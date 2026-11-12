package mcpserver

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type faqAskSession struct{ interfaces.SessionService }

func (faqAskSession) AgentQA(ctx context.Context, req *types.QARequest, bus *event.EventBus) error {
	if err := bus.Emit(ctx, event.Event{ID: "draft", Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: "withdrawn draft"}}); err != nil {
		return err
	}
	if err := bus.Emit(ctx, event.Event{ID: "draft", Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Replace: true}}); err != nil {
		return err
	}
	if err := bus.Emit(ctx, event.Event{Type: event.EventAgentFinalAnswer, Data: event.AgentFinalAnswerData{Content: "国内还是海外？", Done: true}}); err != nil {
		return err
	}
	return bus.Emit(ctx, event.Event{Type: event.EventAgentComplete, Data: event.AgentCompleteData{
		MessageID: req.AssistantMessageID, FinalAnswer: "国内还是海外？",
		AgentSteps: []types.AgentStep{{ToolCalls: []types.ToolCall{{ID: types.FAQClarificationToolCallID, Args: map[string]interface{}{"state": "saved"}}}}},
	}})
}

type faqAskMessages struct {
	interfaces.MessageService
	updated *types.Message
}

func (*faqAskMessages) CreateMessage(_ context.Context, m *types.Message) (*types.Message, error) {
	m.ID = m.Role
	return m, nil
}
func (s *faqAskMessages) UpdateMessage(_ context.Context, m *types.Message) error {
	s.updated = m
	return nil
}

func TestAskPersistsFAQClarificationState(t *testing.T) {
	messages := &faqAskMessages{}
	srv := &Server{sessionService: faqAskSession{}, messageService: messages}
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{AgentMode: types.AgentModeSmartReasoning}}
	answer, _, err := srv.runQA(context.Background(), &types.Session{ID: "session"}, agent, "如何接入", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "国内还是海外？", answer)
	require.Len(t, messages.updated.AgentSteps, 1)
	require.Equal(t, types.FAQClarificationToolCallID, messages.updated.AgentSteps[0].ToolCalls[0].ID)
}
