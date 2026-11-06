package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func runtimeRegressionReader() *retrievalProbe {
	return &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)), output: "evidence"}
}

func TestRuntimeBatchReadAfterGrace(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	search.output = `<retrieval><document id="a"/><document id="b"/></retrieval>`
	for i := 0; i < questionSearchGrace; i++ {
		require.True(t, retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"q%d"}`, i)).Result.Success)
	}
	reader := runtimeRegressionReader()
	reader.execute = func(context.Context, json.RawMessage) (*types.ToolResult, error) {
		time.Sleep(50 * time.Millisecond)
		return &types.ToolResult{Success: true, Output: "evidence"}, nil
	}
	e.toolRegistry.RegisterTool(reader)
	response := &types.ChatResponse{ToolCalls: []types.LLMToolCall{{ID: "a", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"a"}`}}, {ID: "b", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"b"}`}}}}
	var step types.AgentStep
	require.Nil(t, e.executeToolCallsParallel(t.Context(), response, &step, 0, "session", "message"))
	require.Len(t, step.ToolCalls, 2)
	require.Len(t, reader.calls, 2)
	for _, call := range step.ToolCalls {
		require.True(t, call.Result.Success, call.Result.Error)
	}
}

func TestRuntimeReloadAfterRecoveredFailure(t *testing.T) {
	for _, interveningRead := range []bool{false, true} {
		t.Run(fmt.Sprint(interveningRead), func(t *testing.T) {
			e, search := newRuntimeRetrievalProbeEngine(t)
			e.EnableRuntimeRetrieval()
			search.output = `<retrieval><document id="a"/><document id="b"/></retrieval>`
			require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
			reader := runtimeRegressionReader()
			reader.err = context.DeadlineExceeded
			e.toolRegistry.RegisterTool(reader)
			require.False(t, retrievalProbeCall(e, "read", agenttools.ToolReadDocument, `{"id":"a"}`).Result.Success)
			reader.err = nil
			require.True(t, retrievalProbeCall(e, "recover", agenttools.ToolReadDocument, `{"id":"a"}`).Result.Success)
			if interveningRead {
				require.True(t, retrievalProbeCall(e, "other", agenttools.ToolReadDocument, `{"id":"b"}`).Result.Success)
			}
			raw, err := json.Marshal(e.retrieval.snapshot())
			require.NoError(t, err)
			next, _ := newRuntimeRetrievalProbeEngine(t)
			next.EnableRuntimeRetrieval()
			next.SetRetrievalPersistence(raw, nil)
			require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
			next.toolRegistry.RegisterTool(reader)
			require.Empty(t, next.retrieval.snapshot().Attempts[2].Content)
			call := retrievalProbeCall(next, "reload", agenttools.ToolReadDocument, `{"id":"a"}`)
			require.True(t, call.Result.Success, call.Result.Error)
			attempts := next.retrieval.snapshot().Attempts
			require.Equal(t, retrievalRead, attempts[len(attempts)-1].Action)
			require.Empty(t, attempts[len(attempts)-1].RecoveryCallID)
			require.False(t, retrievalProbeCall(next, "duplicate", agenttools.ToolReadDocument, `{"id":"a"}`).Result.Success)
			require.Equal(t, 1, next.retrieval.searchCount(next.retrieval.gap("gap-1")))
			_, err = restoreRetrievalControl(next.retrieval.snapshot())
			require.NoError(t, err)
		})
	}
}

func TestRuntimeNewQuestionAfterContinuationFailure(t *testing.T) {
	for _, status := range []retrievalStatus{retrievalPending, retrievalExhausted, retrievalSatisfied} {
		for _, failure := range []string{"error", "invalid"} {
			for _, admission := range []string{"allow", "reuse", "unavailable", "read"} {
				t.Run(string(status)+"/"+failure+"/"+admission, func(t *testing.T) {
					e, _ := newRuntimeRetrievalProbeEngine(t)
					e.EnableRuntimeRetrieval()
					require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
					if status == retrievalPending {
						for i := 1; i < questionSearchGrace; i++ {
							require.True(t, retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"q%d"}`, i)).Result.Success)
						}
					} else {
						require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", CallID: "search", Status: status, Quotes: []string{"Only general information"}}))
					}
					before := e.retrieval.snapshot()
					raw, err := json.Marshal(before)
					require.NoError(t, err)
					next, search := newRuntimeRetrievalProbeEngine(t)
					next.EnableRuntimeRetrieval()
					question := "微信小游戏隐私协议如何配置"
					if admission == "reuse" {
						question = "再查一下 TapTap 沙盒配置"
					}
					if admission == "read" {
						question = "读取文档 new-doc"
					}
					next.SetRetrievalUserMessage("new", question)
					next.beginTurn(question, nil, &types.AgentState{})
					next.SetRetrievalPersistence(raw, nil)
					next.chatModel = &unavailableContinuationChat{&mockChat{}}
					if failure == "invalid" {
						next.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"stop","reason":"uncertain"}`}
					}
					require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
					require.Empty(t, next.retrievalSearchGapID)
					require.Len(t, next.retrieval.snapshot().Attempts, len(before.Attempts))
					switch admission {
					case "allow", "read":
						next.chatModel = &runtimeRetrievalChat{mockChat: &mockChat{}}
					case "reuse":
						next.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"reuse","gap_id":"gap-1","reason":"same question rephrased"}`}
					case "unavailable":
						next.chatModel = &unavailableContinuationChat{&mockChat{}}
					}
					var call types.ToolCall
					if admission == "read" {
						next.toolRegistry.RegisterTool(runtimeRegressionReader())
						call = retrievalProbeCall(next, "newread", agenttools.ToolReadDocument, `{"id":"new-doc"}`)
					} else {
						call = retrievalProbeCall(next, "newsearch", agenttools.ToolSearchKnowledge, `{"query":"current question"}`)
					}
					want := admission == "allow" || admission == "read"
					require.Equal(t, want, call.Result.Success, call.Result.Error)
					require.Equal(t, before.Gaps[0].Status, next.retrieval.snapshot().Gaps[0].Status)
					require.Equal(t, len(before.Attempts), next.retrieval.searchCount(next.retrieval.gap("gap-1")))
					if want {
						require.Len(t, next.retrieval.snapshot().Gaps, 2)
						require.Equal(t, "gap-2", next.retrieval.snapshot().Attempts[len(before.Attempts)].GapID)
					} else {
						require.Empty(t, search.calls)
						require.Len(t, next.retrieval.snapshot().Attempts, len(before.Attempts))
						require.Len(t, next.retrieval.snapshot().Gaps, 1)
					}
					_, err = restoreRetrievalControl(next.retrieval.snapshot())
					require.NoError(t, err)
				})
			}
		}
	}
}
