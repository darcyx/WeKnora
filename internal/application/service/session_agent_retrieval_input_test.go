package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type retrievalInputEngine struct {
	interfaces.AgentEngine
	retrievalBoundEngine
	inputID, input, query string
	called, resumed       bool
	history               []chat.Message
}

func (e *retrievalInputEngine) SetRetrievalUserMessage(id, text string) {
	e.inputID, e.input = id, text
}
func (*retrievalInputEngine) SetContextCheckpointSink(types.ContextCheckpointSink) {}
func (e *retrievalInputEngine) SetRunMode(types.RunMode)                           {}
func (e *retrievalInputEngine) Execute(_ context.Context, _, _, query string, history []chat.Message, _ ...[]string) (*types.AgentState, error) {
	e.called, e.query = true, query
	e.history = history
	return &types.AgentState{IsComplete: true}, nil
}
func (e *retrievalInputEngine) Resume(context.Context, string, string, []chat.Message, []byte, *types.AgentPrompt) (*types.AgentState, error) {
	e.called, e.resumed = true, true
	return &types.AgentState{IsComplete: true}, nil
}

type retrievalInputService struct {
	*agentService
	engine *retrievalInputEngine
}

func (s *retrievalInputService) CreateAgentEngine(context.Context, *types.AgentConfig, chat.Chat, rerank.Reranker, *event.EventBus, string, string) (interfaces.AgentEngine, error) {
	return s.engine, nil
}

func TestAgentQABindsOriginalRetrievalUserInput(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "resume"}[resume], func(t *testing.T) {
			e := &retrievalInputEngine{}
			s := &sessionService{cfg: &config.Config{}, agentService: &retrievalInputService{engine: e},
				modelService: &stubModelService{chatModel: &captureChatModel{}, modelsByID: map[string]*types.Model{
					"model": {ID: "model", Type: types.ModelTypeKnowledgeQA},
				}}}
			req := &types.QARequest{Session: &types.Session{ID: "session", TenantID: 1}, TurnLeaseHeld: true,
				UserMessageID: "u1", AssistantMessageID: "a1", Query: "已经扣款", QuotedContext: "示例订单号 EXAMPLE-123",
				CustomAgent: &types.CustomAgent{ID: "agent", TenantID: 1, Config: types.CustomAgentConfig{
					ModelID: "model", KBSelectionMode: "none", SkillsSelectionMode: "none",
					WebSearchProviderID: "unused", AllowedTools: []string{},
				}}}
			if resume {
				req.Resume = &types.AgentResume{}
			}
			ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
			require.NoError(t, s.AgentQA(ctx, req, event.NewEventBus()))
			require.True(t, e.called)
			require.Equal(t, resume, e.resumed)
			require.Equal(t, "u1", e.inputID)
			require.Equal(t, "已经扣款", e.input)
			if !resume {
				require.Contains(t, e.query, "EXAMPLE-123", "runtime context remains available to the model, separate from user provenance")
			}
		})
	}
}
