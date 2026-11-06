package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type alwaysSucceedingTool struct {
	agenttools.BaseTool
	calls int
}

func (t *alwaysSucceedingTool) Execute(context.Context, json.RawMessage) (*types.ToolResult, error) {
	t.calls++
	return &types.ToolResult{Success: true, Output: "some search results"}, nil
}

func successfulToolResponse(id string) mockResponse {
	return mockResponse{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer,
		ToolCalls: []types.LLMToolCall{{
			ID:   id,
			Type: "function",
			Function: types.FunctionCall{
				Name:      "search_again",
				Arguments: `{"query":"same query"}`,
			},
		}},
		Done:         true,
		FinishReason: "tool_calls",
	}}}
}

// TestExecuteLoopNudgesAfterRepeatedIdenticalSuccessfulToolCall guards against
// a stuck loop that the failed-call guard cannot see: the model re-issues the
// same tool call and it succeeds every time, so the failure streak never
// trips. Each repeat still costs one LLM round plus the tool's work, so after
// maxRepeatedIdenticalToolCalls identical successful calls the model must be
// nudged to use the results or change the query.
func TestExecuteLoopNudgesAfterRepeatedIdenticalSuccessfulToolCall(t *testing.T) {
	model := &mockChat{responses: []mockResponse{
		successfulToolResponse("call-1"),
		successfulToolResponse("call-2"),
		successfulToolResponse("call-3"),
		{chunks: []types.StreamResponse{{
			ResponseType: types.ResponseTypeAnswer,
			Content:      "Here is the answer using the results.",
			Done:         true,
			FinishReason: "stop",
		}}},
	}}
	engine := newTestEngine(t, model)
	tool := &alwaysSucceedingTool{BaseTool: agenttools.NewBaseTool(
		"search_again", "always succeeds in this test", json.RawMessage(`{"type":"object"}`),
	)}
	engine.toolRegistry = agenttools.NewToolRegistry()
	engine.toolRegistry.RegisterTool(tool)

	state, err := engine.executeLoop(context.Background(), &types.AgentState{}, "find the info",
		emptyMessages(), emptyTools(), "session", "message")

	require.NoError(t, err)
	require.True(t, state.IsComplete)
	require.Equal(t, 3, tool.calls, "identical successful calls must be bounded before the model is nudged")
	require.Len(t, model.calls, 4)

	var guidance strings.Builder
	for _, message := range model.calls[3] {
		if message.Role == "user" {
			guidance.WriteString(message.Content)
		}
	}
	require.Contains(t, guidance.String(), "has been called")
	require.Contains(t, guidance.String(), "change the query")
}

// TestRepeatedToolCallGuardResetsOnDifferentArguments ensures the guard only
// counts identical calls. Varying the query (or any argument) must not trip it,
// otherwise legitimate multi-step retrieval would be interrupted.
func TestRepeatedToolCallGuardResetsOnDifferentArguments(t *testing.T) {
	g := &loopGuards{}
	ok := types.ToolCall{Name: "search", Args: map[string]interface{}{"query": "a"}, Result: &types.ToolResult{Success: true}}
	diff := types.ToolCall{Name: "search", Args: map[string]interface{}{"query": "b"}, Result: &types.ToolResult{Success: true}}
	fail := types.ToolCall{Name: "search", Args: map[string]interface{}{"query": "a"}, Result: &types.ToolResult{Success: false}}

	// First identical call starts the streak at 1.
	if name, count, nudge := g.recordRepeatedToolCall(ok); name != "" || count != 1 || nudge {
		t.Fatalf("first call = (%q, %d, %v), want (\"\", 1, false)", name, count, nudge)
	}
	// Second identical call reaches 2, still no nudge.
	if name, count, nudge := g.recordRepeatedToolCall(ok); name != "" || count != 2 || nudge {
		t.Fatalf("second call = (%q, %d, %v), want (\"\", 2, false)", name, count, nudge)
	}
	// A different call resets the streak.
	if name, count, nudge := g.recordRepeatedToolCall(diff); name != "" || count != 1 || nudge {
		t.Fatalf("different call = (%q, %d, %v), want (\"\", 1, false)", name, count, nudge)
	}
	// A failed call also resets: only successes are tracked here.
	if name, count, nudge := g.recordRepeatedToolCall(fail); name != "" || count != 0 || nudge {
		t.Fatalf("failed call = (%q, %d, %v), want (\"\", 0, false)", name, count, nudge)
	}
	// Three identical successes from scratch must nudge exactly once.
	g = &loopGuards{}
	g.recordRepeatedToolCall(ok)
	g.recordRepeatedToolCall(ok)
	name, count, nudge := g.recordRepeatedToolCall(ok)
	if name != "search" || count != 3 || !nudge {
		t.Fatalf("third identical call = (%q, %d, %v), want (\"search\", 3, true)", name, count, nudge)
	}
	// A fourth identical call must not nudge again.
	name, count, nudge = g.recordRepeatedToolCall(ok)
	if name != "" || count != 4 || nudge {
		t.Fatalf("fourth identical call = (%q, %d, %v), want (\"\", 4, false)", name, count, nudge)
	}
}
