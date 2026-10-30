package agent

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/utils/ollama"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type liveKnowledgeFixture struct {
	interfaces.KnowledgeBaseService
	mu      sync.Mutex
	queries []string
}

func (f *liveKnowledgeFixture) GetKnowledgeBasesByIDsOnly(_ context.Context, _ []string) ([]*types.KnowledgeBase, error) {
	kb := &types.KnowledgeBase{ID: "kb-test"}
	kb.IndexingStrategy.KeywordEnabled = true
	return []*types.KnowledgeBase{kb}, nil
}

func (f *liveKnowledgeFixture) ResolveEmbeddingModelKeys(_ context.Context, _ []*types.KnowledgeBase) map[string]string {
	return nil
}

func (f *liveKnowledgeFixture) HybridSearch(_ context.Context, _ string, params types.SearchParams) ([]*types.SearchResult, error) {
	f.mu.Lock()
	f.queries = append(f.queries, params.QueryText)
	f.mu.Unlock()
	return []*types.SearchResult{{
		ID: "chunk-test", KnowledgeID: "doc-test", KnowledgeBaseID: "kb-test", KnowledgeTitle: "小游戏文档",
		Score: 0.95, Content: "模拟资料：微信小游戏后台的隐私协议入口在设置页面，发布后生效。",
	}}, nil
}

func TestPlannerSimpleQuestionsWithLocalModel(t *testing.T) {
	name := os.Getenv("WEKNORA_PLAN_LIVE_MODEL")
	if name == "" {
		t.Skip("set WEKNORA_PLAN_LIVE_MODEL to run against a local model")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	service, err := ollama.GetOllamaService()
	require.NoError(t, err)
	require.NoError(t, service.StartService(ctx))
	model, err := chat.NewOllamaChat(&chat.ChatConfig{ModelName: name}, service)
	require.NoError(t, err)
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	for _, question := range []string{
		"支付用的发货地址在哪里配置？", "微信小游戏的隐私协议怎么配置？",
		"测试账号怎么加入支付白名单？", "SDK 登录失败时应先检查什么？",
	} {
		t.Run(question, func(t *testing.T) {
			plan, planned := engine.maybePlan(ctx, question, []chat.Message{
				{Role: "system", Content: "Only address the user's requested scope."},
				{Role: "user", Content: question},
			}, nil)
			require.False(t, planned, "single question should use the normal agent loop: %+v", plan)
		})
	}
}

// Run explicitly with WEKNORA_PLAN_LIVE_MODEL set to an installed Ollama model.
func TestPlannerWithLocalModel(t *testing.T) {
	name := os.Getenv("WEKNORA_PLAN_LIVE_MODEL")
	if name == "" {
		t.Skip("set WEKNORA_PLAN_LIVE_MODEL to run against a local model")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	service, err := ollama.GetOllamaService()
	require.NoError(t, err)
	require.NoError(t, service.StartService(ctx))
	model, err := chat.NewOllamaChat(&chat.ChatConfig{ModelName: name}, service)
	require.NoError(t, err)
	engine := newTestEngine(t, model, withPlanningEnabled(true))
	query := "先查退款规则，再核实账号，最后写申诉邮件"
	plan, ok := engine.maybePlan(ctx, query, []chat.Message{
		{Role: "system", Content: "Only address the user's requested scope."},
		{Role: "user", Content: query},
	}, nil)
	require.True(t, ok, "multi-step task should produce a plan")
	require.GreaterOrEqual(t, len(plan.Steps), 2)
	for _, step := range plan.Steps {
		require.NotContains(t, step.Description, "提交")
		require.NotContains(t, step.Description, "发送")
		require.NotContains(t, strings.ToLower(step.Description), "send")
		require.NotContains(t, strings.ToLower(step.Description), "draft")
		require.NotContains(t, step.Description, "草稿")
	}
	t.Logf("local model plan: %+v", plan.Steps)

	privacy := "微信小游戏的隐私协议怎么配置？"
	privacyPlan, planned := engine.maybePlan(ctx, privacy, []chat.Message{
		{Role: "system", Content: "Only address the user's requested scope."},
		{Role: "user", Content: privacy},
	}, nil)
	if planned {
		for _, step := range privacyPlan.Steps {
			require.NotContains(t, step.Description, "平台服务中心")
		}
		t.Logf("privacy plan: %+v", privacyPlan.Steps)
	} else {
		t.Log("privacy question followed the normal execution path")
	}

	thinking := false
	engine.config.Thinking = &thinking
	engine.toolRegistry = agenttools.NewToolRegistry()
	state, err := engine.Execute(ctx, "local-test", "local-test", "方案甲每月10元，方案乙每月20元。分别计算两种方案的年费用，再比较差额并给出更便宜的方案。", nil)
	require.NoError(t, err)
	require.NotNil(t, state.Plan, "complex task should run through the plan")
	require.Equal(t, len(state.Plan.Steps), state.Plan.Current)
	for _, step := range state.Plan.Steps {
		require.NotEqual(t, "blocked", step.Status)
	}
	require.NotEmpty(t, state.FinalAnswer)
	require.NotContains(t, state.FinalAnswer, `"status"`)
	t.Logf("final answer: %s", state.FinalAnswer)

	retriever := &liveKnowledgeFixture{}
	engine = newTestEngine(t, model, withPlanningEnabled(true))
	engine.config.Thinking = &thinking
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.toolRegistry.RegisterTool(agenttools.NewSearchKnowledgeTool(retriever, nil, nil,
		types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-test"}}, nil, nil))
	engine.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "kb-test", Name: "小游戏文档", Capabilities: []string{"chunks"}}}
	state, err = engine.Execute(ctx, "local-test", "local-test", "根据知识库分别说明微信小游戏隐私协议的配置入口和生效时间，最后整理操作要点。", nil)
	require.NoError(t, err)
	require.Nil(t, state.Plan, "a single knowledge question should use the normal ReAct loop")
	require.NotEmpty(t, retriever.queries, "the model must search the fixture knowledge base")
	for _, query := range retriever.queries {
		require.NotContains(t, query, "平台服务中心")
	}
	require.Contains(t, state.FinalAnswer, "小游戏")
	require.NotContains(t, state.FinalAnswer, "平台服务中心")
	require.NotContains(t, state.FinalAnswer, "重新发布")
	require.NotContains(t, state.FinalAnswer, "审核通过")
	t.Logf("retrieval queries: %v; answer: %s", retriever.queries, state.FinalAnswer)
}
