package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type unavailableContinuationChat struct{ *mockChat }

func (*unavailableContinuationChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return nil, context.DeadlineExceeded
}

func TestRuntimeContinuationFailureKeepsBudgetAndCancellation(t *testing.T) {
	for _, reply := range []string{"error", "invalid"} {
		t.Run(reply, func(t *testing.T) {
			e, _ := newRuntimeRetrievalProbeEngine(t)
			e.EnableRuntimeRetrieval()
			require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
			raw, err := json.Marshal(e.retrieval.snapshot())
			require.NoError(t, err)
			next, _ := newRuntimeRetrievalProbeEngine(t)
			next.EnableRuntimeRetrieval()
			next.SetRetrievalUserMessage("next", "再查一下")
			next.beginTurn("再查一下", nil, &types.AgentState{})
			next.SetRetrievalPersistence(raw, nil)
			if reply == "error" {
				next.chatModel = &unavailableContinuationChat{&mockChat{}}
			} else {
				next.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"stop","reason":"uncertain"}`}
			}
			require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
			require.Len(t, next.retrieval.snapshot().Attempts, 1)
			require.Empty(t, next.retrievalSearchGapID, "unavailable classification must not bind the input to an old fact")
			next.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"reuse","gap_id":"gap-1","reason":"the user is asking to retry the same question"}`}
			for i := 1; i < questionSearchGrace; i++ {
				c := retrievalProbeCall(next, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"q%d"}`, i))
				require.True(t, c.Result.Success, c.Result.Error)
			}
			require.False(t, retrievalProbeCall(next, "spent", agenttools.ToolSearchKnowledge, `{"query":"again"}`).Result.Success)
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, next.initializeRetrievalContinuation(cancelled), context.Canceled)
		})
	}
}

func TestRuntimeReadFailuresAreLocalToOperation(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	search.output = `<retrieval><document id="a"/><document id="b"/><document id="c"/></retrieval>`
	require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	for i := 1; i < questionSearchGrace; i++ {
		require.True(t, retrievalProbeCall(e, fmt.Sprint(i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"q%d"}`, i)).Result.Success)
	}

	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)), err: context.DeadlineExceeded}
	e.toolRegistry.RegisterTool(reader)
	for _, id := range []string{"a", "b"} {
		require.False(t, retrievalProbeCall(e, "read-"+id, agenttools.ToolReadDocument, fmt.Sprintf(`{"id":"%s"}`, id)).Result.Success)
		require.False(t, retrievalProbeCall(e, "retry-"+id, agenttools.ToolReadDocument, fmt.Sprintf(`{"id":"%s"}`, id)).Result.Success)
		require.False(t, retrievalProbeCall(e, "spent-"+id, agenttools.ToolReadDocument, fmt.Sprintf(`{"id":"%s"}`, id)).Result.Success)
	}
	require.Len(t, reader.calls, 4, "each independent operation has one recovery")
	reader.err = nil
	reader.output = "usable evidence"
	c := retrievalProbeCall(e, "read-c", agenttools.ToolReadDocument, `{"id":"c"}`)
	require.True(t, c.Result.Success, c.Result.Error)
	_, err := restoreRetrievalControl(e.retrieval.snapshot())
	require.NoError(t, err)
}

func TestRuntimeCanSearchAfterFailedRead(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	search.output = `<retrieval><document id="a"/></retrieval>`
	require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object"}`)), err: context.DeadlineExceeded}
	e.toolRegistry.RegisterTool(reader)
	require.False(t, retrievalProbeCall(e, "read", agenttools.ToolReadDocument, `{"id":"a"}`).Result.Success)
	c := retrievalProbeCall(e, "alternate", agenttools.ToolSearchKnowledge, `{"query":"configuration steps"}`)
	require.True(t, c.Result.Success, c.Result.Error)
	_, err := restoreRetrievalControl(e.retrieval.snapshot())
	require.NoError(t, err)
}

func TestRuntimeCanReloadRedactedReadWithoutRefundingSearch(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	search.output = `<retrieval><document id="a"/></retrieval>`
	require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object"}`)), output: "evidence"}
	e.toolRegistry.RegisterTool(reader)
	require.True(t, retrievalProbeCall(e, "read", agenttools.ToolReadDocument, `{"id":"a"}`).Result.Success)
	raw, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	next, _ := newRuntimeRetrievalProbeEngine(t)
	next.EnableRuntimeRetrieval()
	next.SetRetrievalPersistence(raw, nil)
	require.NoError(t, next.initializeRetrievalContinuation(t.Context()))
	next.toolRegistry.RegisterTool(reader)
	require.Empty(t, next.retrieval.snapshot().Attempts[1].Content)
	c := retrievalProbeCall(next, "reload", agenttools.ToolReadDocument, `{"id":"a"}`)
	require.True(t, c.Result.Success, c.Result.Error)
	require.False(t, retrievalProbeCall(next, "duplicate", agenttools.ToolReadDocument, `{"id":"a"}`).Result.Success)
	require.Len(t, reader.calls, 2)
	require.Equal(t, 1, next.retrieval.searchCount(next.retrieval.gap("gap-1")))
}

func TestRuntimeIndependentFactIgnoresUnrelatedBlocker(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	blocker, err := e.retrieval.register(retrievalFactProposal{GoalQuote: "TapTap", Fact: "payment status"})
	require.NoError(t, err)
	require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: blocker, Status: retrievalNeedsUser, MissingUserFields: []string{"order"}}))
	independent, err := e.retrieval.registerFact(retrievalFactProposal{GoalQuote: "sandbox", Fact: "sandbox explanation"}, false)
	require.NoError(t, err)
	e.retrievalSearchGapID = independent
	require.True(t, e.usesRuntimeRetrieval())
	e.retrievalSearchGapID = blocker
	require.False(t, e.usesRuntimeRetrieval())
}
