package agent

import (
	"context"
	"encoding/json"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"

	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestThinkingOffOnlyBeforeEvidence(t *testing.T) {
	ctx := context.Background()
	tools := []chat.Tool{{Type: "function"}}
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.config.ReasoningEffort = "minimal"

	require.Equal(t, api.ReasoningMinimal, e.roundReasoningEffort(ctx, tools), "opt-in unset keeps the configured level")

	t.Setenv(skipPreEvidenceThinkingEnv, "true")
	require.Equal(t, api.ReasoningOff, e.roundReasoningEffort(ctx, tools), "a tool round before any retrieval skips thinking")
	require.Equal(t, api.ReasoningMinimal, e.roundReasoningEffort(ctx, nil), "the forced answer round always thinks")

	e.executeToolCallsParallel(ctx, searchCalls("alpha"), &types.AgentStep{}, 0, "session", "message")
	require.Equal(t, api.ReasoningMinimal, e.roundReasoningEffort(ctx, tools), "the round that sees results thinks")

	plain := newTestEngine(t, &mockChat{})
	plain.config.ReasoningEffort = "minimal"
	require.Equal(t, api.ReasoningMinimal, plain.roundReasoningEffort(ctx, tools), "no retrieval ledger, no signal")
}

func TestStableRetrievalToolsStayIdenticalAcrossSearches(t *testing.T) {
	ctx := context.Background()
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.beginTurn("question", nil, &types.AgentState{})
	require.Len(t, e.buildToolsForLLM(), 2, "runtime retrieval sends the original tools")
	require.Empty(t, e.retrievalStatePrompt())

	before, err := json.Marshal(e.buildToolsForLLM())
	require.NoError(t, err)
	require.NotContains(t, string(before), "review_retrieval")
	require.NotContains(t, string(before), `"retrieval"`)

	e.executeToolCallsParallel(ctx, searchCalls("a", "b", "c", "d"), &types.AgentStep{}, 0, "session", "message")
	after, err := json.Marshal(e.buildToolsForLLM())
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "the tool prefix must never mutate mid-turn, keeping prompt prefix cache warm")
}

func TestResourceNavigationFastAnswerUsesOneWikiAndProtectsOtherWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*AgentEngine, *[]chat.Message)
		want   chat.ReasoningEffort
	}{
		{"one wiki", func(*AgentEngine, *[]chat.Message) {}, api.ReasoningMinimal},
		{"not eligible", func(e *AgentEngine, _ *[]chat.Message) { e.config.ResourceNavigationFastAnswer = false }, api.ReasoningMinimal},
		{"skill used", func(_ *AgentEngine, m *[]chat.Message) {
			(*m)[1].ToolCalls = append((*m)[1].ToolCalls, chat.ToolCall{Function: chat.FunctionCall{Name: "read_file"}})
		}, api.ReasoningMinimal},
		{"extra retrieval", func(e *AgentEngine, _ *[]chat.Message) {
			e.retrieval.state.Attempts = append(e.retrieval.state.Attempts, retrievalAttempt{Tool: "search_knowledge"})
		}, api.ReasoningMinimal},
		{"multiple pages", func(e *AgentEngine, _ *[]chat.Message) {
			e.retrieval.state.Attempts[0].Content += "<wiki_page><content>another</content></wiki_page>"
		}, api.ReasoningMinimal},
		{"truncated", func(e *AgentEngine, _ *[]chat.Message) { e.retrieval.state.Attempts[0].Content += "[output truncated:" }, api.ReasoningMinimal},
		{"unreviewed wiki", func(e *AgentEngine, _ *[]chat.Message) {
			e.retrieval.state.Attempts[0].Content += "资料尚未经审核"
		}, api.ReasoningMinimal},
		{"empty wiki body", func(e *AgentEngine, _ *[]chat.Message) {
			e.retrieval.state.Attempts[0].Content = "<wiki_page><content> </content></wiki_page>"
		}, api.ReasoningMinimal},
		{"failed", func(e *AgentEngine, _ *[]chat.Message) { e.retrieval.state.Attempts[0].Outcome = retrievalInput }, api.ReasoningMinimal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "说明", Done: true, FinishReason: "stop"}}}}}
			e := newTestEngine(t, model)
			e.config.ReasoningEffort = "minimal"
			e.config.ResourceNavigationFastAnswer = true
			e.retrieval = newRetrievalControl("服务端文档是什么", "scope")
			e.retrieval.state.UserInputIDs = []string{"q"}
			e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "q", Tool: agenttools.ToolWikiReadPage, Completed: true, Outcome: retrievalSuccess, Content: "<wiki_page><content>正文</content></wiki_page>"}}
			messages := []chat.Message{{Role: "user", Content: "服务端文档是什么"}, {Role: "assistant", ToolCalls: []chat.ToolCall{{Function: chat.FunctionCall{Name: agenttools.ToolWikiReadPage}}}}}
			tc.change(e, &messages)
			_, err := e.streamThinkingToEventBus(t.Context(), messages, []chat.Tool{{Type: "function"}}, 1, "session")
			require.NoError(t, err)
			require.Len(t, model.opts, 1)
			require.Equal(t, tc.want, model.opts[0].ReasoningEffort)
			if tc.name == "one wiki" {
				require.Contains(t, model.opts[0].RuntimeNotice, "one to three sentences")
			} else {
				require.NotContains(t, model.opts[0].RuntimeNotice, "one to three sentences")
			}
		})
	}
}

func TestResourceNavigationRunsWikiThenFastAnswer(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{{ID: "read", Type: "function", Function: types.FunctionCall{Name: agenttools.ToolWikiReadPage, Arguments: `{"slugs":["backend"]}`}}}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "服务端文档介绍服务端联调。", Done: true, FinishReason: "stop"}}},
	}}
	e := newTestEngine(t, model)
	e.config.ReasoningEffort = "minimal"
	e.config.ResourceNavigationFastAnswer = true
	e.config.SearchTargets = types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
	e.toolRegistry = agenttools.NewToolRegistry()
	e.toolRegistry.RegisterTool(&retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolWikiReadPage, "wiki", json.RawMessage(`{"type":"object","properties":{"slugs":{"type":"array","items":{"type":"string"}}},"required":["slugs"]}`)), output: "<wiki_page><content>服务端联调和故障定位。</content></wiki_page>"})
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("u", "服务端文档是什么")
	_, err := e.Execute(t.Context(), "session", "message", "服务端文档是什么", nil)
	require.NoError(t, err)
	require.Len(t, model.opts, 2)
	require.Equal(t, api.ReasoningMinimal, model.opts[0].ReasoningEffort)
	require.Equal(t, api.ReasoningMinimal, model.opts[1].ReasoningEffort)
	require.Contains(t, model.opts[1].RuntimeNotice, "one to three sentences")
	require.Contains(t, model.opts[1].RuntimeNotice, "For an entry or location")
	require.Contains(t, model.opts[1].RuntimeNotice, "if no entry is provided, state the gap")
}

func TestResourceNavigationSteerDisablesFastAnswer(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	e.config.ReasoningEffort = "minimal"
	e.config.ResourceNavigationFastAnswer = true
	e.retrieval = newRetrievalControl("服务端文档是什么", "scope")
	e.retrieval.state.UserInputIDs = []string{"message:u"}
	e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "message:u", Tool: agenttools.ToolWikiReadPage, Completed: true, Outcome: retrievalSuccess, Content: "<wiki_page><content>服务端资料</content></wiki_page>"}}
	messages := []chat.Message{{Role: "user", Content: "服务端文档是什么"}, {Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "w", Function: chat.FunctionCall{Name: agenttools.ToolWikiReadPage}}}}, {Role: "tool", ToolCallID: "w", Name: agenttools.ToolWikiReadPage, Content: "服务端资料"}}
	require.True(t, e.resourceNavigationHasOneWiki(messages))
	e.SetSteerSink(&fakeSteerSink{queued: []map[string]interface{}{steerEntry("s", "再给出实现代码")}})
	require.Equal(t, 1, e.drainSteerMessages(t.Context(), &types.AgentState{}, &messages, "session", "message"))
	require.False(t, e.resourceNavigationHasOneWiki(messages))
	require.Equal(t, api.ReasoningMinimal, e.roundReasoningEffort(t.Context(), []chat.Tool{{Type: "function"}}))
}

func TestResourceNavigationEntryNoticeKeepsEvidenceOrGap(t *testing.T) {
	for _, entry := range []string{"https://docs.example.test/server", ""} {
		t.Run(map[bool]string{true: "source entry", false: "missing entry"}[entry != ""], func(t *testing.T) {
			model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "答复", Done: true, FinishReason: "stop"}}}}}
			e := newTestEngine(t, model)
			e.config.ReasoningEffort = "minimal"
			e.config.ResourceNavigationFastAnswer = true
			e.retrieval = newRetrievalControl("服务端文档入口在哪里", "scope")
			e.retrieval.state.UserInputIDs = []string{"q"}
			e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "q", Tool: agenttools.ToolWikiReadPage, Completed: true, Outcome: retrievalSuccess, Content: "<wiki_page><content>服务端资料 " + entry + "</content></wiki_page>"}}
			messages := []chat.Message{{Role: "user", Content: "服务端文档入口在哪里"}, {Role: "assistant", ToolCalls: []chat.ToolCall{{Function: chat.FunctionCall{Name: agenttools.ToolWikiReadPage}}}}}
			_, err := e.streamThinkingToEventBus(t.Context(), messages, []chat.Tool{{Type: "function"}}, 1, "session")
			require.NoError(t, err)
			require.Equal(t, api.ReasoningMinimal, model.opts[0].ReasoningEffort)
			require.Contains(t, model.opts[0].RuntimeNotice, "preserve the address or location directly supported by the evidence")
			require.Contains(t, model.opts[0].RuntimeNotice, "if no entry is provided, state the gap and never construct an address")
		})
	}
}
