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
		search.output = `<retrieval><document id="other"/></retrieval>`
	}
	reader := runtimeRegressionReader()
	reader.execute = func(context.Context, json.RawMessage) (*types.ToolResult, error) {
		time.Sleep(50 * time.Millisecond)
		return &types.ToolResult{Success: true, Output: "evidence"}, nil
	}
	e.toolRegistry.RegisterTool(reader)
	for _, tool := range e.buildToolsForLLM() {
		if tool.Function.Name == agenttools.ToolReadDocument {
			require.NotContains(t, string(tool.Function.Parameters), `"retrieval"`, "ordinary reads must not require the review protocol after two searches")
		}
	}
	response := &types.ChatResponse{ToolCalls: []types.LLMToolCall{{ID: "a", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"a","retrieval":{"gap_id":"gap-1","action":"read","review":{"call_id":"mistyped-observation","status":"progress","quotes":["quote from another search"]}}}`}}, {ID: "b", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"b"}`}}}}
	var step types.AgentStep
	require.Nil(t, e.executeToolCallsParallel(t.Context(), response, &step, 0, "session", "message"))
	require.Len(t, step.ToolCalls, 2)
	require.Len(t, reader.calls, 2)
	for _, call := range step.ToolCalls {
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Empty(t, e.retrieval.snapshot().Gaps[0].LastReview, "a redundant progress claim must not update evidence state")
	unknown := retrievalProbeCall(e, "invented", agenttools.ToolReadDocument, `{"id":"invented","retrieval":{"gap_id":"gap-1","action":"read","review":{"call_id":"mistyped-observation","status":"progress"}}}`)
	require.False(t, unknown.Result.Success, "a progress claim cannot authorize an unobserved reference")
	require.Len(t, reader.calls, 2)
	for _, message := range e.appendToolResults(nil, step) {
		require.NotContains(t, message.Content, "<retrieval_state>", "the next round supplies state once; tool history must not repeat it")
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
			require.Equal(t, 1, next.retrieval.countSearches(next.retrieval.gap("gap-1"), "", ""))
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
					reuse := admission == "reuse" && status == retrievalPending
					// An unavailable boundary check fails open: the fact is admitted
					// and the search runs under the ledger's own rules, exactly as an
					// allow decision would.
					want := admission == "allow" || admission == "read" || admission == "unavailable" || reuse
					require.Equal(t, want, call.Result.Success, call.Result.Error)
					require.Equal(t, before.Gaps[0].Status, next.retrieval.snapshot().Gaps[0].Status)
					for i, attempt := range before.Attempts {
						require.Equal(t, attempt.ID, next.retrieval.snapshot().Attempts[i].ID)
					}
					if reuse {
						require.Len(t, search.calls, 1)
						require.Len(t, next.retrieval.snapshot().Gaps, 1)
						require.Equal(t, "gap-1", next.retrieval.snapshot().Attempts[len(before.Attempts)].GapID)
					} else if want {
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

// After a progress review the gap leaves runtime management, yet two reads of
// observed documents must still be ordered rather than race on one in-flight slot.
func TestReviewedReadBatchIsSerialized(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	search.output = `<retrieval><document id="a"/><document id="b"/></retrieval>`
	require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	reader := runtimeRegressionReader()
	e.toolRegistry.RegisterTool(reader)
	review := retrievalProbeCall(e, "review", reviewRetrievalTool, `{"gap_id":"gap-1","call_id":"search","status":"progress","quotes":["document"]}`)
	require.True(t, review.Result.Success, review.Result.Error)
	require.False(t, e.usesRuntimeRetrievalTool(agenttools.ToolReadDocument), "a reviewed gap uses the structured protocol")
	response := &types.ChatResponse{ToolCalls: []types.LLMToolCall{
		{ID: "a", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"a"}`}},
		{ID: "b", Function: types.FunctionCall{Name: agenttools.ToolReadDocument, Arguments: `{"id":"b"}`}},
	}}
	var step types.AgentStep
	require.Nil(t, e.executeToolCallsParallel(t.Context(), response, &step, 0, "session", "message"))
	for _, call := range step.ToolCalls {
		require.True(t, call.Result.Success, call.Result.Error)
	}
	require.Len(t, reader.calls, 2)
}

func TestObservedDocumentReferenceSurvivesIllegalText(t *testing.T) {
	render := func(text string) string {
		return `<documents><document id="a"><chunk id="c1"><content>` + text + `</content></chunk></document><document id="b"><chunk id="c2"/></document></documents>`
	}
	for name, tc := range map[string]struct {
		content, id string
		want        bool
	}{
		"clean text":           {render("配置说明"), "b", true},
		"form feed from a PDF": {render("第一页\f第二页"), "b", true},
		"NUL byte":             {render("a\x00b"), "c2", true},
		"invalid UTF-8":        {render("a\xffb"), "b", true},
		"unknown id":           {render("第一页\f第二页"), "z", false},
		"empty id":             {render("x"), "", false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, observedDocumentReference(tc.content, tc.id))
		})
	}
}
