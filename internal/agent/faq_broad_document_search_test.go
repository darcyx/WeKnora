package agent

import (
	"context"
	"encoding/json"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestFAQBroadDocumentSearchReplacesOnlyOneDocumentCall(t *testing.T) {
	query := "我们海外安卓版接了 Google 支付，上线头几天都正常，大概一周后玩家付完款都提示校验失败，没到账，是什么原因？"
	e := &AgentEngine{faqBroadDocumentQuery: query, config: &types.AgentConfig{SearchTargets: types.SearchTargets{
		{KnowledgeBaseID: "faq"}, {KnowledgeBaseID: "doc"}, {KnowledgeBaseID: "other"},
	}}, knowledgeBasesInfo: []*KnowledgeBaseInfo{
		{ID: "faq", Type: "faq", Capabilities: []string{"chunks"}},
		{ID: "doc", Type: "document", Capabilities: []string{"chunks"}},
		{ID: "other", Type: "document", Capabilities: []string{"chunks"}},
	}}
	response := &types.ChatResponse{ToolCalls: []types.LLMToolCall{
		{Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"Google 支付 校验失败 上线几天后 玩家付款未到账 原因", "knowledge_base_ids":["faq"]}`}},
		{Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"Google Play 支付校验失败 服务端校验 purchaseToken 报错 排查", "knowledge_base_ids":["doc"], "limit":10}`}},
		{Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"other", "knowledge_base_ids":["other"]}`}},
	}}
	e.useFAQBroadDocumentSearch(response)
	require.Contains(t, response.ToolCalls[0].Function.Arguments, `"faq"`)
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(response.ToolCalls[1].Function.Arguments), &args))
	require.Equal(t, query, args["query"])
	require.NotContains(t, args, "knowledge_base_ids")
	require.NotContains(t, args, "mode")
	require.EqualValues(t, 10, args["limit"])
	require.Contains(t, response.ToolCalls[1].ModelArguments, `"doc"`)
	require.Contains(t, response.ToolCalls[2].Function.Arguments, `"other"`)
	require.Empty(t, e.faqBroadDocumentQuery)
}

func TestFAQBroadDocumentSearchPreservesOtherActions(t *testing.T) {
	for _, calls := range [][]types.LLMToolCall{
		{{Function: types.FunctionCall{Name: agenttools.ToolAskUser, Arguments: `{}`}}, {Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"q"}`}}},
		{{Function: types.FunctionCall{Name: agenttools.ToolWikiSearch, Arguments: `{}`}}, {Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"q"}`}}},
		{{Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"q", "retrieval":{"action":"supplement"}}`}}},
		{{Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"q", "knowledge_base_ids":["unknown"]}`}}},
	} {
		e := &AgentEngine{faqBroadDocumentQuery: "original", knowledgeBasesInfo: []*KnowledgeBaseInfo{{ID: "doc", Type: "document", Capabilities: []string{"chunks"}}}}
		response := &types.ChatResponse{ToolCalls: calls}
		before, err := json.Marshal(calls)
		require.NoError(t, err)
		e.useFAQBroadDocumentSearch(response)
		after, err := json.Marshal(calls)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
}

func TestFAQBroadDocumentSearchUsesDispatchScopeAndLedger(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	targets := types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "doc", KnowledgeIDs: []string{"selected"}},
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "pack"},
	}
	e.config.SearchTargets = targets
	e.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "doc", Type: "document", Capabilities: []string{"chunks"}}, {ID: "pack", Type: "document", Capabilities: []string{"chunks"}}}
	query := "上线初期正常，一周后支付校验失败"
	e.beginTurn(query, nil, &types.AgentState{})
	e.SetFAQBroadDocumentSearch(query)
	var got types.SearchTargets
	probe.execute = func(ctx context.Context, _ json.RawMessage) (*types.ToolResult, error) {
		execution, _ := agenttools.ToolExecFromContext(ctx)
		got = execution.RetrievalTargets.Clone()
		if len(got) == 0 {
			got = e.config.SearchTargets.Clone()
		}
		return &types.ToolResult{Success: true, Output: "result"}, nil
	}
	response := &types.ChatResponse{ToolCalls: []types.LLMToolCall{{ID: "call", Function: types.FunctionCall{
		Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"支付","knowledge_base_ids":["doc"]}`,
	}}}}
	step := &types.AgentStep{}
	require.Nil(t, e.executeToolCalls(t.Context(), response, step, 0, "session", "message"))
	require.Len(t, probe.calls, 1)
	require.JSONEq(t, `{"query":"上线初期正常，一周后支付校验失败"}`, string(probe.calls[0]))
	require.Equal(t, targets, got)
	require.Len(t, e.retrieval.snapshot().Attempts, 1)
}

func TestFAQBroadDocumentSearchPreservesExplicitSources(t *testing.T) {
	for _, query := range []string{
		"请只查 Google 支付配置文档：一周后校验失败怎么办？",
		"仅从参数申请指南查一周后失败原因",
		"查上面的文档，支付为什么失败？",
		"按照之前的资料查这个问题",
		"only search the named guide for payment failure",
	} {
		e := &AgentEngine{}
		e.SetFAQBroadDocumentSearch(query)
		require.Empty(t, e.faqBroadDocumentQuery, query)
	}
}

func TestFAQBroadDocumentSearchKeepsSymptomQualifiers(t *testing.T) {
	for _, query := range []string{
		"只有上线一周以后才失败，是什么原因？",
		"只在 Android 出现支付校验失败，是什么原因？",
		"仅 Google 渠道受影响，支付校验失败是什么原因？",
	} {
		e := &AgentEngine{}
		e.SetFAQBroadDocumentSearch(query)
		require.Equal(t, query, e.faqBroadDocumentQuery)
	}
}
