package agent

import (
	"context"
	"encoding/json"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type faqDispatchTool struct {
	agenttools.BaseTool
	calls int
	check func(context.Context, json.RawMessage)
}

func (t *faqDispatchTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	t.calls++
	if t.check != nil {
		t.check(ctx, args)
	}
	return &types.ToolResult{Success: true, Output: "document evidence"}, nil
}

func faqDispatchFixture(t *testing.T, model *mockChat) (*AgentEngine, *faqDispatchTool) {
	t.Helper()
	e := newTestEngine(t, model)
	e.config.SearchTargets = types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "faq", TenantID: 1},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "doc", KnowledgeIDs: []string{"selected-doc"}, TenantID: 1},
	}
	e.toolRegistry = agenttools.NewToolRegistry()
	tool := &faqDispatchTool{BaseTool: agenttools.NewBaseTool(agenttools.ToolSearchKnowledge, "search", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1},"knowledge_base_ids":{"type":"array","items":{"type":"string"}}},"required":["query"],"additionalProperties":false}`))}
	e.toolRegistry.RegisterTool(tool)
	e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
	return e, tool
}

func TestMergedRecoveryKeepsNonSearchDiagnostics(t *testing.T) {
	graph := &types.ToolResult{Success: true, Output: "Recovered isolated graph stage", Data: map[string]any{
		"display_type": "graph_query_results", "results": []map[string]any{{"chunk_id": "graph-chunk", "content": "graph evidence"}},
	}}
	failed := &types.ToolResult{Output: "The isolated graph stage did not complete", Error: "graph timeout"}
	for _, tc := range []struct {
		name        string
		left, right *types.ToolResult
		success     bool
	}{
		{"successful graph stages", graph, graph, true},
		{"partial failure", graph, failed, true},
		{"all failed", failed, failed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeFAQSearchResult(tc.left, tc.right)
			require.Equal(t, tc.success, merged.Success)
			require.Equal(t, "search_stages", merged.Data["display_type"])
			require.Contains(t, merged.Output, tc.left.Output)
			require.Contains(t, merged.Output, tc.right.Output)
			require.Contains(t, merged.Error, tc.right.Error)
			stages := merged.Data["stages"].([]map[string]any)
			require.Equal(t, tc.left.Data, stages[0]["data"])
			require.Equal(t, tc.right.Error, stages[1]["error"])
		})
	}
}

// A skill read and a knowledge search issued in the same reply both run, in
// model order, with the provider metadata that authorizes the skill preserved.
func TestKnowledgeSearchAndSkillReadBothRunInOneRound(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(map[bool]string{false: "sequential", true: "parallel"}[parallel], func(t *testing.T) {
			signature := types.ToolCallMetadata{"thought_signature": json.RawMessage(`"original-signature"`)}
			model := &mockChat{responses: []mockResponse{
				{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: []types.LLMToolCall{
					{ID: "skill", Type: "function", ProviderMetadata: signature, Function: types.FunctionCall{Name: agenttools.ToolReadFile, Arguments: `{"path":"skill://sdk-operation-guide/SKILL.md"}`}},
					{ID: "lookup", Type: "function", Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: `{"query":"sandbox"}`}},
				}}}},
				{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "文档回复", Done: true}}},
			}}
			e, search := faqDispatchFixture(t, model)
			e.EnableRuntimeRetrieval()
			e.chatModel = &runtimeRetrievalChat{mockChat: model}
			e.config.ParallelToolCalls = parallel
			reader := newCountingTool(agenttools.ToolReadFile)
			e.toolRegistry.RegisterTool(reader)
			var outcomes []string
			e.eventBus.On(event.EventAgentToolResult, func(_ context.Context, evt event.Event) error {
				result := evt.Data.(event.AgentToolResultData)
				if result.Success {
					outcomes = append(outcomes, result.ToolCallID)
				}
				return nil
			})
			state, err := e.Execute(t.Context(), "s", "a", "TapTap sandbox configuration", nil)
			require.NoError(t, err)
			require.Equal(t, "文档回复", state.FinalAnswer)
			require.Equal(t, 1, reader.calls)
			require.Equal(t, 1, search.calls)
			require.Equal(t, []string{"skill", "lookup"}, outcomes)
			found := false
			for _, msg := range model.calls[1] {
				if len(msg.ToolCalls) == 2 {
					require.Equal(t, "skill", msg.ToolCalls[0].ID)
					require.Equal(t, signature, msg.ToolCalls[0].ProviderMetadata)
					found = true
				}
			}
			require.True(t, found)
		})
	}
}
