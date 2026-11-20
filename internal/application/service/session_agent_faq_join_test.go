package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type faqJoinEngine struct {
	*retrievalInputEngine
	cleaned   bool
	memory    string
	questions []string
}

func (e *faqJoinEngine) Cleanup(context.Context)                     { e.cleaned = true }
func (e *faqJoinEngine) SetMemoryPrompt(prompt string)               { e.memory = prompt }
func (e *faqJoinEngine) SetFAQPreflightQuestions(questions []string) { e.questions = questions }

type faqJoinService struct {
	*agentService
	engine *faqJoinEngine
	config *types.AgentConfig
}

func (s *faqJoinService) CreateAgentEngine(_ context.Context, cfg *types.AgentConfig, _ chat.Chat, _ rerank.Reranker, _ *event.EventBus, _, _ string) (interfaces.AgentEngine, error) {
	s.config = cfg
	return s.engine, nil
}

type faqJoinMemory struct{ interfaces.MemoryService }

func (faqJoinMemory) Recall(context.Context, string) interfaces.MemoryRecall {
	return interfaces.MemoryRecall{Prompt: "remembered"}
}

func TestAgentQAJoinsParallelFAQPreflight(t *testing.T) {
	for _, hit := range []bool{true, false} {
		t.Run(map[bool]string{true: "hit", false: "miss"}[hit], func(t *testing.T) {
			s, req, _, repo := preflightFixture(t)
			repo.exact = func(context.Context, string) ([]*types.Chunk, error) { return []*types.Chunk{repo.chunk}, nil }
			decision := `{"decision":"insufficient"}`
			if hit {
				decision = preflightSelection
			}
			model := preflightChat{call: func(context.Context, []chat.Message) (*types.ChatResponse, error) {
				return &types.ChatResponse{Content: decision, Usage: types.TokenUsage{TotalTokens: 7}}, nil
			}}
			engine := &faqJoinEngine{retrievalInputEngine: &retrievalInputEngine{}}
			svc := &faqJoinService{engine: engine}
			s.agentService, s.cfg, s.memoryService = svc, &config.Config{}, faqJoinMemory{}
			s.messageRepo = &preflightMessages{}
			s.modelService = &stubModelService{chatModel: model, modelsByID: map[string]*types.Model{
				"model": {ID: "model", Type: types.ModelTypeKnowledgeQA},
			}}
			req.Query, req.UserMessageID, req.AssistantMessageID = "支付问题", "u", "a"
			req.Session.TenantID, req.TurnLeaseHeld = 1, true
			req.CustomAgent = &types.CustomAgent{ID: "agent", TenantID: 1, Config: types.CustomAgentConfig{
				ModelID: "model", KBSelectionMode: "selected", KnowledgeBases: []string{"kb"}, SkillsSelectionMode: "none",
				WebSearchProviderID: "unused", AllowedTools: []string{tools.ToolSearchKnowledge},
			}}
			bus := event.NewEventBus()
			var answers, memories int
			bus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error { answers++; return nil })
			bus.On(event.EventMemoryRecalled, func(context.Context, event.Event) error { memories++; return nil })

			ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
			require.NoError(t, s.AgentQA(ctx, req, bus))
			require.NotNil(t, svc.config, "preparation runs alongside the preflight")
			if hit {
				require.Equal(t, 1, answers)
				require.False(t, engine.called, "a FAQ answer must not start the agent")
				require.True(t, engine.cleaned, "the unused engine must release its tools")
				require.Zero(t, memories)
				require.Empty(t, engine.memory)
				return
			}
			require.Zero(t, answers)
			require.True(t, engine.called)
			require.False(t, engine.cleaned)
			require.Equal(t, 1, memories)
			require.Equal(t, "remembered", engine.memory)
			require.NotNil(t, svc.config.FAQPreflightUsage)
			require.Equal(t, 7, svc.config.FAQPreflightUsage.TotalTokens)
			require.Equal(t, []string{"支付问题"}, svc.config.FAQPreflightQuestions)
			require.Equal(t, svc.config.FAQPreflightQuestions, engine.questions)
		})
	}
}
