package agent

import (
	"encoding/json"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestObservedDocumentReadReusesAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, target, verdict   string
		guard, noProgress, want bool
	}{
		{"observed source", "doc", "allow", true, false, true},
		{"unseen source", "invented", "allow", true, false, false},
		{"semantic rejection", "doc", "stop", true, false, false},
		{"explicit no progress", "doc", "allow", true, true, false},
		{"unguarded keeps original protocol", "doc", "allow", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, search := newRetrievalProbeEngine(t)
			search.output = `<retrieval><document id="doc"><content>配置步骤见后续章节。</content></document></retrieval>`
			first := retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
			require.True(t, first.Result.Success, first.Result.Error)
			if tc.noProgress {
				require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: "gap-1", CallID: e.retrievalCallReference("search"), Status: retrievalNoProgress}))
			}
			reader := &retrievalProbe{BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)), output: search.output}
			e.toolRegistry.RegisterTool(reader)
			e.retrievalGuard = tc.guard
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"` + tc.verdict + `","reason":"assessed whether the observed source advances the remaining question"}`}
			before := e.retrieval.snapshot()
			args := `{"id":"` + tc.target + `","retrieval":{"gap_id":"gap-1","action":"read"}}`
			call := retrievalProbeCall(e, "read", agenttools.ToolReadDocument, args)
			require.Equal(t, tc.want, call.Result.Success, call.Result.Error)
			if !tc.want {
				require.Empty(t, reader.calls)
				require.Equal(t, before, e.retrieval.snapshot())
				require.Equal(t, true, call.Result.Data[retrievalDispatchBlocked])
				return
			}
			require.Len(t, reader.calls, 1)
			require.JSONEq(t, `{"id":"doc"}`, string(reader.calls[0]))
			repeated := retrievalProbeCall(e, "repeat", agenttools.ToolReadDocument, args)
			require.False(t, repeated.Result.Success, "admission cannot authorize repeating the same range")
			require.Len(t, reader.calls, 1)
		})
	}
}

func TestOmittedCandidateDocumentIsObservedForRead(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	const doc = "33333333-3333-3333-3333-333333333333"
	result := &types.ToolResult{Success: true, Data: map[string]interface{}{
		"display_type": "search_results", "mode": "hybrid", "results": []map[string]interface{}{{"chunk_id": "11111111-1111-1111-1111-111111111111", "knowledge_id": "22222222-2222-2222-2222-222222222222", "content": "known"}},
		"omitted_for_budget": 1, "omitted_candidates": []map[string]interface{}{{"knowledge_id": doc, "knowledge_base_id": "44444444-4444-4444-4444-444444444444", "title": "next"}},
	}}
	text := e.modelContext.ModelToolResultForTool(agenttools.ToolSearchKnowledge, result)
	text = e.modelContext.DecodeObservationText(text)
	require.True(t, observedDocumentReference(text, doc), text)
}
