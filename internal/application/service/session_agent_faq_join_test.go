package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type faqJoinEngine struct {
	*retrievalInputEngine
	cleaned    bool
	cleanups   int
	onCleanup  func()
	memory     string
	questions  []string
	rejections map[string]types.FAQPreflightRejection
	broadQuery string
}

func (e *faqJoinEngine) Cleanup(context.Context) {
	e.cleaned = true
	e.cleanups++
	if e.onCleanup != nil {
		e.onCleanup()
	}
}
func (e *faqJoinEngine) SetFAQBroadDocumentSearch(query string)      { e.broadQuery = query }
func (e *faqJoinEngine) SetSteerSink(types.SteerSink)                {}
func (e *faqJoinEngine) SetMemoryPrompt(prompt string)               { e.memory = prompt }
func (e *faqJoinEngine) SetFAQPreflightQuestions(questions []string) { e.questions = questions }
func (e *faqJoinEngine) ApplyFAQPreflight(_ *types.TokenUsage, _ []string, rejections map[string]types.FAQPreflightRejection) {
	e.rejections = rejections
}

type faqJoinService struct {
	*agentService
	engine *faqJoinEngine
	config *types.AgentConfig
	create func(context.Context) error
}

func (s *faqJoinService) CreateAgentEngine(ctx context.Context, cfg *types.AgentConfig, _ chat.Chat, _ rerank.Reranker, _ *event.EventBus, _, _ string) (interfaces.AgentEngine, error) {
	s.config = cfg
	if s.create != nil {
		if err := s.create(ctx); err != nil {
			return nil, err
		}
	}
	return s.engine, nil
}

type failingRetrievalMessages struct{ preflightMessages }

func (failingRetrievalMessages) GetPreviousRetrievalState(context.Context, string, string) (*types.Message, error) {
	return nil, errors.New("retrieval state unavailable")
}

type faqJoinModelService struct {
	*stubModelService
	reranker rerank.Reranker
}

func (s *faqJoinModelService) GetRerankModel(context.Context, string) (rerank.Reranker, error) {
	return s.reranker, nil
}

// faqJoinFixture builds an AgentQA turn whose exact FAQ lookup misses so the
// semantic FAQ preflight runs in parallel with Agent preparation and replies
// with the given decision; wait, when set, blocks every preflight model call.
func faqJoinFixture(t *testing.T, decision string, wait <-chan struct{}) (*sessionService, *types.QARequest, *faqJoinService) {
	t.Helper()
	s, req, _, _ := preflightFixture(t)
	model := preflightChat{call: func(ctx context.Context, _ []chat.Message) (*types.ChatResponse, error) {
		if wait != nil {
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &types.ChatResponse{Content: decision, Usage: types.TokenUsage{TotalTokens: 7}}, nil
	}}
	svc := &faqJoinService{engine: &faqJoinEngine{retrievalInputEngine: &retrievalInputEngine{}}}
	s.agentService, s.cfg, s.memoryService = svc, &config.Config{}, faqJoinMemory{}
	s.messageRepo = &preflightMessages{}
	s.modelService = &faqJoinModelService{
		stubModelService: &stubModelService{chatModel: model, modelsByID: map[string]*types.Model{
			"model": {ID: "model", Type: types.ModelTypeKnowledgeQA},
		}},
		reranker: &scoredReranker{scores: []float64{0.99}},
	}
	req.Query, req.UserMessageID, req.AssistantMessageID = "支付问题", "u", "a"
	req.Session.TenantID, req.TurnLeaseHeld = 1, true
	req.CustomAgent = &types.CustomAgent{ID: "agent", TenantID: 1, Config: types.CustomAgentConfig{
		ModelID: "model", RerankModelID: "rerank", KBSelectionMode: "selected", KnowledgeBases: []string{"kb"}, SkillsSelectionMode: "none",
		WebSearchProviderID: "unused", AllowedTools: []string{tools.ToolSearchKnowledge},
	}}
	return s, req, svc
}

func TestAgentQAPublishesFAQAnswerAfterPreparationStops(t *testing.T) {
	entered := make(chan struct{})
	s, req, svc := faqJoinFixture(t, preflightSelection, entered)
	var preparing atomic.Bool
	svc.create = func(ctx context.Context) error {
		preparing.Store(true)
		defer preparing.Store(false)
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return errors.New("FAQ hit did not stop the preparation")
		}
	}
	bus := event.NewEventBus()
	var completes, overlapped atomic.Int32
	bus.On(event.EventAgentComplete, func(context.Context, event.Event) error {
		completes.Add(1)
		if preparing.Load() {
			overlapped.Add(1)
		}
		return nil
	})
	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	require.NoError(t, s.AgentQA(ctx, req, bus), "the FAQ answer owns the turn")
	require.EqualValues(t, 1, completes.Load())
	require.Zero(t, overlapped.Load(), "Complete must follow the stopped preparation")
	require.False(t, svc.engine.called)
}

func TestAgentQACleansEngineWhenRetrievalBindFails(t *testing.T) {
	s, req, svc := faqJoinFixture(t, `{"decision":"insufficient"}`, nil)
	s.messageRepo = &failingRetrievalMessages{}
	req.CustomAgent.Config.MultiTurnEnabled = true
	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	err := s.AgentQA(ctx, req, event.NewEventBus())
	require.ErrorContains(t, err, "load previous retrieval state")
	require.True(t, svc.engine.cleaned)
	require.False(t, svc.engine.called)
}

type faqJoinMemory struct{ interfaces.MemoryService }

func (faqJoinMemory) Recall(context.Context, string) interfaces.MemoryRecall {
	return interfaces.MemoryRecall{Prompt: "remembered"}
}

func TestAgentQAJoinsParallelFAQPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, decision string
		hit, live      bool
	}{
		{name: "hit", decision: preflightSelection, hit: true},
		{name: "miss", decision: `{"decision":"insufficient"}`},
		{name: "needs_live_data", decision: `{"decision":"needs_live_data"}`, live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, req, svc := faqJoinFixture(t, tc.decision, nil)
			engine := svc.engine
			bus := event.NewEventBus()
			var answers, memories int
			bus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error { answers++; return nil })
			bus.On(event.EventMemoryRecalled, func(context.Context, event.Event) error { memories++; return nil })

			ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
			require.NoError(t, s.AgentQA(ctx, req, bus))
			require.NotNil(t, svc.config, "preparation runs alongside the preflight")
			if tc.hit {
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
			if tc.live {
				require.Equal(t, "支付问题", engine.rejections["faq"].Question)
				require.Contains(t, engine.rejections["faq"].Reason, "requires live data")
			} else {
				require.Empty(t, engine.rejections)
			}
		})
	}
}

func TestAgentQAUsesFAQPreflightWithWebSearchEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, decision string
		hit            bool
	}{
		{name: "covered FAQ", decision: preflightSelection, hit: true},
		{name: "insufficient FAQ", decision: `{"decision":"insufficient"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, req, svc := faqJoinFixture(t, tc.decision, nil)
			req.WebSearchEnabled = true
			req.CustomAgent.Config.WebSearchEnabled = true
			req.CustomAgent.Config.AllowedTools = append(req.CustomAgent.Config.AllowedTools, tools.ToolWebSearch)
			bus := event.NewEventBus()
			var answers int
			bus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error { answers++; return nil })
			ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
			require.NoError(t, s.AgentQA(ctx, req, bus))
			require.NotNil(t, svc.config)
			require.True(t, svc.config.WebSearchEnabled)
			require.Contains(t, svc.config.AllowedTools, tools.ToolWebSearch)
			if tc.hit {
				require.Equal(t, 1, answers)
				require.False(t, svc.engine.called)
			} else {
				require.Zero(t, answers)
				require.True(t, svc.engine.called)
				require.Equal(t, []string{"支付问题"}, svc.engine.questions)
			}
		})
	}
}

type recordingTurnSandbox struct {
	sandbox.Manager
	released atomic.Bool
	events   *[]string
}

func (m *recordingTurnSandbox) BeginSessionTurn(ctx context.Context, _ string) (context.Context, func(), error) {
	return ctx, func() {
		m.released.Store(true)
		if m.events != nil {
			*m.events = append(*m.events, "release_turn")
		}
	}, nil
}

func TestAgentQASkipsFAQPreflightForDocumentOnlyKBs(t *testing.T) {
	s, req, svc := faqJoinFixture(t, preflightSelection, nil)
	docKBs := &docOnlyPreflightKBs{kb: &types.KnowledgeBase{ID: "kb", TenantID: 1, Type: types.KnowledgeBaseTypeDocument}}
	s.knowledgeBaseService = docKBs
	repo := s.chunkService.(preflightChunks).repo
	var exactCalls atomic.Int32
	repo.exact = func(context.Context, string) ([]*types.Chunk, error) {
		exactCalls.Add(1)
		return nil, nil
	}

	bus := event.NewEventBus()
	var answers int
	bus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error { answers++; return nil })

	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	require.NoError(t, s.AgentQA(ctx, req, bus))
	require.Zero(t, answers)
	require.Zero(t, exactCalls.Load(), "document-only KBs must skip FAQ preflight without querying FAQ chunks")
	require.EqualValues(t, 1, docKBs.getByIDsCalls.Load(), "KB metadata must only be fetched once in buildSearchTargets")
	require.True(t, svc.engine.called)
}

type docOnlyPreflightKBs struct {
	interfaces.KnowledgeBaseService
	kb            *types.KnowledgeBase
	getByIDsCalls atomic.Int32
}

func (s *docOnlyPreflightKBs) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	s.getByIDsCalls.Add(1)
	return []*types.KnowledgeBase{s.kb}, nil
}

type stagingTrackingService struct {
	*faqJoinService
	inputStore sandbox.SessionFileStore
	stageCalls atomic.Int32
}

func (s *stagingTrackingService) sessionSandboxInputStore(context.Context, string, string) (sandbox.SessionFileStore, error) {
	return s.inputStore, nil
}

func (s *stagingTrackingService) stageSessionAttachments(
	context.Context, string, string, uint64, types.MessageAttachments, sandbox.WorkspaceLayout,
) ([]stagedSessionAttachment, error) {
	s.stageCalls.Add(1)
	return nil, nil
}

type faqStagingFileStore struct {
	sandbox.SessionFileStore
}

type emptyAttachmentMessages struct{ preflightMessages }

func (*emptyAttachmentMessages) GetSessionAttachments(context.Context, string) (types.MessageAttachments, error) {
	return nil, nil
}

func TestAgentQADefersEmptyAttachmentStagingUntilFAQMisses(t *testing.T) {
	for _, hit := range []bool{true, false} {
		t.Run(map[bool]string{true: "hit", false: "miss"}[hit], func(t *testing.T) {
			decision := `{"decision":"insufficient"}`
			if hit {
				decision = preflightSelection
			}
			s, req, svc := faqJoinFixture(t, decision, nil)
			tracker := &stagingTrackingService{faqJoinService: svc, inputStore: faqStagingFileStore{}}
			s.agentService = tracker
			s.messageRepo = &emptyAttachmentMessages{}

			ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
			require.NoError(t, s.AgentQA(ctx, req, event.NewEventBus()))
			if hit {
				require.Zero(t, tracker.stageCalls.Load(), "FAQ hit must not wake remote sandbox staging when session has no attachments")
				require.False(t, svc.engine.called)
				return
			}
			require.EqualValues(t, 1, tracker.stageCalls.Load(), "FAQ miss must still reconcile sandbox staging before execution")
			require.True(t, svc.engine.called)
		})
	}
}

func TestAgentQAReleasesTurnLeaseAfterFAQPublish(t *testing.T) {
	entered := make(chan struct{})
	s, req, svc := faqJoinFixture(t, preflightSelection, entered)
	req.TurnLeaseHeld = false
	var order []string
	mgr := &recordingTurnSandbox{events: &order}
	s.sandboxMgr = mgr
	svc.create = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	bus := event.NewEventBus()
	bus.On(event.EventAgentComplete, func(context.Context, event.Event) error {
		require.False(t, mgr.released.Load(), "sandbox turn lease must still be held when Complete is published")
		order = append(order, "publish_complete")
		return nil
	})

	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	require.NoError(t, s.AgentQA(ctx, req, bus))
	require.Equal(t, []string{"publish_complete", "release_turn"}, order)
}

// withdrawingSteerSink reports a pending steer only once steered is set, so the FAQ
// evaluation completes with an answer that the steer then withdraws.
type withdrawingSteerSink struct {
	types.SteerSink
	steered atomic.Bool
}

func (s *withdrawingSteerSink) PollSteer(context.Context, string, string, int) ([]map[string]interface{}, int, error) {
	if s.steered.Load() {
		return []map[string]interface{}{{"id": "steer", "content": "换个问题"}}, 0, nil
	}
	return nil, 0, nil
}

func TestAgentQARunsAgentWhenSteerWithdrawsFAQAnswer(t *testing.T) {
	s, req, svc := faqJoinFixture(t, preflightSelection, nil)
	sink := &withdrawingSteerSink{}
	req.SteerSink = sink
	var creates atomic.Int32
	svc.create = func(ctx context.Context) error {
		if creates.Add(1) > 1 {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			return errors.New("FAQ hit did not stop the preparation")
		}
		// The user steers after the FAQ answer stopped the preparation, which
		// still hands back a half-built engine.
		sink.steered.Store(true)
		return nil
	}
	var executes int
	svc.engine.run = func(ctx context.Context) error {
		executes++
		return ctx.Err()
	}
	bus := event.NewEventBus()
	var answers int
	bus.On(event.EventAgentFinalAnswer, func(context.Context, event.Event) error { answers++; return nil })

	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	require.NoError(t, s.AgentQA(ctx, req, bus), "a withdrawn FAQ answer must not fail the turn")
	require.EqualValues(t, 2, creates.Load(), "the stopped preparation is redone")
	require.Equal(t, 1, executes, "the Agent runs exactly once")
	require.Zero(t, answers, "the withdrawn FAQ answer is not published")
	require.Equal(t, 1, svc.engine.cleanups, "the half-built engine is released exactly once")
	require.Equal(t, "remembered", svc.engine.memory)
}

func TestAgentQAPublishesFAQBeforeEngineCleanupAndLeaseRelease(t *testing.T) {
	s, req, svc := faqJoinFixture(t, preflightSelection, nil)
	req.TurnLeaseHeld = false
	var order []string
	s.sandboxMgr = &recordingTurnSandbox{events: &order}
	svc.engine.onCleanup = func() {
		order = append(order, "engine_cleanup")
	}
	bus := event.NewEventBus()
	bus.On(event.EventAgentComplete, func(context.Context, event.Event) error {
		order = append(order, "publish_complete")
		return nil
	})

	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	require.NoError(t, s.AgentQA(ctx, req, bus))
	require.Equal(t, []string{"publish_complete", "engine_cleanup", "release_turn"}, order)
}

func TestAgentQARepreparePreservesAgentConfigAcrossMCPMutation(t *testing.T) {
	s, req, svc := faqJoinFixture(t, preflightSelection, nil)
	req.CustomAgent.Config.MCPSelectionMode = "all"
	req.CustomAgent.Config.MCPServices = []string{"mcp-1", "mcp-2"}
	sink := &withdrawingSteerSink{}
	req.SteerSink = sink

	var seenModes []string
	var seenServices [][]string
	svc.create = func(ctx context.Context) error {
		seenModes = append(seenModes, svc.config.MCPSelectionMode)
		seenServices = append(seenServices, append([]string(nil), svc.config.MCPServices...))
		if len(seenModes) == 1 {
			// Simulate registerMCPTools mutating config in place before cancel.
			svc.config.MCPSelectionMode = "selected"
			svc.config.MCPServices = nil
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				return errors.New("FAQ hit did not stop the preparation")
			}
			sink.steered.Store(true)
			return ctx.Err()
		}
		return nil
	}

	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	require.NoError(t, s.AgentQA(ctx, req, event.NewEventBus()))
	require.Equal(t, []string{"all", "all"}, seenModes, "retry must receive pristine MCPSelectionMode")
	require.Equal(t, [][]string{{"mcp-1", "mcp-2"}, {"mcp-1", "mcp-2"}}, seenServices, "retry must preserve MCPServices after first attempt mutated config")
	require.True(t, svc.engine.called)
}

func TestAgentQAPassesOnlyConfirmedBroadFAQMiss(t *testing.T) {
	for _, tc := range []struct {
		name, decision string
		want           bool
	}{
		{"confirmed", `{"decision":"insufficient","broad_document_search":true}`, true},
		{"unspecified", `{"decision":"insufficient"}`, false},
		{"restricted", `{"decision":"insufficient","broad_document_search":false}`, false},
		{"live", `{"decision":"needs_live_data","broad_document_search":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, req, svc := faqJoinFixture(t, tc.decision, nil)
			ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
			require.NoError(t, s.AgentQA(ctx, req, event.NewEventBus()))
			if tc.want {
				require.Equal(t, req.Query, svc.engine.broadQuery)
			} else {
				require.Empty(t, svc.engine.broadQuery)
			}
		})
	}
}

func TestFAQBroadDocumentSearchChecksOlderUserSourceRestriction(t *testing.T) {
	history := make([]chat.Message, 29)
	for i := range history {
		history[i] = chat.Message{Role: "user", Content: "只有一周以后才失败，是什么原因？"}
	}
	require.False(t, faqHistoryRestrictsSources(history))
	history[0].Content = "本次会话支付问题依据《Google支付配置》作答，排除其他材料"
	require.True(t, faqHistoryRestrictsSources(history))
	history[0].Role = "assistant"
	require.False(t, faqHistoryRestrictsSources(history), "assistant prose is not a user source restriction")
}

func TestFAQSourceHistoryIsCompleteOrDisablesBroadSearch(t *testing.T) {
	history := make([]chat.Message, 29)
	for i := range history {
		history[i] = chat.Message{Role: "user", Content: "支付只有一周后才失败"}
	}
	history[0].Content = "本次会话支付问题依据《Google支付配置》作答，排除其他材料"
	messages, complete := faqSourceHistory(history)
	require.True(t, complete)
	require.Len(t, messages, 29)
	require.Equal(t, history[0].Content, messages[0])
	history[1].Content = strings.Repeat("a", 16001)
	_, complete = faqSourceHistory(history)
	require.False(t, complete)
}
