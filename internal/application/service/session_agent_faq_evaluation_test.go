package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestAgentFAQPublicationKeepsRetrievedEvidence(t *testing.T) {
	req := &types.QARequest{Session: &types.Session{ID: "s"}, AssistantMessageID: "m"}
	bus := event.NewEventBus()
	var references event.AgentReferencesData
	var completed event.AgentCompleteData
	bus.On(event.EventAgentReferences, func(_ context.Context, e event.Event) error {
		references = e.Data.(event.AgentReferencesData)
		return nil
	})
	bus.On(event.EventAgentComplete, func(_ context.Context, e event.Event) error {
		completed = e.Data.(event.AgentCompleteData)
		return nil
	})
	match := tools.FAQMatch{
		Result:   &types.SearchResult{ID: "faq", Content: "retrieved question and source context"},
		Metadata: &types.FAQChunkMetadata{Answers: []string{"原文答案"}},
	}
	answered, err := emitAgentFAQAnswer(t.Context(), req, bus, match, nil, "")
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, "retrieved question and source context", match.Result.Content,
		"publishing the answer must not rewrite the evidence retained for the ledger")
	require.Equal(t, "原文答案", references.References.([]*types.SearchResult)[0].Content)
	require.Equal(t, "原文答案", completed.KnowledgeRefs[0].(*types.SearchResult).Content)
}

func TestAgentFAQClarificationReportsAllUsage(t *testing.T) {
	svc := &sessionService{}
	req := &types.QARequest{Session: &types.Session{ID: "s"}, Query: "SDK 登录失败"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge},
		FAQPreflightUsage: &types.TokenUsage{TotalTokens: 5}}
	matches := []tools.FAQMatch{{Result: &types.SearchResult{ID: "ios"},
		Metadata: &types.FAQChunkMetadata{StandardQuestion: "iOS SDK 登录失败", Answers: []string{"iOS 步骤"}}}}
	bus := event.NewEventBus()
	var completed event.AgentCompleteData
	bus.On(event.EventAgentComplete, func(_ context.Context, e event.Event) error {
		completed = e.Data.(event.AgentCompleteData)
		return nil
	})
	answered, err := svc.tryAgentFAQClarification(t.Context(), req, req.Query, cfg, bus,
		faqMatchModel{response: `{"question":"您使用哪个平台？","scope_match":"missing"}`,
			usage: types.TokenUsage{TotalTokens: 7}}, nil, nil, matches, nil)
	require.NoError(t, err)
	require.True(t, answered)
	require.Equal(t, 12, completed.Usage.(*types.TokenUsage).TotalTokens, "query rewrite and scope check belong to the same turn")
	addFAQPreflightUsage(cfg, types.TokenUsage{TotalTokens: 3})
	require.Equal(t, 12, completed.Usage.(*types.TokenUsage).TotalTokens, "the published usage is a snapshot")
}

func faqEvaluationFixture(t *testing.T) (*sessionService, *types.QARequest, *types.AgentConfig) {
	t.Helper()
	meta, err := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "问题", Answers: []string{"原文答案"}})
	require.NoError(t, err)
	chunk := &types.Chunk{ID: "faq", KnowledgeBaseID: "kb", KnowledgeID: "doc",
		ChunkType: types.ChunkTypeFAQ, Metadata: meta, Content: "问题及原始上下文"}
	svc := &sessionService{knowledgeBaseService: faqMatchKBService{},
		chunkService: faqMatchChunkService{repo: faqMatchRepository{chunks: []*types.Chunk{chunk}}}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, Query: "问题", AssistantMessageID: "answer"}
	cfg := &types.AgentConfig{AllowedTools: []string{tools.ToolSearchKnowledge},
		SearchTargets: types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}}
	return svc, req, cfg
}

func TestAgentFAQEvaluationDefersPublication(t *testing.T) {
	for _, question := range []bool{false, true} {
		t.Run(map[bool]string{false: "answer", true: "clarification"}[question], func(t *testing.T) {
			svc, req, cfg := faqEvaluationFixture(t)
			response := `{"selected_id":"faq","scope_match":"same"}`
			if question {
				response = `{"question":"您使用哪个平台？","scope_match":"missing"}`
			}
			bus := event.NewEventBus()
			var events []event.Event
			for _, kind := range []event.EventType{event.EventAgentReferences, event.EventAgentFinalAnswer, event.EventAgentComplete} {
				bus.On(kind, func(_ context.Context, e event.Event) error { events = append(events, e); return nil })
			}
			evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg,
				faqMatchModel{response: response, usage: types.TokenUsage{TotalTokens: 7}}, nil, nil)
			require.Empty(t, events, "evaluation cannot publish before the caller records its decision")
			require.Equal(t, 7, evaluation.Usage.TotalTokens)
			addFAQPreflightUsage(cfg, types.TokenUsage{TotalTokens: 3})
			require.Equal(t, 7, evaluation.Usage.TotalTokens)
			require.Equal(t, "faq_exact", evaluation.Observations[0].Stage)
			require.Equal(t, cfg.SearchTargets, evaluation.Observations[0].CompletedTargets)
			require.Empty(t, evaluation.Observations[1].CompletedTargets, "a scope decision is not another retrieval")
			require.Equal(t, "问题及原始上下文", evaluation.Observations[0].Candidates[0].Content)
			if question {
				require.Nil(t, evaluation.Match)
				require.Equal(t, "您使用哪个平台？", evaluation.Question)
				require.Equal(t, []string{"faq"}, evaluation.Clarification.IDs)
			} else {
				require.NotNil(t, evaluation.Match)
				require.Equal(t, "faq_verify", evaluation.Observations[2].Stage)
				require.Equal(t, cfg.SearchTargets, evaluation.Observations[2].CompletedTargets)
			}
			answered, err := emitAgentFAQEvaluation(t.Context(), req, bus, evaluation)
			require.NoError(t, err)
			require.True(t, answered)
			require.NotEmpty(t, events)
			complete := events[len(events)-1].Data.(event.AgentCompleteData)
			require.Equal(t, 7, complete.Usage.(*types.TokenUsage).TotalTokens)
			require.Equal(t, "问题及原始上下文", evaluation.Observations[0].Candidates[0].Content)
			if !question {
				require.Equal(t, "问题及原始上下文", evaluation.Match.Result.Content)
				require.Equal(t, "原文答案", complete.FinalAnswer)
			}
		})
	}
}

func TestAgentFAQEvaluationRetainsPartialMatch(t *testing.T) {
	svc, req, cfg := faqEvaluationFixture(t)
	repo := svc.chunkService.(faqMatchChunkService).repo.(faqMatchRepository)
	svc.chunkService = faqMatchChunkService{repo: faqPartialMatchRepository{repo}}
	cfg.SearchTargets = types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"failed"}},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: []string{"doc"}},
	}
	evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg,
		faqMatchModel{onChat: func() { t.Fatal("partial matches cannot prove uniqueness") }}, nil, nil)
	require.Nil(t, evaluation.Match)
	require.Empty(t, evaluation.Question)
	require.Len(t, evaluation.Observations, 1)
	observation := evaluation.Observations[0]
	require.ErrorIs(t, observation.Err, errFAQMatchUnavailable)
	require.ErrorIs(t, observation.Err, context.DeadlineExceeded)
	require.Len(t, observation.Matches, 1)
	require.Len(t, observation.Candidates, 1)
	require.Len(t, observation.Failures, 1)
	require.Equal(t, "faq_exact", observation.Failures[0].Stage)
	require.Equal(t, types.SearchTargets{cfg.SearchTargets[1]}, observation.CompletedTargets)
	cfg.SearchTargets[0].KnowledgeIDs[0] = "changed"
	require.Equal(t, []string{"failed"}, observation.RequestedTargets[0].KnowledgeIDs)
	require.Equal(t, []string{"failed"}, observation.Failures[0].Targets[0].KnowledgeIDs)
}

func TestAgentFAQEvaluationRetainsRawSemanticCandidates(t *testing.T) {
	svc, req, cfg := faqEvaluationFixture(t)
	chunk := svc.chunkService.(faqMatchChunkService).repo.(faqMatchRepository).chunks[0]
	svc.chunkService = faqMatchChunkService{repo: faqMatchRepository{}}
	svc.knowledgeBaseService = faqMatchKBService{results: []*types.SearchResult{{
		ID: chunk.ID, KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: string(types.ChunkTypeFAQ),
		Content: chunk.Content, ChunkMetadata: chunk.Metadata, Score: 0.9,
	}}}
	evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg,
		faqMatchModel{onChat: func() { t.Fatal("missing rerank cannot produce an applicable candidate") }}, nil, nil)
	require.Nil(t, evaluation.Match)
	require.Len(t, evaluation.Observations, 2)
	observation := evaluation.Observations[1]
	require.Equal(t, "faq_semantic", observation.Stage)
	require.ErrorContains(t, observation.Err, "requires a rerank model")
	require.Empty(t, observation.Matches)
	require.Len(t, observation.Candidates, 1)
	require.Equal(t, "faq", observation.Candidates[0].ID)
	require.Equal(t, cfg.SearchTargets, observation.CompletedTargets)
	require.Equal(t, "faq_rerank", observation.CauseStage)
}

func TestAgentFAQEvaluationRetainsUnmatchedRawCandidates(t *testing.T) {
	svc, req, cfg := faqEvaluationFixture(t)
	chunk := svc.chunkService.(faqMatchChunkService).repo.(faqMatchRepository).chunks[0]
	chunk.Metadata = []byte(`{"standard_question":"其他问题","answers":["其他答案"]}`)
	evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg,
		faqMatchModel{onChat: func() { t.Fatal("no applicable candidates") }}, nil, nil)
	require.Len(t, evaluation.Observations, 2)
	observation := evaluation.Observations[0]
	require.NoError(t, observation.Err)
	require.Empty(t, observation.Matches)
	require.Len(t, observation.Candidates, 1, "raw successful results must survive even without matching metadata")
	require.Equal(t, cfg.SearchTargets, observation.CompletedTargets)
}

func TestAgentFAQEvaluationSkippedScopesStayUnqueried(t *testing.T) {
	svc, req, cfg := faqEvaluationFixture(t)
	cfg.SearchTargets = types.SearchTargets{{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1}}
	calls := 0
	svc.chunkService = faqMatchChunkService{repo: faqEvaluationCountingRepository{calls: &calls}}
	evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg,
		faqMatchModel{onChat: func() { t.Fatal("skipped search cannot produce scope candidates") }}, nil, nil)
	require.Zero(t, calls)
	require.Len(t, evaluation.Observations, 2)
	for _, observation := range evaluation.Observations {
		require.NoError(t, observation.Err)
		require.Empty(t, observation.CompletedTargets)
		require.Equal(t, []tools.FAQSkippedTarget{{Target: cfg.SearchTargets[0], Reason: "empty_document_selection"}}, observation.SkippedTargets)
	}
	cfg.SearchTargets[0].KnowledgeBaseID = "changed"
	require.Equal(t, "kb", evaluation.Observations[0].SkippedTargets[0].Target.KnowledgeBaseID)
}

func TestAgentFAQEvaluationPendingRevalidationKeepsCoverage(t *testing.T) {
	for _, semantic := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "semantic"}[semantic], func(t *testing.T) {
			svc, req, cfg := faqEvaluationFixture(t)
			chunk := svc.chunkService.(faqMatchChunkService).repo.(faqMatchRepository).chunks[0]
			var meta types.FAQChunkMetadata
			require.NoError(t, json.Unmarshal(chunk.Metadata, &meta))
			svc.knowledgeBaseService = faqMatchKBService{results: []*types.SearchResult{{
				ID: chunk.ID, KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeFAQ),
				Content: chunk.Content, ChunkMetadata: chunk.Metadata,
			}}}
			pending := &pendingFAQClarification{question: "平台？", state: faqClarificationState{
				Query: "问题", IDs: []string{chunk.ID}, Semantic: semantic,
				Fingerprints: map[string]string{chunk.ID: faqFingerprint(&meta)},
			}}
			evaluation := &agentFAQEvaluation{}
			svc.evaluateAgentFAQClarification(t.Context(), req, "回答", cfg, faqMatchModel{response: `{}`},
				faqMatchReranker{}, nil, nil, pending, evaluation)
			require.Len(t, evaluation.Observations, 2)
			observation := evaluation.Observations[0]
			require.Equal(t, map[bool]string{false: "faq_exact_revalidation", true: "faq_semantic_revalidation"}[semantic], observation.Stage)
			require.NoError(t, observation.Err)
			require.Equal(t, cfg.SearchTargets, observation.CompletedTargets)
			require.Empty(t, evaluation.Observations[1].CompletedTargets)
		})
	}
}

type faqEvaluationResponseModel struct {
	faqMatchModel
	reply *types.ChatResponse
	err   error
}

func (m faqEvaluationResponseModel) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return m.reply, m.err
}

func TestAgentFAQEvaluationScopeFailureKeepsEvidence(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply *types.ChatResponse
		err   error
	}{
		{name: "typed timeout", reply: &types.ChatResponse{Usage: types.TokenUsage{TotalTokens: 3}}, err: context.DeadlineExceeded},
		{name: "nil response"},
		{name: "invalid json", reply: &types.ChatResponse{Content: "现在我有足够信息组织回复"}},
		{name: "null decision", reply: &types.ChatResponse{Content: "null"}},
		{name: "unconfirmed scope", reply: &types.ChatResponse{Content: `{"selected_id":"faq","scope_match":"missing"}`}},
		{name: "unknown id", reply: &types.ChatResponse{Content: `{"selected_id":"unknown","scope_match":"same"}`}},
		{name: "unexpected tool", reply: &types.ChatResponse{ToolCalls: []types.LLMToolCall{{ID: "unexpected"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, req, cfg := faqEvaluationFixture(t)
			evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg, faqEvaluationResponseModel{reply: tt.reply, err: tt.err}, nil, nil)
			require.Nil(t, evaluation.Match)
			require.Empty(t, evaluation.Question)
			require.Len(t, evaluation.Observations, 2)
			observation := evaluation.Observations[1]
			require.Equal(t, "faq_scope", observation.Stage)
			require.Error(t, observation.Err)
			require.Len(t, observation.Candidates, 1)
			if tt.err != nil {
				require.ErrorIs(t, observation.Err, tt.err)
				require.Equal(t, 3, evaluation.Usage.TotalTokens)
			}
		})
	}
	// No applicable candidate is a valid negative decision, not a tool failure.
	svc, req, cfg := faqEvaluationFixture(t)
	evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg, faqMatchModel{response: `{}`}, nil, nil)
	require.Nil(t, evaluation.Match)
	require.NoError(t, evaluation.Observations[1].Err)
}

func TestAgentFAQEvaluationRevalidationFailureKeepsPriorEvidence(t *testing.T) {
	svc, req, cfg := faqEvaluationFixture(t)
	evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg,
		faqMatchModel{response: `{"selected_id":"faq","scope_match":"same"}`, onChat: func() {
			svc.chunkService = faqMatchChunkService{repo: faqMatchRepository{err: context.DeadlineExceeded}}
		}}, nil, nil)
	require.Nil(t, evaluation.Match)
	require.Len(t, evaluation.Observations, 3)
	require.Len(t, evaluation.Observations[0].Candidates, 1)
	failed := evaluation.Observations[2]
	require.Equal(t, "faq_verify", failed.Stage)
	require.ErrorIs(t, failed.Err, context.DeadlineExceeded)
	require.Equal(t, cfg.SearchTargets, failed.Failures[0].Targets)
	require.Empty(t, failed.CompletedTargets)
	require.Equal(t, cfg.SearchTargets, evaluation.Observations[0].CompletedTargets)
}

func TestAgentFAQEvaluationRewriteFailureUsesRawQuery(t *testing.T) {
	svc, req, cfg := faqEvaluationFixture(t)
	var queries []string
	svc.chunkService = faqMatchChunkService{repo: faqRecordingRepository{queries: &queries}}
	evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg,
		faqEvaluationResponseModel{err: context.DeadlineExceeded}, nil,
		[]chat.Message{{Role: "user", Content: "此前的问题"}})
	require.Equal(t, []string{req.Query}, queries)
	require.Equal(t, req.Query, evaluation.Query)
	require.Equal(t, "faq_exact", evaluation.Observations[0].Stage)
	require.NoError(t, evaluation.Observations[0].Err, "rewrite failure is not retrieval failure")
	require.Equal(t, "faq_rewrite", evaluation.Observations[1].Stage)
	require.ErrorIs(t, evaluation.Observations[1].Err, context.DeadlineExceeded)
}

type faqEvaluationHistoryFailure struct{ faqMatchMessageRepo }

func (faqEvaluationHistoryFailure) GetRecentMessagesBySession(context.Context, string, int) ([]*types.Message, error) {
	return nil, context.DeadlineExceeded
}

func TestAgentFAQEvaluationHistoryFailureIsObservable(t *testing.T) {
	svc, req, cfg := faqEvaluationFixture(t)
	svc.messageRepo = faqEvaluationHistoryFailure{}
	evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg, faqMatchModel{
		onChat: func() { t.Fatal("unavailable clarification history must not silently continue FAQ evaluation") },
	}, nil, nil)
	require.Len(t, evaluation.Observations, 1)
	require.Equal(t, "faq_history", evaluation.Observations[0].Stage)
	require.ErrorIs(t, evaluation.Observations[0].Err, context.DeadlineExceeded)
}

func TestAgentFAQEvaluationPublicationRechecksCancellationAndSteering(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "steered", true: "canceled"}[canceled], func(t *testing.T) {
			svc, req, cfg := faqEvaluationFixture(t)
			evaluation := svc.evaluateAgentFAQ(t.Context(), req, cfg,
				faqMatchModel{response: `{"selected_id":"faq","scope_match":"same"}`}, nil, nil)
			require.NotNil(t, evaluation.Match)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			} else {
				req.SteerSink = faqPendingSteer{}
			}
			bus := event.NewEventBus()
			for _, kind := range []event.EventType{event.EventAgentReferences, event.EventAgentFinalAnswer, event.EventAgentComplete} {
				bus.On(kind, func(context.Context, event.Event) error { t.Fatal("stale decision must not be published"); return nil })
			}
			answered, err := emitAgentFAQEvaluation(ctx, req, bus, evaluation)
			require.False(t, answered)
			require.Equal(t, canceled, errors.Is(err, context.Canceled))
		})
	}
}

func TestAgentFAQEvaluationInterruptionStopsLaterSearch(t *testing.T) {
	for _, phase := range []string{"before evaluation", "after rewrite", "after scope"} {
		t.Run(phase, func(t *testing.T) {
			svc, req, cfg := faqEvaluationFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			repo := svc.chunkService.(faqMatchChunkService).repo.(faqMatchRepository)
			calls := 0
			svc.chunkService = faqMatchChunkService{repo: faqEvaluationCountingRepository{faqMatchRepository: repo, calls: &calls}}
			var history []chat.Message
			if phase == "before evaluation" {
				cancel()
			} else if phase == "after rewrite" {
				history = []chat.Message{{Role: "user", Content: "原问题"}}
				svc.chunkService = faqMatchChunkService{repo: faqEvaluationCountingRepository{faqMatchRepository: faqMatchRepository{}, calls: &calls}}
			}
			evaluation := svc.evaluateAgentFAQ(ctx, req, cfg,
				faqMatchModel{response: `{"selected_id":"faq","scope_match":"same"}`, onChat: cancel}, nil, history)
			require.Nil(t, evaluation.Match)
			require.Empty(t, evaluation.Question)
			require.NotEmpty(t, evaluation.Observations)
			require.ErrorIs(t, evaluation.Observations[len(evaluation.Observations)-1].Err, context.Canceled)
			wantCalls := 0
			if phase == "after scope" || phase == "after rewrite" {
				wantCalls = 1
			}
			require.Equal(t, wantCalls, calls, "cancellation cannot trigger candidate revalidation")
		})
	}
}

type faqEvaluationCountingRepository struct {
	faqMatchRepository
	calls *int
}

func (r faqEvaluationCountingRepository) FindExactFAQQuestions(ctx context.Context, target *types.SearchTarget, query string) ([]*types.Chunk, error) {
	*r.calls++
	return r.faqMatchRepository.FindExactFAQQuestions(ctx, target, query)
}
