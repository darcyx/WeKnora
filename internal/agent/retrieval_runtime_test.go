package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type retrievalProbe struct {
	agenttools.BaseTool
	calls   []json.RawMessage
	output  string
	result  *types.ToolResult
	err     error
	execute func(context.Context, json.RawMessage) (*types.ToolResult, error)
}

func searchCalls(queries ...string) *types.ChatResponse {
	calls := make([]types.LLMToolCall, 0, len(queries))
	for i, q := range queries {
		raw, _ := json.Marshal(map[string]any{"query": q})
		calls = append(calls, types.LLMToolCall{
			ID:       fmt.Sprintf("search-%d", i+1),
			Type:     "function",
			Function: types.FunctionCall{Name: agenttools.ToolSearchKnowledge, Arguments: string(raw)},
		})
	}
	return &types.ChatResponse{ToolCalls: calls, FinishReason: "tool_calls"}
}

func (p *retrievalProbe) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	p.calls = append(p.calls, append(json.RawMessage(nil), args...))
	if p.execute != nil {
		return p.execute(ctx, args)
	}
	if p.err != nil {
		return p.result, p.err
	}
	if p.result != nil {
		return p.result, nil
	}
	out := p.output
	if out == "" {
		out = "search output"
	}
	return &types.ToolResult{
		Success: true,
		Output:  out,
		Data:    map[string]any{"display_type": "search_results", "results": []map[string]any{}},
	}, nil
}

func retrievalScopeTarget(kbID string) *types.SearchTarget {
	return &types.SearchTarget{
		Type:            types.SearchTargetTypeKnowledgeBase,
		KnowledgeBaseID: kbID,
		TenantID:        1,
	}
}

func newRuntimeRetrievalProbeEngine(t *testing.T) (*AgentEngine, *retrievalProbe) {
	t.Helper()
	e := newTestEngine(t, &mockChat{})
	e.config.SearchTargets = types.SearchTargets{retrievalScopeTarget("kb-1")}
	e.toolRegistry = agenttools.NewToolRegistry()
	searchProbe := &retrievalProbe{
		BaseTool: agenttools.NewBaseTool(agenttools.ToolSearchKnowledge, "search", json.RawMessage(`{
			"type":"object",
			"properties":{"query":{"type":"string"},"knowledge_base_ids":{"type":"array","items":{"type":"string"}}},
			"required":["query"]
		}`)),
	}
	readProbe := &retrievalProbe{
		BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{
			"type":"object",
			"properties":{"id":{"type":"string"},"offset":{"type":"integer"}},
			"required":["id"]
		}`)),
	}
	e.toolRegistry.RegisterTool(searchProbe)
	e.toolRegistry.RegisterTool(readProbe)
	e.beginTurn("question", nil, &types.AgentState{})
	return e, searchProbe
}

func newPartialRetrievalProbeEngine(t *testing.T) (*AgentEngine, *retrievalProbe) {
	t.Helper()
	e, probe := newRuntimeRetrievalProbeEngine(t)
	e.config.SearchTargets = types.SearchTargets{
		retrievalScopeTarget("healthy"),
		retrievalScopeTarget("retryable"),
	}
	return e, probe
}

func partialRetrievalResult() *types.ToolResult {
	return &types.ToolResult{
		Success: true,
		Output:  "partial results from healthy",
		Data:    map[string]any{"display_type": "search_results", "results": []map[string]any{}},
		RetrievalFailures: []types.RetrievalFailure{{
			KnowledgeBaseIDs: []string{"retryable"},
			Targets:          types.SearchTargets{retrievalScopeTarget("retryable")},
			Stage:            "search",
			Cause:            context.DeadlineExceeded,
		}},
	}
}

func retrievalProbeCall(e *AgentEngine, id, name, args string) types.ToolCall {
	step := &types.AgentStep{}
	e.executeSingleToolCall(
		context.Background(),
		types.LLMToolCall{ID: id, Function: types.FunctionCall{Name: name, Arguments: args}},
		0, step, 0, 1, "sess", "msg",
	)
	if len(step.ToolCalls) == 0 {
		return types.ToolCall{}
	}
	return step.ToolCalls[0]
}

func TestRuntimeRetrievalNeverMutatesToolSchemas(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("q1", "sandbox config")
	e.beginTurn("sandbox config", nil, &types.AgentState{})

	before := e.buildToolsForLLM()
	beforeJSON, err := json.Marshal(before)
	require.NoError(t, err)
	require.NotContains(t, string(beforeJSON), "review_retrieval")
	require.NotContains(t, string(beforeJSON), `"retrieval"`)

	for i := 1; i <= maxSearchesPerQuestion; i++ {
		call := retrievalProbeCall(e, fmt.Sprintf("s%d", i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"q-%d"}`, i))
		require.True(t, call.Result.Success, call.Result.Error)
	}

	after := e.buildToolsForLLM()
	afterJSON, err := json.Marshal(after)
	require.NoError(t, err)
	require.Equal(t, string(beforeJSON), string(afterJSON), "KB tool schemas must stay byte-identical across the entire turn for prompt prefix caching")
}

func TestRuntimeRetrievalBlocksDuplicateAndEnforcesBudget(t *testing.T) {
	e, probe := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("q1", "sandbox config")
	e.beginTurn("sandbox config", nil, &types.AgentState{})

	first := retrievalProbeCall(e, "s1", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
	require.True(t, first.Result.Success)

	dup := retrievalProbeCall(e, "s2", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
	require.False(t, dup.Result.Success)
	require.Contains(t, dup.Result.Error, "exact search already completed")
	require.Len(t, probe.calls, 1)

	for i := 2; i <= maxSearchesPerQuestion; i++ {
		call := retrievalProbeCall(e, fmt.Sprintf("u%d", i), agenttools.ToolSearchKnowledge, fmt.Sprintf(`{"query":"distinct-%d"}`, i))
		require.True(t, call.Result.Success, call.Result.Error)
	}
	over := retrievalProbeCall(e, "over", agenttools.ToolSearchKnowledge, `{"query":"one-too-many"}`)
	require.False(t, over.Result.Success)
	require.Contains(t, over.Result.Error, "search budget exhausted")
	require.Len(t, probe.calls, maxSearchesPerQuestion)
	require.Contains(t, e.retrievalStatePrompt(), "search budget for this question is spent")
}

func TestRuntimeRetrievalValidatesReadDocumentObservedID(t *testing.T) {
	e, probe := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("q1", "check docs")
	e.beginTurn("check docs", nil, &types.AgentState{})

	unseen := retrievalProbeCall(e, "r1", agenttools.ToolReadDocument, `{"id":"invented-doc"}`)
	require.False(t, unseen.Result.Success)
	require.Contains(t, unseen.Result.Error, "requires a document reference")

	probe.result = &types.ToolResult{
		Success: true,
		Output:  "found doc-42",
		Data: map[string]any{
			"display_type": "search_results",
			"results":      []map[string]any{{"knowledge_id": "doc-42", "chunk_id": "chunk-1"}},
		},
	}
	searchCall := retrievalProbeCall(e, "s1", agenttools.ToolSearchKnowledge, `{"query":"find doc"}`)
	require.True(t, searchCall.Result.Success)

	seen := retrievalProbeCall(e, "r2", agenttools.ToolReadDocument, `{"id":"doc-42"}`)
	require.True(t, seen.Result.Success, seen.Result.Error)
}

func TestRuntimeRetrievalScopedPartialFailureRecoveryOnce(t *testing.T) {
	e, probe := newPartialRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("q1", "sandbox")
	e.beginTurn("sandbox", nil, &types.AgentState{})

	probe.result = partialRetrievalResult()
	first := retrievalProbeCall(e, "s1", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
	require.True(t, first.Result.Success)

	// Retry only the failed KB: succeeds and marks action=recovery.
	probe.result = &types.ToolResult{Success: true, Output: "recovered", Data: map[string]any{"display_type": "search_results"}}
	rec := retrievalProbeCall(e, "s2", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["retryable"]}`)
	require.True(t, rec.Result.Success, rec.Result.Error)
	require.Equal(t, retrievalRecovery, e.retrieval.snapshot().Attempts[1].Action)

	// Repeating the succeeded recovery search is blocked as a duplicate.
	dupRec := retrievalProbeCall(e, "s3", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["retryable"]}`)
	require.False(t, dupRec.Result.Success)
	require.Contains(t, dupRec.Result.Error, "exact search already completed")

	// When a recovery itself fails transiently, a second recovery is also blocked.
	e2, probe2 := newPartialRetrievalProbeEngine(t)
	e2.EnableRuntimeRetrieval()
	e2.SetRetrievalUserMessage("q2", "sandbox2")
	e2.beginTurn("sandbox2", nil, &types.AgentState{})
	probe2.result = partialRetrievalResult()
	require.True(t, retrievalProbeCall(e2, "a1", agenttools.ToolSearchKnowledge, `{"query":"sandbox2"}`).Result.Success)
	probe2.err = context.DeadlineExceeded
	probe2.result = &types.ToolResult{Success: false, Error: "timeout"}
	recFail := retrievalProbeCall(e2, "a2", agenttools.ToolSearchKnowledge, `{"query":"sandbox2","knowledge_base_ids":["retryable"]}`)
	require.False(t, recFail.Result.Success)
	recAgain := retrievalProbeCall(e2, "a3", agenttools.ToolSearchKnowledge, `{"query":"sandbox2","knowledge_base_ids":["retryable"]}`)
	require.False(t, recAgain.Result.Success)
	require.Contains(t, recAgain.Result.Error, "recovery already used")
}

func TestRuntimeRetrievalSnapshotVersioningAndValidation(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.beginTurn("q", nil, &types.AgentState{})
	scope := e.retrieval.snapshot().Scope

	// v1 and v2 snapshots are discarded cleanly without failing resume.
	for _, legacyVersion := range []int{1, 2} {
		restored, err := e.restoreSavedRetrieval(t.Context(), retrievalState{
			Version: legacyVersion,
			Scope:   scope,
			Scopes:  []string{scope},
		})
		require.NoError(t, err)
		require.Nil(t, restored, "legacy v%d snapshot should be discarded cleanly", legacyVersion)
	}

	// Valid v3 and v4 snapshots restore losslessly, preserving structured sources.
	for _, supportedVersion := range []int{3, 4} {
		saved := retrievalState{
			Version:      supportedVersion,
			Goal:         "check docs",
			Scope:        scope,
			Scopes:       []string{scope},
			UserInputIDs: []string{"message:u1"},
			Attempts: []retrievalAttempt{{
				QuestionID:        "message:u1",
				ID:                "call-1",
				Scope:             scope,
				Action:            retrievalInitial,
				Tool:              agenttools.ToolSearchKnowledge,
				Arguments:         `{"query":"docs"}`,
				Completed:         true,
				Outcome:           retrievalSuccess,
				StructuredSources: true,
				ObservedIDs:       []string{"doc-1"},
				CitableIDs:        []string{"doc-1"},
			}},
		}
		restored, err := e.restoreSavedRetrieval(t.Context(), saved)
		require.NoError(t, err)
		require.NotNil(t, restored, "v%d snapshot must restore losslessly", supportedVersion)
		snap := restored.snapshot()
		require.Equal(t, retrievalStateVersion, snap.Version)
		require.Len(t, snap.Attempts, 1)
		require.Equal(t, []string{"doc-1"}, snap.Attempts[0].ObservedIDs)
		require.Equal(t, []string{"doc-1"}, snap.Attempts[0].CitableIDs)
	}

	// Unsupported versions return an error.
	for _, badVersion := range []int{0, 5, 99} {
		_, err := restoreRetrievalControl(retrievalState{Version: badVersion, Scope: scope, Scopes: []string{scope}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported retrieval snapshot version")
	}

	// Structural invariants: duplicate attempt ID, out-of-scope attempt, unknown question, invalid recovery.
	_, err := restoreRetrievalControl(retrievalState{
		Version: 4, Scope: scope, Scopes: []string{scope},
		Attempts: []retrievalAttempt{
			{ID: "dup", Scope: scope, Action: retrievalInitial, Tool: agenttools.ToolSearchKnowledge},
			{ID: "dup", Scope: scope, Action: retrievalSearch, Tool: agenttools.ToolSearchKnowledge},
		},
	})
	require.Error(t, err)

	_, err = restoreRetrievalControl(retrievalState{
		Version: 4, Scope: scope, Scopes: []string{scope},
		Attempts: []retrievalAttempt{
			{ID: "a1", Scope: retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("other-kb")}), Action: retrievalInitial, Tool: agenttools.ToolSearchKnowledge},
		},
	})
	require.Error(t, err)

	_, err = restoreRetrievalControl(retrievalState{
		Version: 4, Scope: scope, Scopes: []string{scope},
		Attempts: []retrievalAttempt{
			{ID: "a1", Scope: scope, Action: retrievalRecovery, RecoveryCallID: "missing", Tool: agenttools.ToolSearchKnowledge},
		},
	})
	require.Error(t, err)

	// Completed/Outcome pairing validation and unfinished attempt reconciliation.
	_, err = restoreRetrievalControl(retrievalState{
		Version: 4, Scope: scope, Scopes: []string{scope},
		Attempts: []retrievalAttempt{
			{ID: "a1", Scope: scope, Action: retrievalInitial, Tool: agenttools.ToolSearchKnowledge, Completed: true, Outcome: ""},
		},
	})
	require.Error(t, err)

	_, err = restoreRetrievalControl(retrievalState{
		Version: 4, Scope: scope, Scopes: []string{scope},
		Attempts: []retrievalAttempt{
			{ID: "a1", Scope: scope, Action: retrievalInitial, Tool: agenttools.ToolSearchKnowledge, Completed: false, Outcome: retrievalSuccess},
		},
	})
	require.Error(t, err)

	reconciled, err := restoreRetrievalControl(retrievalState{
		Version: 4, Scope: scope, Scopes: []string{scope},
		Attempts: []retrievalAttempt{
			{ID: "a1", Scope: scope, Action: retrievalInitial, Tool: agenttools.ToolSearchKnowledge, Completed: false},
		},
	})
	require.NoError(t, err)
	require.True(t, reconciled.snapshot().Attempts[0].Completed)
	require.Equal(t, retrievalUnknown, reconciled.snapshot().Attempts[0].Outcome)
}

func TestPartialRecoveryOnlyRecordsTransientOverlappingStagesAndRoundTrips(t *testing.T) {
	e, probe := newRuntimeRetrievalProbeEngine(t)
	e.config.SearchTargets = types.SearchTargets{
		retrievalScopeTarget("kb-a"),
		retrievalScopeTarget("kb-b"),
		retrievalScopeTarget("kb-c"),
	}
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("u1", "mixed partial failure")
	e.beginTurn("mixed partial failure", nil, &types.AgentState{})

	// KB A has a non-transient embedding failure, KB B has a transient search timeout, KB C succeeds.
	probe.result = &types.ToolResult{
		Success: true,
		Output:  "results from kb-c",
		Data:    map[string]any{"display_type": "search_results", "results": []map[string]any{}},
		RetrievalFailures: []types.RetrievalFailure{
			{
				KnowledgeBaseIDs: []string{"kb-a"},
				Targets:          types.SearchTargets{retrievalScopeTarget("kb-a")},
				Stage:            "embedding",
				Cause:            errors.New("bad api key"),
			},
			{
				KnowledgeBaseIDs: []string{"kb-b"},
				Targets:          types.SearchTargets{retrievalScopeTarget("kb-b")},
				Stage:            "search",
				Cause:            context.DeadlineExceeded,
			},
		},
	}
	first := retrievalProbeCall(e, "s1", agenttools.ToolSearchKnowledge, `{"query":"mixed"}`)
	require.True(t, first.Result.Success)

	// Retrying only KB B must record only ["search"] (never KB A's non-transient "embedding").
	probe.result = &types.ToolResult{Success: true, Output: "recovered kb-b", Data: map[string]any{"display_type": "search_results"}}
	rec := retrievalProbeCall(e, "s2", agenttools.ToolSearchKnowledge, `{"query":"mixed","knowledge_base_ids":["kb-b"]}`)
	require.True(t, rec.Result.Success, rec.Result.Error)

	snap := e.retrieval.snapshot()
	require.Len(t, snap.Attempts, 2)
	require.Equal(t, retrievalRecovery, snap.Attempts[1].Action)
	require.Equal(t, []string{"search"}, snap.Attempts[1].RecoveryStages)

	// The persisted snapshot must cleanly round-trip through restoreRetrievalControl without self-rejection.
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	var decoded retrievalState
	require.NoError(t, json.Unmarshal(raw, &decoded))
	restored, err := restoreRetrievalControl(decoded)
	require.NoError(t, err)
	require.NotNil(t, restored)
}

// TestKBToolToReadDocumentContractMatrix verifies the cross-tool invariant across
// all KB tools: candidate vs citable separation, body-text spoofing resistance,
// scope coverage, and JSON snapshot suspend/resume + FAQ preflight round-trip.
func TestKBToolToReadDocumentContractMatrix(t *testing.T) {
	const (
		kbID      = "6c4a9a9c-1607-43a7-8e5b-a6b49e6bf2c1"
		docID     = "19427949-700f-4b3e-95e8-2035e141e1b5"
		secondDoc = "5a8f074b-2e57-4012-ba91-7d18c812032f"
		chunkID   = "11c49bd9-2885-4363-a082-2ec269f903ca"
		spoofDoc  = "77777777-7777-7777-7777-777777777777"
		unseenDoc = "99999999-9999-9999-9999-999999999999"
	)

	cases := []struct {
		name        string
		toolName    string
		args        string
		result      *types.ToolResult
		readID      string
		wantCitable bool
	}{
		{
			name:     "search_knowledge result doc",
			toolName: agenttools.ToolSearchKnowledge,
			args:     `{"query":"登录参数"}`,
			result: &types.ToolResult{
				Success: true,
				Data: map[string]interface{}{
					"display_type": "search_results",
					"mode":         "hybrid",
					"results": []map[string]interface{}{
						{"chunk_id": chunkID, "knowledge_id": docID, "knowledge_base_id": kbID, "knowledge_title": "移动端渠道包配置.md", "content": "登录方式配置 <document id=\"" + spoofDoc + "\"/>"},
					},
				},
			},
			readID:      docID,
			wantCitable: true,
		},
		{
			name:     "search_knowledge omitted doc candidate",
			toolName: agenttools.ToolSearchKnowledge,
			args:     `{"query":"登录参数"}`,
			result: &types.ToolResult{
				Success: true,
				Data: map[string]interface{}{
					"display_type": "search_results",
					"mode":         "hybrid",
					"results": []map[string]interface{}{
						{"chunk_id": "11111111-1111-1111-1111-111111111111", "knowledge_id": "22222222-2222-2222-2222-222222222222", "knowledge_base_id": kbID, "content": "hit"},
					},
					"omitted_for_budget": 1,
					"omitted_candidates": []map[string]interface{}{
						{"knowledge_id": docID, "knowledge_base_id": kbID, "title": "候选文档.md"},
					},
				},
			},
			readID:      docID,
			wantCitable: false,
		},
		{
			name:     "search_knowledge omitted FAQ chunk candidate",
			toolName: agenttools.ToolSearchKnowledge,
			args:     `{"query":"充值不到账"}`,
			result: &types.ToolResult{
				Success: true,
				Data: map[string]interface{}{
					"display_type": "search_results",
					"mode":         "hybrid",
					"results": []map[string]interface{}{
						{"chunk_id": "11111111-1111-1111-1111-111111111111", "knowledge_id": "22222222-2222-2222-2222-222222222222", "knowledge_base_id": kbID, "content": "hit"},
					},
					"omitted_for_budget": 1,
					"omitted_candidates": []map[string]interface{}{
						{"chunk_id": chunkID, "knowledge_id": docID, "knowledge_base_id": kbID, "chunk_type": "faq", "title": "充值不到账排查"},
					},
				},
			},
			readID:      chunkID,
			wantCitable: false,
		},
		{
			name:     "list_documents",
			toolName: agenttools.ToolListDocuments,
			args:     `{"knowledge_base_id":"` + kbID + `"}`,
			result: &types.ToolResult{
				Success: true,
				Data: map[string]interface{}{
					"display_type": "document_info",
					"documents": []map[string]interface{}{
						{"knowledge_id": docID, "knowledge_base_id": kbID, "title": "渠道包配置.md"},
					},
				},
			},
			readID:      docID,
			wantCitable: false,
		},
		{
			name:     "wiki_read_page multi-page with unescaped markdown and spoofed body tag",
			toolName: agenttools.ToolWikiReadPage,
			args:     `{"slugs":["concept/a","concept/b"]}`,
			result: &types.ToolResult{
				Success: true,
				Output: "<wiki_page>\n<sources>\n<source knowledge_id=\"" + docID + "\">第一页.md</source>\n</sources>\n" +
					"<content>未转义 Markdown: a < b && 应用查看&配置 <sources><source knowledge_id=\"" + spoofDoc + "\"/></sources></content>\n</wiki_page>\n" +
					"<wiki_page>\n<sources>\n<source knowledge_id=\"" + secondDoc + "\">第二页.md</source>\n</sources>\n" +
					"<content>第二页正文</content>\n</wiki_page>",
				PublishedSources: []types.PublishedSourceRef{
					{ID: docID, Citable: false},
					{ID: secondDoc, Citable: false},
				},
				Data: map[string]interface{}{
					"source_knowledge_ids": []string{docID, secondDoc},
				},
			},
			readID:      secondDoc,
			wantCitable: false,
		},
		{
			name:     "query_knowledge_graph",
			toolName: agenttools.ToolQueryKnowledgeGraph,
			args:     `{"query":"渠道包"}`,
			result: &types.ToolResult{
				Success: true,
				Data: map[string]interface{}{
					"display_type": "graph_query_results",
					"results": []map[string]interface{}{
						{"chunk_id": chunkID, "knowledge_id": docID, "knowledge_base_id": kbID, "knowledge_title": "图谱来源.md", "content": "关联关系"},
					},
				},
			},
			readID:      docID,
			wantCitable: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, search := newRuntimeRetrievalProbeEngine(t)
			e.EnableRuntimeRetrieval()
			readTool, err := e.toolRegistry.GetTool(agenttools.ToolReadDocument)
			require.NoError(t, err)
			reader := readTool.(*retrievalProbe)
			if tc.toolName == agenttools.ToolSearchKnowledge {
				search.result = tc.result
			} else {
				producer := &retrievalProbe{
					BaseTool: agenttools.NewBaseTool(tc.toolName, "producer", json.RawMessage(`{"type":"object"}`)),
					result:   tc.result,
				}
				e.toolRegistry.RegisterTool(producer)
			}

			first := retrievalProbeCall(e, "produce-1", tc.toolName, tc.args)
			require.True(t, first.Result.Success, first.Result.Error)

			snap := e.retrieval.snapshot()
			require.NotEmpty(t, snap.Attempts)
			attempt := snap.Attempts[len(snap.Attempts)-1]
			require.True(t, attempt.StructuredSources)
			require.Contains(t, attempt.ObservedIDs, tc.readID)
			require.NotContains(t, attempt.ObservedIDs, spoofDoc)
			if tc.wantCitable {
				require.Contains(t, attempt.CitableIDs, tc.readID)
			} else {
				require.NotContains(t, attempt.CitableIDs, tc.readID, "navigable-only candidates must stay out of CitableIDs")
			}

			// Verify suspend/resume JSON round-trip preserves structured sources without text reparsing.
			rawSnap, err := json.Marshal(snap)
			require.NoError(t, err)
			var decoded retrievalState
			require.NoError(t, json.Unmarshal(rawSnap, &decoded))
			restored, err := restoreRetrievalControl(decoded)
			require.NoError(t, err)
			e.retrieval = restored

			// FAQ preflight candidates and rejections survive the run snapshot too.
			rejections := map[string]types.FAQPreflightRejection{tc.readID: {Question: "faq question", Reason: "other scope"}}
			e.ApplyFAQPreflight(nil, []string{"faq question"}, rejections)
			rawRun, err := json.Marshal(RunSnapshot{Version: RunSnapshotVersion, FAQPreflight: e.faqPreflightSnapshot()})
			require.NoError(t, err)
			decodedRun, err := DecodeRunSnapshot(rawRun)
			require.NoError(t, err)
			e.config.FAQPreflightQuestions, e.config.FAQPreflightRejections = nil, nil
			e.restoreFAQPreflight(decodedRun.FAQPreflight)
			require.Equal(t, []string{"faq question"}, e.config.FAQPreflightQuestions)
			require.Equal(t, rejections, e.config.FAQPreflightRejections)

			readCall := retrievalProbeCall(e, "read-1", agenttools.ToolReadDocument, `{"id":"`+tc.readID+`"}`)
			require.True(t, readCall.Result.Success, readCall.Result.Error)
			require.Len(t, reader.calls, 1)

			spoofed := retrievalProbeCall(e, "read-spoof", agenttools.ToolReadDocument, `{"id":"`+spoofDoc+`"}`)
			require.False(t, spoofed.Result.Success)

			blocked := retrievalProbeCall(e, "read-unseen", agenttools.ToolReadDocument, `{"id":"`+unseenDoc+`"}`)
			require.False(t, blocked.Result.Success)
			require.Contains(t, blocked.Result.Error, "read_document requires a document reference")

			// Verify scope narrowing/change revokes admission for out-of-scope observations.
			e.retrieval.mu.Lock()
			e.retrieval.state.Scope = `["kb:different-scope"]`
			e.retrieval.mu.Unlock()
			require.False(t, e.runtimeDocumentObserved(e.retrieval.snapshot(), tc.readID))
		})
	}
}

func TestExecuteContinuationRestoresPreviousRetrievalStateForRetainHistory(t *testing.T) {
	scope := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("kb-1")})
	priorRef := retrievalCallReference("msg-1", "call-1")
	prevState := retrievalState{
		Version:      retrievalStateVersion,
		Goal:         "first question",
		Scope:        scope,
		Scopes:       []string{scope},
		UserInputIDs: []string{"message:u1"},
		Attempts: []retrievalAttempt{{
			QuestionID:        "message:u1",
			ID:                priorRef,
			Scope:             scope,
			Action:            retrievalInitial,
			Tool:              agenttools.ToolSearchKnowledge,
			Arguments:         `{"query":"login"}`,
			Completed:         true,
			Outcome:           retrievalSuccess,
			StructuredSources: true,
			ObservedIDs:       []string{"doc-prev"},
			CitableIDs:        []string{"doc-prev"},
		}},
		AwaitingUserInput: true,
	}
	prevRaw, err := json.Marshal(prevState)
	require.NoError(t, err)

	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "follow-up answer", Done: true, FinishReason: "stop"},
	}}}}
	e := newTestEngine(t, model)
	e.config.SearchTargets = types.SearchTargets{retrievalScopeTarget("kb-1")}
	e.config.RetainRetrievalHistory = true
	e.toolRegistry = agenttools.NewToolRegistry()
	e.toolRegistry.RegisterTool(&retrievalProbe{
		BaseTool: agenttools.NewBaseTool(agenttools.ToolSearchKnowledge, "search", json.RawMessage(`{"type":"object"}`)),
	})
	e.toolRegistry.RegisterTool(&retrievalProbe{
		BaseTool: agenttools.NewBaseTool(agenttools.ToolReadDocument, "read", json.RawMessage(`{"type":"object"}`)),
	})
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("u2", "follow-up question")
	var savedRaw json.RawMessage
	e.SetRetrievalPersistence(prevRaw, func(_ context.Context, raw json.RawMessage) error {
		savedRaw = append(json.RawMessage(nil), raw...)
		return nil
	})

	history := []chat.Message{
		{Role: "user", Content: "first question"},
		{
			Role: "assistant",
			ToolCalls: []chat.ToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: chat.FunctionCall{
					Name:      agenttools.ToolSearchKnowledge,
					Arguments: `{"query":"login"}`,
				},
			}},
		},
		{
			Role:       "tool",
			ToolCallID: "call-1",
			TurnID:     "msg-1",
			Name:       agenttools.ToolSearchKnowledge,
			Content:    "preserved-turn1-kb-evidence",
		},
		{Role: "assistant", Content: "first answer"},
	}

	state, err := e.Execute(t.Context(), "sess-1", "msg-2", "follow-up question", history, nil)
	require.NoError(t, err)
	require.Equal(t, "follow-up answer", state.FinalAnswer)
	require.NotEmpty(t, model.calls)

	// Prior in-scope KB tool result must be replayed unredacted.
	foundUnredacted := false
	for _, msg := range model.calls[0] {
		if msg.Role == "tool" && strings.Contains(msg.Content, "preserved-turn1-kb-evidence") {
			foundUnredacted = true
			require.NotContains(t, msg.Content, "omitted")
		}
	}
	require.True(t, foundUnredacted, "in-scope Turn 1 KB evidence must stay unredacted on Turn 2 continuation")

	// Observed doc from Turn 1 remains readable on Turn 2, and AwaitingUserInput was cleared.
	require.True(t, e.runtimeDocumentObserved(e.retrieval.snapshot(), "doc-prev"))
	require.False(t, e.retrieval.isAwaitingUserInput())
	require.Nil(t, RetrievalMissingUserFields(savedRaw))
}

func TestPersistenceFailureIsStickyAndBlocksAskUser(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("u1", "need clarification")
	e.beginTurn("need clarification", nil, &types.AgentState{})

	failOnce := true
	e.retrieval.mu.Lock()
	e.retrieval.persist = func(_ retrievalState) error {
		if failOnce {
			failOnce = false
			return errors.New("db write timeout")
		}
		return nil
	}
	e.retrieval.mu.Unlock()

	// Trigger a persistence failure during tool preparation.
	call := retrievalProbeCall(e, "s1", agenttools.ToolSearchKnowledge, `{"query":"x"}`)
	require.False(t, call.Result.Success)
	require.True(t, e.retrieval.hasPersistenceFailed())

	// Subsequent saves must stay blocked even though the underlying callback would now return nil.
	e.retrieval.mu.Lock()
	err := e.retrieval.saveLocked()
	e.retrieval.mu.Unlock()
	require.ErrorIs(t, err, errRetrievalStateSave)
	require.True(t, e.retrieval.hasPersistenceFailed())

	// Both publicAnswer and finishUserQuestion must fail with errRetrievalStateSave.
	_, err = e.publicAnswer(t.Context(), "draft answer", nil)
	require.ErrorIs(t, err, errRetrievalStateSave)

	state := &types.AgentState{}
	msgs := []chat.Message{{Role: "user", Content: "need clarification"}}
	step := types.AgentStep{
		ToolCalls: []types.ToolCall{{
			ID:     "ask-1",
			Name:   agenttools.ToolAskUser,
			Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请问是手游还是端游？"}),
		}},
	}
	done, err := e.finishUserQuestion(t.Context(), state, step, &msgs, "sess", "msg")
	require.False(t, done)
	require.ErrorIs(t, err, errRetrievalStateSave)
	require.False(t, state.IsComplete)
}

func TestSteerNewQuestionDoesNotReusePriorQuestionFailure(t *testing.T) {
	e, probe := newPartialRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("u1", "question 1")
	e.beginTurn("question 1", nil, &types.AgentState{})

	// Question 1: search hits partial transient failure and uses its one-shot recovery.
	probe.result = partialRetrievalResult()
	require.True(t, retrievalProbeCall(e, "q1-s1", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	probe.err = context.DeadlineExceeded
	probe.result = &types.ToolResult{Success: false, Error: "timeout"}
	require.False(t, retrievalProbeCall(e, "q1-s2", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["retryable"]}`).Result.Success)

	// Mid-run steer injects Question 2 (new UserInputID).
	e.retrieval.recordUserInput("steer:1", "question 2")
	probe.err = nil
	probe.result = &types.ToolResult{Success: true, Output: "fresh q2 result", Data: map[string]any{"display_type": "search_results"}}

	// Same search args on Question 2 must execute as a normal search, NOT fail with "recovery already used".
	q2Call := retrievalProbeCall(e, "q2-s1", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["retryable"]}`)
	require.True(t, q2Call.Result.Success, q2Call.Result.Error)
	snap := e.retrieval.snapshot()
	last := snap.Attempts[len(snap.Attempts)-1]
	require.Equal(t, "steer:1", last.QuestionID)
	require.Equal(t, retrievalSearch, last.Action)
	require.Empty(t, last.RecoveryCallID)
}

func TestFinalSynthesisStreamsLiveEvenWhenHoldCollaborationAnswerSet(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "第一部分"},
		{ResponseType: types.ResponseTypeAnswer, Content: "第二部分"},
		{ResponseType: types.ResponseTypeAnswer, Content: "收尾", Done: true, FinishReason: "stop"},
	}}}}
	e := newTestEngine(t, model)
	e.holdCollaborationAnswer = true

	var liveChunks []string
	var doneContent string
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		if data.Done {
			doneContent += data.Content
		} else if data.Content != "" {
			liveChunks = append(liveChunks, data.Content)
		}
		return nil
	})

	state := &types.AgentState{}
	err := e.streamFinalAnswerToEventBus(
		t.Context(),
		"synthesize",
		state,
		"sess-collab",
		[]chat.Message{{Role: "user", Content: "synthesize"}},
	)
	require.NoError(t, err)
	require.Equal(t, "第一部分第二部分收尾", state.FinalAnswer)
	require.Equal(t, []string{"第一部分", "第二部分"}, liveChunks, "final synthesis must stream live even when holdCollaborationAnswer is true")
	require.Equal(t, "收尾", doneContent)
	require.Equal(t, state.FinalAnswer, strings.Join(liveChunks, "")+doneContent)
}

func TestInstanceHooksApplyToRuntimeContextAndFixedResponse(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.SetHooks(NoopEngineHooks{})
	e.appInfo = `{"gameInfo":{"device_type":"手游、端游"}}`

	rendered := e.RenderUserTurnContent("sess-1", "hi")
	require.NotContains(t, rendered, `multi_device_type="true"`, "NoopEngineHooks should suppress GameSDK multi_device_type annotations")

	e.config.FixedResponses = map[string]string{
		"NO_ANSWER": "当前无答案",
	}
	// Under NoopEngineHooks, trailing <button-send> is NOT stripped before sentinel matching.
	rawWithButton := "[[FIXED:NO_ANSWER]]\n<button-send>保留</button-send>"
	out, err := e.publicAnswer(t.Context(), rawWithButton, nil)
	require.NoError(t, err)
	require.Equal(t, rawWithButton, out, "NoopEngineHooks should not strip buttons during fixed response sentinel matching")
	// Pure sentinel without buttons still expands normally under NoopEngineHooks.
	outPlain, err := e.publicAnswer(t.Context(), "[[FIXED:NO_ANSWER]]", nil)
	require.NoError(t, err)
	require.Equal(t, "当前无答案", outPlain)

	// With default GameSDKHooks, multi_device_type is annotated and buttons are stripped before sentinel expansion.
	e.SetHooks(NewGameSDKHooks(DefaultGameSDKHooksConfig()))
	renderedDefault := e.RenderUserTurnContent("sess-1", "hi")
	require.Contains(t, renderedDefault, `multi_device_type="true"`)
	outDefault, err := e.publicAnswer(t.Context(), "[[FIXED:NO_ANSWER]]\n<button-send>移除</button-send>", nil)
	require.NoError(t, err)
	require.Equal(t, "当前无答案", outDefault)
}

func TestGameSDKExpandLibrariesStrictGameInfoDeviceTypeOnly(t *testing.T) {
	setup := func(appInfo string) (*AgentEngine, map[string]any) {
		e, _ := newRuntimeRetrievalProbeEngine(t)
		e.config.SearchTargets = types.SearchTargets{
			retrievalScopeTarget("faq"),
			retrievalScopeTarget("client"),
			retrievalScopeTarget("pc"),
		}
		e.knowledgeBasesInfo = []*KnowledgeBaseInfo{
			{ID: "faq", Name: "docs-general", Type: types.KnowledgeBaseTypeDocument},
			{ID: "client", Name: "docs-client", Type: types.KnowledgeBaseTypeDocument},
			{ID: "pc", Name: "docs-pc", Type: types.KnowledgeBaseTypeDocument},
		}
		e.appInfo = appInfo
		e.retrieval = newRetrievalControl("q", retrievalScopeForTargets(e.config.SearchTargets))
		e.retrieval.state.UserInputIDs = []string{"q"}
		e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "q", Tool: "search_knowledge", Completed: true, Outcome: retrievalSuccess}}
		args := map[string]any{"knowledge_base_ids": []any{"faq"}}
		return e, args
	}

	// Top-level device_type must NOT expand KBs.
	e1, args1 := setup(`{"device_type":"手游、端游"}`)
	e1.extHooks().ExpandSearchKnowledgeBases(e1, args1)
	require.Equal(t, []any{"faq"}, args1["knowledge_base_ids"])

	// Slash/pipe delimiters must NOT split for KB expansion.
	e2, args2 := setup(`{"gameInfo":{"device_type":"手游/端游"}}`)
	e2.extHooks().ExpandSearchKnowledgeBases(e2, args2)
	require.Equal(t, []any{"faq"}, args2["knowledge_base_ids"])

	// Exact gameInfo.device_type with original delimiters DOES expand KBs.
	e3, args3 := setup(`{"gameInfo":{"device_type":"手游、端游"}}`)
	e3.extHooks().ExpandSearchKnowledgeBases(e3, args3)
	require.Equal(t, []any{"faq", "client", "pc"}, args3["knowledge_base_ids"])
}

func TestAskUserMarksAwaitingUserInputForSuggestionsAndButtonSuppression(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("u1", "配置登录")
	e.beginTurn("配置登录", nil, &types.AgentState{})

	var persisted json.RawMessage
	e.SetRetrievalPersistence(nil, func(_ context.Context, raw json.RawMessage) error {
		persisted = append(json.RawMessage(nil), raw...)
		return nil
	})
	require.NoError(t, e.bindRetrievalPersistence(t.Context()))
	require.Nil(t, RetrievalMissingUserFields(persisted))

	state := &types.AgentState{}
	msgs := []chat.Message{{Role: "user", Content: "配置登录"}}
	step := types.AgentStep{
		ToolCalls: []types.ToolCall{{
			ID:     "ask-1",
			Name:   agenttools.ToolAskUser,
			Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请问您接入的是手游还是端游？"}),
		}},
	}
	done, err := e.finishUserQuestion(t.Context(), state, step, &msgs, "sess", "msg")
	require.NoError(t, err)
	require.True(t, done)

	// Persisted state marks awaiting_user_input so message_suggestion suppresses follow-up chips.
	require.True(t, e.hasMissingUserFields())
	require.NotEmpty(t, RetrievalMissingUserFields(persisted))

	// Button sanitizer also strips <button-send> while awaiting user input.
	out, err := e.publicAnswer(t.Context(), "请先确认平台。\n<button-send>如何接入</button-send>", nil)
	require.NoError(t, err)
	require.Equal(t, "请先确认平台。", out)
}

func TestAskUserRollsBackAwaitingUserInputOnEmitFailure(t *testing.T) {
	e, _ := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("u1", "配置登录")
	e.beginTurn("配置登录", nil, &types.AgentState{})

	var persisted json.RawMessage
	e.SetRetrievalPersistence(nil, func(_ context.Context, raw json.RawMessage) error {
		persisted = append(json.RawMessage(nil), raw...)
		return nil
	})
	require.NoError(t, e.bindRetrievalPersistence(t.Context()))

	emitErr := errors.New("stream emit failed")
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, _ event.Event) error {
		return emitErr
	})

	state := &types.AgentState{}
	msgs := []chat.Message{{Role: "user", Content: "配置登录"}}
	step := types.AgentStep{
		ToolCalls: []types.ToolCall{{
			ID:     "ask-1",
			Name:   agenttools.ToolAskUser,
			Result: agenttools.AskUserQuestion(types.AskUserPromptRequest{Question: "请问是手游还是端游？"}),
		}},
	}
	done, err := e.finishUserQuestion(t.Context(), state, step, &msgs, "sess", "msg")
	require.False(t, done)
	require.ErrorIs(t, err, emitErr)
	require.False(t, e.hasMissingUserFields(), "AwaitingUserInput must roll back when clarification event fails to publish")
	require.Nil(t, RetrievalMissingUserFields(persisted))
}

func TestEngineHooksIsolationAndConfigurability(t *testing.T) {
	t.Run("NoopEngineHooks disables vertical Game SDK rules", func(t *testing.T) {
		e, _ := newRuntimeRetrievalProbeEngine(t)
		e.SetHooks(NoopEngineHooks{})
		e.config.SearchTargets = types.SearchTargets{retrievalScopeTarget("faq"), retrievalScopeTarget("pc")}
		e.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "pc", Name: "docs-pc"}}
		e.appInfo = `{"gameInfo":{"device_type":"端游"}}`
		e.retrieval = newRetrievalControl("q", retrievalScopeForTargets(e.config.SearchTargets))
		e.retrieval.state.UserInputIDs = []string{"q"}
		e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "q", Tool: "search_knowledge", Completed: true, Outcome: retrievalSuccess}}

		args := map[string]any{"knowledge_base_ids": []any{"faq"}}
		e.extHooks().ExpandSearchKnowledgeBases(e, args)
		require.Equal(t, []any{"faq"}, args["knowledge_base_ids"])

		in := "当前知识库没有找到相关解答。\n<button-send>保留按钮</button-send>"
		out, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, in, out)
	})

	t.Run("custom GameSDKHooksConfig drives device-to-KB mapping without hardcoding", func(t *testing.T) {
		e, _ := newRuntimeRetrievalProbeEngine(t)
		e.SetHooks(NewGameSDKHooks(GameSDKHooksConfig{
			DeviceToKBNames: map[string][]string{"console": {"docs-console"}},
		}))
		e.config.SearchTargets = types.SearchTargets{retrievalScopeTarget("faq"), retrievalScopeTarget("c1")}
		e.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "c1", Name: "docs-console"}}
		e.appInfo = `{"gameInfo":{"device_type":"console"}}`
		e.retrieval = newRetrievalControl("q", retrievalScopeForTargets(e.config.SearchTargets))
		e.retrieval.state.UserInputIDs = []string{"q"}
		e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "q", Tool: "search_knowledge", Completed: true, Outcome: retrievalSuccess}}

		args := map[string]any{"knowledge_base_ids": []any{"faq"}}
		e.extHooks().ExpandSearchKnowledgeBases(e, args)
		require.Equal(t, []any{"faq", "c1"}, args["knowledge_base_ids"])
	})

	t.Run("RetrievalMissingUserFields remains backward-compatible with legacy snapshots", func(t *testing.T) {
		legacy := json.RawMessage(`{"version":4,"gaps":[{"status":"needs_user","missing_user_fields":["平台","渠道"]}]}`)
		require.Equal(t, []string{"平台", "渠道"}, RetrievalMissingUserFields(legacy))
		v5 := json.RawMessage(`{"version":5,"attempts":[]}`)
		require.Nil(t, RetrievalMissingUserFields(v5))
	})

	t.Run("non-timeout failure is not retryable as recovery", func(t *testing.T) {
		e, probe := newRuntimeRetrievalProbeEngine(t)
		e.EnableRuntimeRetrieval()
		e.SetRetrievalUserMessage("q1", "perm fail")
		e.beginTurn("perm fail", nil, &types.AgentState{})
		probe.err = errors.New("permission denied")
		first := retrievalProbeCall(e, "s1", agenttools.ToolSearchKnowledge, `{"query":"perm"}`)
		require.False(t, first.Result.Success)
		require.Equal(t, retrievalUnknown, e.retrieval.snapshot().Attempts[0].Outcome)
	})
}
