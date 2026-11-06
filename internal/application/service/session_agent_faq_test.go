package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type faqMatchKBService struct {
	interfaces.KnowledgeBaseService
	results []*types.SearchResult
}

func (faqMatchKBService) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	return []*types.KnowledgeBase{{ID: "kb", Type: types.KnowledgeBaseTypeFAQ}}, nil
}
func (faqMatchKBService) ResolveEmbeddingModelKeys(context.Context, []*types.KnowledgeBase) map[string]string {
	return nil
}
func (s faqMatchKBService) HybridSearch(context.Context, string, types.SearchParams) ([]*types.SearchResult, error) {
	return s.results, nil
}

type faqMatchRepository struct {
	interfaces.ChunkRepository
	chunks []*types.Chunk
	err    error
}

func (r faqMatchRepository) FindExactFAQQuestions(context.Context, *types.SearchTarget, string) ([]*types.Chunk, error) {
	return r.chunks, r.err
}

type faqMatchChunkService struct {
	interfaces.ChunkService
	repo interfaces.ChunkRepository
}

func (s faqMatchChunkService) GetRepository() interfaces.ChunkRepository { return s.repo }

type faqMatchMessageRepo struct {
	interfaces.MessageRepository
	messages []*types.Message
}

func (r faqMatchMessageRepo) GetRecentMessagesBySession(context.Context, string, int) ([]*types.Message, error) {
	return r.messages, nil
}

func mustPendingFAQ(t *testing.T, s *sessionService, req *types.QARequest) *pendingFAQClarification {
	t.Helper()
	pending, err := s.pendingFAQClarification(context.Background(), req)
	require.NoError(t, err)
	return pending
}

func (s *sessionService) tryAgentFAQClarification(ctx context.Context, req *types.QARequest, query string, cfg *types.AgentConfig, bus *event.EventBus, model chat.Chat, reranker rerank.Reranker, history []chat.Message, matches []tools.FAQMatch, pending *pendingFAQClarification) (bool, error) {
	if !faqDirectEligible(req, cfg, bus) {
		return false, nil
	}
	evaluation := &agentFAQEvaluation{Query: query}
	s.evaluateAgentFAQClarification(ctx, req, query, cfg, model, reranker, history, matches, pending, evaluation)
	return emitAgentFAQEvaluation(ctx, req, bus, evaluation)
}

type faqMatchModel struct {
	response string
	usage    types.TokenUsage
	onChat   func()
}

type faqMatchReranker struct{}

func (faqMatchReranker) Rerank(_ context.Context, _ string, docs []string) ([]rerank.RankResult, error) {
	result := make([]rerank.RankResult, len(docs))
	for i := range docs {
		result[i] = rerank.RankResult{Index: i, RelevanceScore: 0.9}
	}
	return result, nil
}
func (faqMatchReranker) GetModelName() string { return "faq-test" }
func (faqMatchReranker) GetModelID() string   { return "faq-test" }

type faqPendingSteer struct{ types.SteerSink }

func (faqPendingSteer) PollSteer(context.Context, string, string, int) ([]map[string]interface{}, int, error) {
	return []map[string]interface{}{{"content": "补充条件"}}, 1, nil
}

func (m faqMatchModel) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	if m.onChat != nil {
		m.onChat()
	}
	return &types.ChatResponse{Content: m.response, Usage: m.usage}, nil
}
func (faqMatchModel) ChatStream(context.Context, []chat.Message, *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, nil
}
func (faqMatchModel) GetModelName() string { return "test" }
func (faqMatchModel) GetModelID() string   { return "test" }

func TestAgentFAQExactFailurePreservesCause(t *testing.T) {
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{},
		chunkService: faqMatchChunkService{repo: faqMatchRepository{err: context.DeadlineExceeded}}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "问题"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge},
		SearchTargets: types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}}
	_, err := svc.findAgentExactFAQs(t.Context(), req, req.Query, cfg)
	require.ErrorIs(t, err, errFAQMatchUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the service must retain the actual typed cause for bounded recovery")
	var incomplete *tools.FAQSearchError
	require.ErrorAs(t, err, &incomplete)
	require.Len(t, incomplete.Failures, 1)
	require.Equal(t, "faq_exact", incomplete.Failures[0].Stage)
	require.Equal(t, cfg.SearchTargets, incomplete.Failures[0].Targets)
}

type faqPartialMatchRepository struct{ faqMatchRepository }

func (r faqPartialMatchRepository) FindExactFAQQuestions(ctx context.Context, target *types.SearchTarget, query string) ([]*types.Chunk, error) {
	if len(target.KnowledgeIDs) > 0 && target.KnowledgeIDs[0] == "failed" {
		return nil, context.DeadlineExceeded
	}
	return r.faqMatchRepository.FindExactFAQQuestions(ctx, target, query)
}

func TestAgentFAQPartialMatchDoesNotBecomeUniqueAnswer(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "问题", Answers: []string{"标准答案"}})
	require.NoError(t, err)
	chunk := &types.Chunk{ID: "faq", KnowledgeBaseID: "kb", KnowledgeID: "good", ChunkType: types.ChunkTypeFAQ, Metadata: meta}
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{}, chunkService: faqMatchChunkService{
		repo: faqPartialMatchRepository{faqMatchRepository{chunks: []*types.Chunk{chunk}}},
	}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "问题", AssistantMessageID: "message"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, SearchTargets: types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"failed"}},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"good"}},
	}}
	bus := event.NewEventBus()
	var events []event.Event
	for _, kind := range []event.EventType{event.EventAgentReferences, event.EventAgentFinalAnswer, event.EventAgentComplete} {
		bus.On(kind, func(_ context.Context, e event.Event) error { events = append(events, e); return nil })
	}
	result, err := svc.findAgentExactFAQs(t.Context(), req, req.Query, cfg)
	require.ErrorIs(t, err, errFAQMatchUnavailable)
	require.Len(t, result.Matches, 1, "the caller receives evidence together with its incomplete status")
	require.Equal(t, "faq", result.Matches[0].Result.ID)
	var incomplete *tools.FAQSearchError
	require.ErrorAs(t, err, &incomplete)
	require.Equal(t, types.SearchTargets{cfg.SearchTargets[0]}, incomplete.Failures[0].Targets)
	scopeCalls := 0
	answered, err := svc.tryAgentFAQ(t.Context(), req, cfg, bus, faqMatchModel{
		response: `{"selected_id":"faq","scope_match":"same"}`,
		onChat:   func() { scopeCalls++ },
	}, nil, nil)
	require.NoError(t, err)
	require.False(t, answered)
	require.Zero(t, scopeCalls, "an incomplete candidate set cannot reach uniqueness or scope confirmation")
	require.Empty(t, events, "no answer, references or completion may be published from a partial match")
}

func TestAgentFAQDirectEmitsNormalAnswer(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "问题", Answers: []string{"标准答案"}})
	require.NoError(t, err)
	chunk := &types.Chunk{ID: "faq", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: meta}
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{}, chunkService: faqMatchChunkService{repo: faqMatchRepository{chunks: []*types.Chunk{chunk}}}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "问题", AssistantMessageID: "message"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}}
	bus := event.NewEventBus()
	var events []event.Event
	for _, kind := range []event.EventType{event.EventAgentReferences, event.EventAgentFinalAnswer, event.EventAgentComplete} {
		bus.On(kind, func(_ context.Context, e event.Event) error { events = append(events, e); return nil })
	}
	answered, err := svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{"selected_id":"faq","scope_match":"same"}`}, nil, nil)
	require.NoError(t, err)
	require.True(t, answered)
	require.Len(t, events, 3)
	require.Equal(t, "标准答案", events[1].Data.(event.AgentFinalAnswerData).Content)
	complete := events[2].Data.(event.AgentCompleteData)
	require.Equal(t, "message", complete.MessageID)
	require.Equal(t, "标准答案", complete.FinalAnswer)
	require.Len(t, complete.KnowledgeRefs, 1)
	req.SteerSink = faqPendingSteer{}
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{}`}, nil, nil)
	require.NoError(t, err)
	require.False(t, answered)
	require.Len(t, events, 3)
	req.SteerSink = nil

	req.Query = "另一个问题"
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{}`}, nil, nil)
	require.NoError(t, err)
	require.False(t, answered)
	require.Len(t, events, 3)

	// A duplicate across the authorized scope is not eligible for direct output.
	svc.chunkService = faqMatchChunkService{repo: faqMatchRepository{chunks: []*types.Chunk{chunk, {
		ID: "other", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: meta,
	}}}}
	req.Query = "问题"
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{}`}, nil, nil)
	require.NoError(t, err)
	require.False(t, answered)
	require.Len(t, events, 3)
	svc.chunkService = faqMatchChunkService{repo: faqMatchRepository{err: errors.New("database unavailable")}}
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{}, nil, nil)
	require.NoError(t, err)
	require.False(t, answered)
	require.Len(t, events, 3)
}

func TestFAQEligibilityUsesEffectiveWebSearchConfiguration(t *testing.T) {
	req := &types.QARequest{Session: &types.Session{ID: "session"}, WebSearchEnabled: true}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}}
	bus := event.NewEventBus()
	require.True(t, faqDirectEligible(req, cfg, bus), "a request flag alone does not enable web search")
	cfg.WebSearchEnabled = true
	require.False(t, faqDirectEligible(req, cfg, bus), "effective web search must keep the normal agent flow")
}

func TestExactFAQWithContextRequiresModelConfirmation(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "如何接入", Answers: []string{"仅适用于国内"}})
	require.NoError(t, err)
	chunk := &types.Chunk{ID: "faq", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: meta}
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{}, chunkService: faqMatchChunkService{repo: faqMatchRepository{chunks: []*types.Chunk{chunk}}}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "如何接入", AppInfo: `{"region":"海外"}`, AssistantMessageID: "m"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}}
	bus := event.NewEventBus()
	called := false
	answered, err := svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{}`, onChat: func() { called = true }}, nil, nil)
	require.NoError(t, err)
	require.True(t, called)
	require.False(t, answered, "a context-conflicting FAQ must fall back to the agent")
}

func TestAgentFAQClarificationContinuesAsOrdinaryMessage(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "如何接入", Answers: []string{"答案"}})
	require.NoError(t, err)
	chunks := []*types.Chunk{
		{ID: "domestic", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: meta},
		{ID: "overseas", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: meta},
	}
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{}, chunkService: faqMatchChunkService{repo: faqMatchRepository{chunks: chunks}}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, CustomAgent: &types.CustomAgent{ID: "agent-1"}, Query: "如何接入", UserMessageID: "user-1", AssistantMessageID: "assistant-1"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}}
	bus := event.NewEventBus()
	var completed event.AgentCompleteData
	bus.On(event.EventAgentComplete, func(_ context.Context, e event.Event) error { completed = e.Data.(event.AgentCompleteData); return nil })
	result, err := svc.findAgentExactFAQs(context.Background(), req, req.Query, cfg)
	require.NoError(t, err)
	require.Len(t, result.Matches, 2)
	answered, err := svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, bus, faqMatchModel{response: `{"question":"国内还是海外？","scope_match":"missing"}`}, nil, nil, result.Matches, nil)
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "国内还是海外？", completed.FinalAnswer)
	steps := completed.AgentSteps.([]types.AgentStep)
	svc.messageRepo = faqMatchMessageRepo{messages: []*types.Message{
		{ID: "assistant-1", Role: "assistant", IsCompleted: true, Content: completed.FinalAnswer, AgentSteps: steps},
		{ID: "user-2", Role: "user", Content: "海外"},
	}}
	req.Query, req.UserMessageID, req.AssistantMessageID = "海外", "user-2", "assistant-2"
	req.UserMessageID = ""
	require.NotNil(t, mustPendingFAQ(t, svc, req))
	req.UserMessageID = "user-2"
	otherMeta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "海外", Answers: []string{"无关答案"}})
	require.NoError(t, err)
	svc.chunkService = faqMatchChunkService{repo: faqMatchRepository{chunks: append(append([]*types.Chunk(nil), chunks...), &types.Chunk{
		ID: "unrelated", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: otherMeta,
	})}}
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{"selected_id":"overseas","scope_match":"same"}`}, nil, nil)
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "答案", completed.FinalAnswer)
	require.Equal(t, "overseas", completed.KnowledgeRefs[0].(*types.SearchResult).ID)
	req.CustomAgent = &types.CustomAgent{ID: "agent-2"}
	answered, err = svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, bus, faqMatchModel{response: `{"selected_id":"overseas","scope_match":"same"}`}, nil, nil, nil, mustPendingFAQ(t, svc, req))
	require.NoError(t, err)
	require.False(t, answered)
	req.CustomAgent = &types.CustomAgent{ID: "agent-1"}

	// A changed subject or an invalid candidate never triggers a direct answer.
	answered, err = svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, bus, faqMatchModel{response: `{}`, usage: types.TokenUsage{TotalTokens: 12}}, nil, nil, nil, mustPendingFAQ(t, svc, req))
	require.NoError(t, err)
	require.False(t, answered)
	require.Equal(t, 12, cfg.FAQPreflightUsage.TotalTokens)
	answered, err = svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, bus, faqMatchModel{response: `{"selected_id":"unknown","scope_match":"same"}`}, nil, nil, nil, mustPendingFAQ(t, svc, req))
	require.NoError(t, err)
	require.False(t, answered)
	changedMeta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "如何接入", Answers: []string{"变更后的答案"}})
	require.NoError(t, err)
	changed := *chunks[1]
	changed.Metadata = changedMeta
	svc.chunkService = faqMatchChunkService{repo: faqMatchRepository{chunks: []*types.Chunk{chunks[0], &changed}}}
	answered, err = svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, bus, faqMatchModel{response: `{"selected_id":"overseas","scope_match":"same"}`}, nil, nil, nil, mustPendingFAQ(t, svc, req))
	require.NoError(t, err)
	require.False(t, answered)
	// Revoked or removed candidates invalidate the saved clarification.
	svc.chunkService = faqMatchChunkService{repo: faqMatchRepository{chunks: chunks[:1]}}
	answered, err = svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, bus, faqMatchModel{response: `{"selected_id":"overseas","scope_match":"same"}`}, nil, nil, nil, mustPendingFAQ(t, svc, req))
	require.NoError(t, err)
	require.False(t, answered)
}

func TestAgentFAQSemanticCandidatesAskAndResolve(t *testing.T) {
	domestic, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "国内如何接入", Answers: []string{"国内答案"}})
	require.NoError(t, err)
	overseas, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "海外如何接入", Answers: []string{"海外答案"}})
	require.NoError(t, err)
	results := []*types.SearchResult{
		{ID: "domestic", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: string(types.ChunkTypeFAQ), Content: "国内如何接入", ChunkMetadata: domestic},
		{ID: "overseas", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: string(types.ChunkTypeFAQ), Content: "海外如何接入", ChunkMetadata: overseas},
	}
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{results: results}, chunkService: faqMatchChunkService{repo: faqMatchRepository{chunks: []*types.Chunk{
		{ID: "domestic", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: domestic},
		{ID: "overseas", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: overseas},
	}}}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "怎样接入", UserMessageID: "user-1", AssistantMessageID: "assistant-1"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}}
	bus := event.NewEventBus()
	var completed event.AgentCompleteData
	bus.On(event.EventAgentComplete, func(_ context.Context, e event.Event) error { completed = e.Data.(event.AgentCompleteData); return nil })
	answered, err := svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, bus, faqMatchModel{response: "```json\n{\"question\":\"国内还是海外？\",\"scope_match\":\"missing\"}\n```"}, faqMatchReranker{}, nil, nil, mustPendingFAQ(t, svc, req))
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "国内还是海外？", completed.FinalAnswer)
	svc.messageRepo = faqMatchMessageRepo{messages: []*types.Message{
		{ID: "assistant-1", Role: "assistant", IsCompleted: true, Content: completed.FinalAnswer, AgentSteps: completed.AgentSteps.([]types.AgentStep)},
		{ID: "user-2", Role: "user", Content: "海外"},
	}}
	req.Query, req.UserMessageID, req.AssistantMessageID = "海外", "user-2", "assistant-2"
	answered, err = svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, bus, faqMatchModel{response: `{"selected_id":"overseas","scope_match":"same"}`}, faqMatchReranker{}, nil, nil, mustPendingFAQ(t, svc, req))
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "海外答案", completed.FinalAnswer)
}

func TestAgentFAQSemanticMatchUsesStoredAnswer(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "如何接入", Answers: []string{"标准答案原文"}})
	require.NoError(t, err)
	result := &types.SearchResult{ID: "faq", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: string(types.ChunkTypeFAQ), Content: "如何接入", ChunkMetadata: meta}
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{results: []*types.SearchResult{result}}, chunkService: faqMatchChunkService{repo: faqMatchRepository{chunks: []*types.Chunk{
		{ID: "faq", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: meta},
	}}}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "怎么接入", AssistantMessageID: "assistant"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}}
	bus := event.NewEventBus()
	var completed event.AgentCompleteData
	bus.On(event.EventAgentComplete, func(_ context.Context, e event.Event) error { completed = e.Data.(event.AgentCompleteData); return nil })
	answered, err := svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{"selected_id":"faq","scope_match":"same"}`}, faqMatchReranker{}, nil)
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "标准答案原文", completed.FinalAnswer)
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{}`}, faqMatchReranker{}, nil)
	require.NoError(t, err)
	require.False(t, answered)
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{
		response: `{"selected_id":"faq","scope_match":"same"}`,
		onChat:   func() { svc.chunkService = faqMatchChunkService{repo: faqMatchRepository{}} },
	}, faqMatchReranker{}, nil)
	require.NoError(t, err)
	require.False(t, answered, "a removed FAQ must be revalidated after the model call")
}

func TestAgentFAQAnswerStrategy(t *testing.T) {
	req := &types.QARequest{Session: &types.Session{ID: "s"}, AssistantMessageID: "m"}
	bus := event.NewEventBus()
	var answer string
	bus.On(event.EventAgentComplete, func(_ context.Context, e event.Event) error {
		answer = e.Data.(event.AgentCompleteData).FinalAnswer
		return nil
	})
	match := tools.FAQMatch{Result: &types.SearchResult{ID: "faq"}, Metadata: &types.FAQChunkMetadata{Answers: []string{"第一条", "第二条"}, AnswerStrategy: types.AnswerStrategyAll}}
	answered, err := emitAgentFAQAnswer(context.Background(), req, bus, match, nil, "")
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "第一条\n\n第二条", answer)
	match.Metadata.AnswerStrategy = types.AnswerStrategyRandom
	answered, err = emitAgentFAQAnswer(context.Background(), req, bus, match, nil, "")
	require.NoError(t, err)
	require.True(t, answered)
	require.Contains(t, []string{"第一条", "第二条"}, answer)
}

func TestFAQScopeMissingWithSingleCandidateAsksAndResumes(t *testing.T) {
	meta, err := json.Marshal(types.FAQChunkMetadata{
		StandardQuestion: "iOS SDK 登录失败", SimilarQuestions: []string{"SDK 登录失败"}, Answers: []string{"iOS 排查步骤"},
	})
	require.NoError(t, err)
	chunk := &types.Chunk{ID: "ios", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, Metadata: meta}
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{}, chunkService: faqMatchChunkService{repo: faqMatchRepository{chunks: []*types.Chunk{chunk}}}}
	req := &types.QARequest{Session: &types.Session{ID: "s"}, Query: "SDK 登录失败", UserMessageID: "u1", AssistantMessageID: "a1"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}}
	bus := event.NewEventBus()
	var completed event.AgentCompleteData
	bus.On(event.EventAgentComplete, func(_ context.Context, e event.Event) error { completed = e.Data.(event.AgentCompleteData); return nil })
	questionModel := faqMatchModel{response: `{"question":"您使用的是哪个平台的 SDK？","scope_match":"missing"}`}
	answered, err := svc.tryAgentFAQ(context.Background(), req, cfg, bus, questionModel, nil, nil)
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "您使用的是哪个平台的 SDK？", completed.FinalAnswer)
	svc.messageRepo = faqMatchMessageRepo{messages: []*types.Message{
		{ID: "a1", Role: "assistant", IsCompleted: true, Content: completed.FinalAnswer, AgentSteps: completed.AgentSteps.([]types.AgentStep)},
		{ID: "u2", Role: "user", Content: "iOS"},
	}}
	req.Query, req.UserMessageID, req.AssistantMessageID = "iOS", "u2", "a2"
	require.Len(t, mustPendingFAQ(t, svc, req).state.IDs, 1)
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, bus, faqMatchModel{response: `{"selected_id":"ios","scope_match":"same"}`}, nil, nil)
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "iOS 排查步骤", completed.FinalAnswer)
}

func TestFAQCannotSelectCandidateWithUnconfirmedScope(t *testing.T) {
	meta := &types.FAQChunkMetadata{StandardQuestion: "iOS SDK 登录失败", Answers: []string{"iOS 步骤"}}
	matches := []tools.FAQMatch{{Result: &types.SearchResult{ID: "ios"}, Metadata: meta}}
	for _, tc := range []struct{ query, response string }{
		{"SDK 登录失败", `{"selected_id":"ios","scope_match":"missing"}`},
		{"Android SDK 登录失败", `{"selected_id":"ios","scope_match":"conflict"}`},
		{"iOS 和 Android SDK 登录失败", `{"selected_id":"ios","scope_match":"partial"}`},
		{"SDK 登录失败", `{"selected_id":"ios"}`},
		{"SDK 登录失败", `{"question":"请问是通过哪个 SDK`},
		{"Android SDK 登录失败", `{"scope_match":"conflicting","selected_id":null,"reason":"候选 FAQ 仅适用于 iOS"}`},
	} {
		t.Run(tc.query+tc.response, func(t *testing.T) {
			svc := &sessionService{}
			req := &types.QARequest{Session: &types.Session{ID: "s"}, Query: tc.query}
			cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}}
			answered, err := svc.tryAgentFAQClarification(context.Background(), req, req.Query, cfg, event.NewEventBus(), faqMatchModel{response: tc.response}, nil, nil, matches, nil)
			require.NoError(t, err)
			require.False(t, answered)
		})
	}
}

type faqRecordingRepository struct {
	interfaces.ChunkRepository
	queries *[]string
}

func (r faqRecordingRepository) FindExactFAQQuestions(_ context.Context, _ *types.SearchTarget, query string) ([]*types.Chunk, error) {
	*r.queries = append(*r.queries, query)
	return nil, nil
}

type faqScriptedModel struct {
	faqMatchModel
	responses *[]string
}

func (m faqScriptedModel) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	next := (*m.responses)[0]
	*m.responses = (*m.responses)[1:]
	return &types.ChatResponse{Content: next, Usage: types.TokenUsage{TotalTokens: 5}}, nil
}

func TestFAQFollowUpSearchesOriginalThenRewrittenQuery(t *testing.T) {
	var queries []string
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{}, chunkService: faqMatchChunkService{repo: faqRecordingRepository{queries: &queries}}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "那安卓呢？", AssistantMessageID: "m"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb", TenantID: 1}}}
	history := []chat.Message{{Role: "user", Content: "iOS SDK 登录失败怎么办"}, {Role: "assistant", Content: "请检查证书配置"}}
	responses := []string{"安卓 SDK 登录失败怎么办"}
	answered, err := svc.tryAgentFAQ(context.Background(), req, cfg, event.NewEventBus(), faqScriptedModel{responses: &responses}, nil, history)
	require.NoError(t, err)
	require.False(t, answered)
	require.Equal(t, []string{"那安卓呢？", "安卓 SDK 登录失败怎么办"}, queries)
	require.Equal(t, 5, cfg.FAQPreflightUsage.TotalTokens, "rewrite usage must be billed to the turn")

	// Without history the raw query is searched and no rewrite call is made.
	queries, cfg.FAQPreflightUsage = nil, nil
	answered, err = svc.tryAgentFAQ(context.Background(), req, cfg, event.NewEventBus(), faqScriptedModel{responses: &[]string{}}, nil, nil)
	require.NoError(t, err)
	require.False(t, answered)
	require.Equal(t, []string{"那安卓呢？"}, queries)
}
