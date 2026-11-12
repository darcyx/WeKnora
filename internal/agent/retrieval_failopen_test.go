package agent

import (
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// A boundary check that cannot answer is not a decision. Retrieval must keep
// working: a second model call may never be the reason a question cannot reach
// the knowledge base.
func TestRetrievalGuardOutageStillSearches(t *testing.T) {
	for _, name := range []string{"timeout", "invalid", "missing decision", "unknown decision", "missing user fields", "blank user field", "duplicate user field"} {
		t.Run(name, func(t *testing.T) {
			e, tool := newRetrievalProbeEngine(t)
			e.EnableRetrievalGuard()
			if name == "timeout" {
				e.chatModel = &unavailableContinuationChat{&mockChat{}}
			} else {
				replies := map[string]string{
					"invalid":              `not a decision`,
					"missing decision":     `{"reason":"cannot decide"}`,
					"unknown decision":     `{"decision":"unknown","reason":"cannot decide"}`,
					"missing user fields":  `{"decision":"needs_user","reason":"platform needed"}`,
					"blank user field":     `{"decision":"needs_user","missing_user_fields":[" "],"reason":"platform needed"}`,
					"duplicate user field": `{"decision":"needs_user","missing_user_fields":["platform","platform"],"reason":"platform needed"}`,
				}
				e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: replies[name]}
			}
			call := retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
			require.True(t, call.Result.Success, call.Result.Error)
			require.Len(t, tool.calls, 1, "the search must reach the tool")
			state := e.retrieval.snapshot()
			require.Len(t, state.Gaps, 1, "the fact is admitted under the ledger's own rules")
			require.Len(t, state.Attempts, 1, "the attempt stays observable")
			require.Equal(t, "gap-1", state.Attempts[0].GapID)
		})
	}
}

// A reading check that cannot answer must not make an already observed source
// unreadable.
func TestRetrievalGuardOutageStillReads(t *testing.T) {
	e, search := newRetrievalProbeEngine(t)
	e.EnableRetrievalGuard()
	search.output = `<retrieval><document id="a"/></retrieval>`
	require.True(t, retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read",
		[]byte(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)), output: "document content"}
	e.toolRegistry.RegisterTool(reader)
	e.chatModel = &unavailableContinuationChat{&mockChat{}}
	// Reading needs the structured protocol, which the ledger still enforces.
	call := retrievalProbeCall(e, "read", agenttools.ToolReadDocument,
		`{"id":"a","retrieval":{"gap_id":"gap-1","action":"read","lead":"a"}}`)
	require.True(t, call.Result.Success, call.Result.Error)
	require.Len(t, reader.calls, 1)
}

// A verdict that neither allows nor confirms required user fields must not
// replace a usable answer: only a confirmed needs_user acts on it.
func TestDirectAnswerSurvivesUnconfirmedBoundaryVerdict(t *testing.T) {
	for _, reply := range []string{
		`{"decision":"stop","reason":"uncertain"}`,
		`{"decision":"continue","reason":"same goal"}`,
		`{"decision":"reuse","gap_id":"gap-1","reason":"already asked"}`,
		`not a decision`,
	} {
		t.Run(reply, func(t *testing.T) {
			e, _ := newRetrievalProbeEngine(t)
			e.EnableRetrievalGuard()
			const answer = "可以在设置中配置。"
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: reply}
			got, err := e.publicAnswer(t.Context(), answer, nil)
			require.NoError(t, err)
			require.Equal(t, answer, got, "an unconfirmed verdict leaves the answer untouched")
			require.Empty(t, e.retrieval.snapshot().Gaps)
		})
	}
}
