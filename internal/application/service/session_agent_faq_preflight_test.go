package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type preflightKBs struct {
	interfaces.KnowledgeBaseService
	rows []*types.SearchResult
}

func (*preflightKBs) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	kb := &types.KnowledgeBase{ID: "kb", TenantID: 1, Type: types.KnowledgeBaseTypeFAQ}
	kb.IndexingStrategy.VectorEnabled = true
	return []*types.KnowledgeBase{kb}, nil
}
func (*preflightKBs) ResolveEmbeddingModelKeys(context.Context, []*types.KnowledgeBase) map[string]string {
	return nil
}
func (s *preflightKBs) HybridSearch(context.Context, string, types.SearchParams) ([]*types.SearchResult, error) {
	return s.rows, nil
}

type preflightChunks struct {
	interfaces.ChunkService
	repo *preflightRepo
}

func (s preflightChunks) GetRepository() interfaces.ChunkRepository { return s.repo }

type preflightRepo struct {
	interfaces.ChunkRepository
	chunk *types.Chunk
	exact func(context.Context, string) ([]*types.Chunk, error)
}

func (r *preflightRepo) FindExactFAQQuestions(ctx context.Context, _ *types.SearchTarget, query string) ([]*types.Chunk, error) {
	if r.exact != nil {
		return r.exact(ctx, query)
	}
	return nil, nil
}
func (r *preflightRepo) GetVisibleFAQByID(context.Context, *types.SearchTarget, string) (*types.Chunk, error) {
	return r.chunk, nil
}

type preflightChat struct {
	graphReplyChat
	call func(context.Context, []chat.Message) (*types.ChatResponse, error)
}

func (c preflightChat) Chat(ctx context.Context, messages []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	return c.call(ctx, messages)
}

const preflightSelection = `{"decision":"direct_answer","selected_id":"1","scope_match":"same","question_match":"equivalent","requires_steps":false,"answer_kind":"other","unanswered_parts_of_original_request":[]}`
const preflightQuestion = `{"question":"哪个平台？","scope_match":"missing","missing_user_fields":["平台"],"unanswered_parts_of_original_request":[]}`

func preflightFixture(t *testing.T) (*sessionService, *types.QARequest, *types.AgentConfig, *preflightRepo) {
	t.Helper()
	metadata, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "支付问题", Answers: []string{"标准答案"}})
	require.NoError(t, err)
	chunk := &types.Chunk{ID: "faq", KnowledgeBaseID: "kb", ChunkType: types.ChunkTypeFAQ, Content: "支付问题", Metadata: metadata}
	row := &types.SearchResult{ID: "faq", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeFAQ), Content: chunk.Content, ChunkMetadata: metadata, Score: 1}
	repo := &preflightRepo{chunk: chunk}
	s := &sessionService{knowledgeBaseService: &preflightKBs{rows: []*types.SearchResult{row}}, chunkService: preflightChunks{repo: repo}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "支付出错", AssistantMessageID: "assistant"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge}, MultiTurnEnabled: true,
		SearchTargets: types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}}
	return s, req, cfg, repo
}

type preflightMessages struct {
	historyRepo
}

func (preflightMessages) GetPreviousRetrievalState(context.Context, string, string) (*types.Message, error) {
	return nil, nil
}

func (preflightMessages) UpdateMessageRetrievalState(context.Context, string, string, json.RawMessage) error {
	return nil
}

func TestFAQFallsThroughForCurrentProjectState(t *testing.T) {
	const query = "账密注册的功能开关状态"
	for _, decision := range []string{`{"decision":"needs_live_data"}`, `{"decision":"insufficient"}`, `{}`, preflightQuestion, `{"question":"请问是官包还是渠道包？"}`,
		strings.Replace(preflightSelection, `"selected_id":"1"`, `"selected_id":"0"`, 1),
		preflightSelection[:len(preflightSelection)-1] + `,"question":"是官包吗？"}`} {
		s, req, cfg, repo := preflightFixture(t)
		req.Query = query
		repo.chunk.Metadata = types.JSON(`{"standard_question":"账密注册开关的各个值是什么意思？","similar_questions":["账密注册的功能开关状态"],"answers":["0 关闭注册和登录，1 仅登录，2 仅注册，3 注册和登录均开启。"]}`)
		repo.exact = func(context.Context, string) ([]*types.Chunk, error) { return []*types.Chunk{repo.chunk}, nil }
		calls := 0
		model := preflightChat{call: func(_ context.Context, messages []chat.Message) (*types.ChatResponse, error) {
			if messages[0].Content == faqRewritePrompt {
				return &types.ChatResponse{Content: query}, nil
			}
			calls++
			require.Contains(t, messages[1].Content, query)
			require.Contains(t, messages[1].Content, "仅注册")
			return &types.ChatResponse{Content: decision}, nil
		}}
		answered, err := s.tryAgentFAQ(t.Context(), req, cfg, event.NewEventBus(), model, nil, nil)
		require.NoError(t, err)
		require.False(t, answered, decision)

		req.Query = "我想问注册配置"
		reply, err := s.steerFAQ(req, cfg, event.NewEventBus(), model, nil, nil)(t.Context(), "next", query, nil)
		require.NoError(t, err)
		require.Empty(t, reply.Answer, decision)
		require.Equal(t, 2, calls, "both entry points must reach the coverage judge")
	}
}

func TestFAQFollowupSearchReadsCurrentExactEntry(t *testing.T) {
	const title = "支付回调地址可以是内网地址吗？"
	s, req, cfg, repo := preflightFixture(t)
	kbs := s.knowledgeBaseService.(*preflightKBs)
	entry := repo.chunk
	entry.Metadata = types.JSON(`{"standard_question":"支付回调地址可以是内网地址吗？","answers":["旧答案"]}`)
	kbs.rows[0].ChunkMetadata = entry.Metadata
	req.Query = "手游 pay 可以用内网域名吗？不传回调参数会用哪个地址？"
	model := &graphReplyChat{resp: &types.ChatResponse{Content: `{"decision":"insufficient"}`}}
	answered, err := s.tryAgentFAQ(t.Context(), req, cfg, event.NewEventBus(), model, &scoredReranker{scores: []float64{0.9}}, nil)
	require.NoError(t, err)
	require.False(t, answered)
	require.Equal(t, []string{title}, cfg.FAQPreflightQuestions)

	entry.Metadata = types.JSON(`{"standard_question":"支付回调地址可以是内网地址吗？","answers":["更新后的答案"]}`)
	repo.exact = func(_ context.Context, q string) ([]*types.Chunk, error) {
		require.Equal(t, title, q)
		return []*types.Chunk{entry}, nil
	}
	kbs.rows = nil // A second semantic search cannot supply this answer.
	tool := tools.NewSearchKnowledgeTool(kbs, nil, s.chunkService, cfg.SearchTargets, nil, s.cfg).WithFAQPreflightQuestions(cfg.FAQPreflightQuestions)
	args := json.RawMessage(`{"knowledge_base_ids":["kb"],"query":"支付回调地址可以是内网地址吗"}`)
	hit, err := tool.Execute(t.Context(), args)
	require.NoError(t, err)
	require.Equal(t, "exact", hit.Data["mode"])
	require.Contains(t, hit.Output, "更新后的答案")

	repo.exact = nil
	miss, err := tool.Execute(t.Context(), args)
	require.NoError(t, err)
	require.NotEqual(t, "exact", miss.Data["mode"])
	require.NotContains(t, miss.Output, "更新后的答案")
}

func TestFAQPreflightDoesNotBypassPinnedSkills(t *testing.T) {
	s, req, cfg, repo := preflightFixture(t)
	req.Query = "支付问题"
	req.SkillNames = []string{"payment-diagnosis"}
	cfg.PinnedSkillNames = []string{"payment-diagnosis"}
	repo.exact = func(context.Context, string) ([]*types.Chunk, error) { return []*types.Chunk{repo.chunk}, nil }
	bus := event.NewEventBus()
	model := graphReplyChat{resp: &types.ChatResponse{Content: preflightSelection}}
	answered, err := s.tryAgentFAQ(t.Context(), req, cfg, bus, model, nil, nil)
	require.NoError(t, err)
	require.False(t, answered, "an exact FAQ must not skip the user's authorized @skill")
	require.Nil(t, s.steerFAQ(req, cfg, bus, model, nil, nil))
}

func TestAgentQAHandoffPreservesLegacyFAQConversation(t *testing.T) {
	s, req, _, _ := preflightFixture(t)
	const question = "你使用哪个支付渠道？"
	steps := []types.AgentStep{{ToolCalls: []types.ToolCall{{ID: types.FAQClarificationToolCallID,
		Args: map[string]interface{}{"state": `{"query":"sdk的沙盒支付怎么设置","ids":["faq"]}`}}}}}
	rows := []*types.Message{
		{ID: "u1", Role: "user", RequestID: "prior", Content: "sdk的沙盒支付怎么设置", CreatedAt: historyBase},
		{ID: "a1", Role: "assistant", RequestID: "prior", Content: question, IsCompleted: true, AgentSteps: steps, CreatedAt: historyBase.Add(time.Second)},
	}
	s.messageRepo = &preflightMessages{historyRepo: historyRepo{rows: rows}}
	s.cfg = &config.Config{}
	engine := &retrievalInputEngine{}
	s.agentService = &retrievalInputService{engine: engine}
	s.modelService = &stubModelService{chatModel: &graphReplyChat{resp: &types.ChatResponse{Content: `{}`}}, modelsByID: map[string]*types.Model{
		"model": {ID: "model", Type: types.ModelTypeKnowledgeQA},
	}}
	req.Query, req.UserMessageID, req.AssistantMessageID = "苹果内购", "u2", "a2"
	req.Session.TenantID, req.TurnLeaseHeld = 1, true
	req.CustomAgent = &types.CustomAgent{ID: "agent", TenantID: 1, Config: types.CustomAgentConfig{
		ModelID: "model", KBSelectionMode: "selected", KnowledgeBases: []string{"kb"}, SkillsSelectionMode: "none",
		MultiTurnEnabled: true, WebSearchProviderID: "unused", AllowedTools: []string{tools.ToolSearchKnowledge},
	}}
	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	require.NoError(t, s.AgentQA(ctx, req, event.NewEventBus()))
	require.True(t, engine.called)
	require.Equal(t, "苹果内购", engine.query)
	require.Contains(t, rolesAndContents(engine.history), [2]string{"user", "sdk的沙盒支付怎么设置"})
	require.Contains(t, rolesAndContents(engine.history), [2]string{"assistant", question})
}

func TestFAQSemanticDirectAnswerUsesModelScore(t *testing.T) {
	for _, tc := range []struct {
		score, threshold float64
		want             bool
	}{
		{0.4, 0, false},
		{0.9, 0, true},
		{0.766, 0.9, false},
		{0.9, 0.9, true},
	} {
		s, req, cfg, _ := preflightFixture(t)
		req.CustomAgent = &types.CustomAgent{Config: types.CustomAgentConfig{FAQPriorityEnabled: true, FAQDirectAnswerThreshold: tc.threshold}}
		kbs := s.knowledgeBaseService.(*preflightKBs)
		row := *kbs.rows[0]
		first, second := row, row
		first.ID, second.ID = "related-first", "related-second"
		kbs.rows = []*types.SearchResult{&first, &second, &row}
		reranker := &scoredReranker{scores: []float64{0.99, 0.98, tc.score}}
		selected := graphReplyChat{resp: &types.ChatResponse{Content: strings.Replace(preflightSelection, `"selected_id":"1"`, `"selected_id":"3"`, 1)}}
		evaluation := s.evaluateAgentFAQ(t.Context(), req, cfg, selected, reranker, nil)
		require.Equal(t, tc.want, evaluation.Match != nil, "the selected third candidate needs the raw score floor, not first rank or a higher fusion score")
		if tc.want {
			require.Equal(t, row.ID, evaluation.Match.Result.ID)
		}
	}
}

func TestFAQPreflightOverlapsRewriteAndCancelsItOnExactHit(t *testing.T) {
	s, req, cfg, repo := preflightFixture(t)
	req.Query = "支付问题"
	started := make(chan struct{})
	finished := make(chan struct{})
	repo.exact = func(ctx context.Context, _ string) ([]*types.Chunk, error) {
		select {
		case <-started:
			return []*types.Chunk{repo.chunk}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	model := preflightChat{call: func(ctx context.Context, messages []chat.Message) (*types.ChatResponse, error) {
		if messages[0].Content == faqRewritePrompt {
			close(started)
			<-ctx.Done()
			close(finished)
			return &types.ChatResponse{Usage: types.TokenUsage{TotalTokens: 3}}, ctx.Err()
		}
		return &types.ChatResponse{Content: preflightSelection, Usage: types.TokenUsage{TotalTokens: 7}}, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	evaluation := s.evaluateAgentFAQ(ctx, req, cfg, model, nil, []chat.Message{{Role: "user", Content: "支付问题"}})
	require.NotNil(t, evaluation.Match)
	select {
	case <-finished:
	default:
		t.Fatal("exact hit must cancel and collect the rewrite before completing")
	}
	require.Equal(t, 10, evaluation.Usage.TotalTokens, "both calls must be counted without concurrent config writes")
}

func TestFAQPreflightUsesRewriteAfterExactMiss(t *testing.T) {
	s, req, cfg, repo := preflightFixture(t)
	var queries []string
	repo.exact = func(_ context.Context, query string) ([]*types.Chunk, error) {
		queries = append(queries, query)
		if query == "支付问题" {
			return []*types.Chunk{repo.chunk}, nil
		}
		return nil, nil
	}
	model := preflightChat{call: func(_ context.Context, messages []chat.Message) (*types.ChatResponse, error) {
		if messages[0].Content == faqRewritePrompt {
			return &types.ChatResponse{Content: "支付问题"}, nil
		}
		return &types.ChatResponse{Content: preflightSelection}, nil
	}}
	evaluation := s.evaluateAgentFAQ(t.Context(), req, cfg, model, nil, []chat.Message{{Role: "user", Content: "上文"}})
	require.NotNil(t, evaluation.Match)
	require.Equal(t, []string{"支付出错", "支付问题"}, queries)
}

func TestFAQPreflightBudgetAndParentCancellation(t *testing.T) {
	s, req, cfg, repo := preflightFixture(t)
	req.Query = "支付问题"
	repo.exact = func(context.Context, string) ([]*types.Chunk, error) { return []*types.Chunk{repo.chunk}, nil }
	model := preflightChat{call: func(ctx context.Context, _ []chat.Message) (*types.ChatResponse, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), faqPreflightTimeout)
		// A sub-call timeout must not turn into parent cancellation.
		return nil, context.DeadlineExceeded
	}}
	answered, err := s.tryAgentFAQ(t.Context(), req, cfg, event.NewEventBus(), model, nil, nil)
	require.NoError(t, err, "preflight timeout must leave the parent request usable")
	require.False(t, answered)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.tryAgentFAQ(ctx, req, cfg, event.NewEventBus(), model, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestFAQPreflightDiscardsAnswerWhenBudgetContextExpires(t *testing.T) {
	s, req, cfg, repo := preflightFixture(t)
	req.Query = "支付问题"
	repo.exact = func(context.Context, string) ([]*types.Chunk, error) { return []*types.Chunk{repo.chunk}, nil }
	model := preflightChat{call: func(ctx context.Context, _ []chat.Message) (*types.ChatResponse, error) {
		<-ctx.Done()
		return &types.ChatResponse{Content: preflightSelection}, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	evaluation := s.evaluateAgentFAQ(ctx, req, cfg, model, nil, nil)
	require.Nil(t, evaluation.Match)
	require.True(t, errors.Is(evaluation.Observations[len(evaluation.Observations)-1].Err, context.DeadlineExceeded))
}

func TestFAQPreflightSharesBudgetAcrossStagesAndSteer(t *testing.T) {
	for _, steer := range []bool{false, true} {
		s, req, cfg, repo := preflightFixture(t)
		req.Query = "支付问题"
		query := req.Query
		if steer {
			query += "\n补充信息"
			meta, err := repo.chunk.FAQMetadata()
			require.NoError(t, err)
			meta.StandardQuestion = query
			repo.chunk.Metadata, err = json.Marshal(meta)
			require.NoError(t, err)
		}
		var exactDeadline, scopeDeadline time.Time
		repo.exact = func(ctx context.Context, _ string) ([]*types.Chunk, error) {
			exactDeadline, _ = ctx.Deadline()
			time.Sleep(15 * time.Millisecond)
			return []*types.Chunk{repo.chunk}, nil
		}
		model := preflightChat{call: func(ctx context.Context, messages []chat.Message) (*types.ChatResponse, error) {
			if messages[0].Content == faqRewritePrompt {
				return &types.ChatResponse{Content: query}, nil
			}
			scopeDeadline, _ = ctx.Deadline()
			return &types.ChatResponse{Content: preflightSelection}, nil
		}}
		if steer {
			reply, err := s.steerFAQ(req, cfg, event.NewEventBus(), model, nil, nil)(t.Context(), "next-user", "补充信息", nil)
			require.NoError(t, err)
			require.Equal(t, "标准答案", reply.Answer)
		} else {
			evaluation := s.evaluateAgentFAQ(t.Context(), req, cfg, model, nil, nil)
			require.NotNil(t, evaluation.Match)
		}
		require.False(t, scopeDeadline.IsZero())
		// Exact lookup has a 10s cap; scope's 15s cap must share the original 12s deadline.
		require.LessOrEqual(t, scopeDeadline.Sub(exactDeadline), faqPreflightTimeout-10*time.Second)
	}
}
