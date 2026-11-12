package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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
	run                   func(context.Context) error
}

func (e *retrievalInputEngine) SetRetrievalUserMessage(id, text string) {
	e.inputID, e.input = id, text
}
func (*retrievalInputEngine) SetContextCheckpointSink(types.ContextCheckpointSink) {}
func (e *retrievalInputEngine) SetRunMode(types.RunMode)                           {}
func (e *retrievalInputEngine) Execute(ctx context.Context, _, _, query string, history []chat.Message, _ ...[]string) (*types.AgentState, error) {
	e.called, e.query = true, query
	e.history = history
	if e.run != nil {
		return nil, e.run(ctx)
	}
	return &types.AgentState{IsComplete: true}, nil
}
func (e *retrievalInputEngine) Resume(context.Context, string, string, []chat.Message, []byte, *types.AgentPrompt) (*types.AgentState, error) {
	e.called, e.resumed = true, true
	return &types.AgentState{IsComplete: true}, nil
}

type retrievalInputService struct {
	*agentService
	engine *retrievalInputEngine
	config *types.AgentConfig
}

func (s *retrievalInputService) CreateAgentEngine(_ context.Context, cfg *types.AgentConfig, _ chat.Chat, _ rerank.Reranker, _ *event.EventBus, _, _ string) (interfaces.AgentEngine, error) {
	s.config = cfg
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

func TestAgentQATotalDeadlineWiring(t *testing.T) {
	for _, mode := range []string{"parent-deadline", "expired-before-engine", "engine-error"} {
		t.Run(mode, func(t *testing.T) {
			e := &retrievalInputEngine{}
			bus := event.NewEventBus()
			events := 0
			bus.On(event.EventError, func(context.Context, event.Event) error { events++; return nil })
			s := &sessionService{cfg: &config.Config{Agent: &config.AgentConfig{QATotalTimeoutSeconds: 10}}, agentService: &retrievalInputService{engine: e}, modelService: &stubModelService{chatModel: &captureChatModel{}, modelsByID: map[string]*types.Model{"model": {ID: "model", Type: types.ModelTypeKnowledgeQA}}}}
			req := &types.QARequest{Session: &types.Session{ID: "session", TenantID: 1}, TurnLeaseHeld: true, UserMessageID: "u1", AssistantMessageID: "a1", Query: "question", CustomAgent: &types.CustomAgent{ID: "agent", TenantID: 1, Config: types.CustomAgentConfig{ModelID: "model", KBSelectionMode: "none", SkillsSelectionMode: "none", WebSearchProviderID: "unused", AllowedTools: []string{}}}}
			parent, cancel := context.WithTimeout(context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1)), 5*time.Second)
			defer cancel()
			deadline, _ := parent.Deadline()
			e.run = func(ctx context.Context) error {
				got, ok := ctx.Deadline()
				require.True(t, ok)
				require.Equal(t, deadline, got)
				if mode == "engine-error" {
					_ = bus.Emit(ctx, event.Event{Type: event.EventError})
					return errors.New("engine already emitted")
				}
				return nil
			}
			if mode == "expired-before-engine" {
				cancel()
			}
			err := s.AgentQA(parent, req, bus)
			if mode == "expired-before-engine" {
				require.ErrorIs(t, err, context.Canceled)
				require.False(t, e.called)
				return
			}
			require.NoError(t, err)
			require.True(t, e.called)
			if mode == "engine-error" {
				require.Equal(t, 1, events)
			} else {
				require.Zero(t, events)
			}
		})
	}
}

func TestResourceNavigationEligibility(t *testing.T) {
	base := types.QARequest{Session: &types.Session{}, Query: "服务端文档是什么"}
	for _, tc := range []struct {
		name  string
		alter func(*types.QARequest, *types.AgentConfig)
		want  bool
	}{
		{"resource overview", func(*types.QARequest, *types.AgentConfig) {}, true},
		{"other resource", func(r *types.QARequest, _ *types.AgentConfig) { r.Query = "客户端文档入口在哪里？" }, true},
		{"default project context", func(r *types.QARequest, _ *types.AgentConfig) { r.AppInfo = `{"gameInfo":{}}` }, true},
		{"single fact", func(r *types.QARequest, _ *types.AgentConfig) { r.Query = "一次最多多少条？" }, false},
		{"code", func(r *types.QARequest, _ *types.AgentConfig) {
			r.Query = "服务端文档是什么，如何调用API？"
		}, false},
		{"multi-part", func(r *types.QARequest, _ *types.AgentConfig) {
			r.Query = "服务端文档是什么？还有客户端文档呢？"
		}, false},
		{"detail", func(r *types.QARequest, _ *types.AgentConfig) { r.Query = "服务端文档是什么，请详细分析" }, false},
		{"live status", func(r *types.QARequest, _ *types.AgentConfig) { r.Query = "当前项目服务端配置是什么" }, false},
		{"explicit thinking", func(r *types.QARequest, _ *types.AgentConfig) { r.ReasoningEffort = "minimal" }, false},
		{"high configured", func(_ *types.QARequest, c *types.AgentConfig) { c.ReasoningEffort = "high" }, false},
		{"pinned skill", func(r *types.QARequest, _ *types.AgentConfig) { r.SkillNames = []string{"x"} }, false},
		{"planning", func(_ *types.QARequest, c *types.AgentConfig) { c.PlanningEnabled = true }, false},
		{"image", func(r *types.QARequest, _ *types.AgentConfig) { r.ImageURLs = []string{"x"} }, false},
		{"resume", func(r *types.QARequest, _ *types.AgentConfig) { r.Resume = &types.AgentResume{} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			c := types.AgentConfig{ReasoningEffort: "minimal"}
			tc.alter(&r, &c)
			require.Equal(t, tc.want, eligibleResourceNavigation(&r, &c))
		})
	}
}

func TestAgentQAPassesResourceNavigationEligibilityToEngine(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[explicit], func(t *testing.T) {
			e := &retrievalInputEngine{}
			svc := &retrievalInputService{engine: e}
			s := &sessionService{cfg: &config.Config{}, agentService: svc, modelService: &stubModelService{chatModel: &captureChatModel{}, modelsByID: map[string]*types.Model{"model": {ID: "model", Type: types.ModelTypeKnowledgeQA}}}}
			req := &types.QARequest{Session: &types.Session{ID: "session", TenantID: 1}, TurnLeaseHeld: true, UserMessageID: "u", AssistantMessageID: "a", Query: "服务端文档是什么", AppInfo: `{"gameInfo":{}}`, CustomAgent: &types.CustomAgent{ID: "agent", TenantID: 1, Config: types.CustomAgentConfig{ModelID: "model", ReasoningEffort: "minimal", KBSelectionMode: "none", SkillsSelectionMode: "none", WebSearchProviderID: "unused"}}}
			if explicit {
				req.ReasoningEffort = "minimal"
			}
			ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
			require.NoError(t, s.AgentQA(ctx, req, event.NewEventBus()))
			require.True(t, e.called)
			require.NotNil(t, svc.config)
			require.Equal(t, !explicit, svc.config.ResourceNavigationFastAnswer)
		})
	}
}

func TestResourceNavigationEligibilityIsNotInherited(t *testing.T) {
	encoded, err := json.Marshal(types.AgentConfig{ResourceNavigationFastAnswer: true})
	require.NoError(t, err)
	var inherited types.AgentConfig
	require.NoError(t, json.Unmarshal(encoded, &inherited))
	require.False(t, inherited.ResourceNavigationFastAnswer)
	req := &types.QARequest{Session: &types.Session{Kind: types.SessionKindAgentDelegated}, Query: "服务端文档是什么"}
	require.False(t, eligibleResourceNavigation(req, &types.AgentConfig{ReasoningEffort: "minimal"}))
}
