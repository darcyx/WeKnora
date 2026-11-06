package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type targetRecoverySearch struct {
	interfaces.KnowledgeBaseService
	mu              sync.Mutex
	failed          bool
	embeddingFailed bool
	embeddings      int
	calls           []types.SearchParams
}

func (*targetRecoverySearch) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	return []*types.KnowledgeBase{{ID: "kb", TenantID: 1, IndexingStrategy: types.IndexingStrategy{KeywordEnabled: true, VectorEnabled: true}}}, nil
}
func (*targetRecoverySearch) ResolveEmbeddingModelKeys(context.Context, []*types.KnowledgeBase) map[string]string {
	return map[string]string{"kb": "embedding"}
}
func (s *targetRecoverySearch) GetQueryEmbedding(context.Context, string, string) ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embeddings++
	if s.embeddingFailed {
		return nil, context.DeadlineExceeded
	}
	return []float32{1, 0}, nil
}
func (s *targetRecoverySearch) HybridSearch(_ context.Context, _ string, params types.SearchParams) ([]*types.SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, params)
	if s.failed && len(params.TagIDs) > 0 && params.TagIDs[0] == "bad-tag" {
		return nil, context.DeadlineExceeded
	}
	return []*types.SearchResult{{ID: "chunk-" + params.TagIDs[0], KnowledgeBaseID: "kb", KnowledgeID: "doc", Content: "sandbox facts", Score: 0.9}}, nil
}

func TestRetrievalRecoveryExecutesOnlyFailedTargetsAfterRestore(t *testing.T) {
	for _, documents := range []bool{false, true} {
		t.Run(map[bool]string{false: "tags", true: "documents_and_tags"}[documents], func(t *testing.T) {
			targets := types.SearchTargets{
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1, TagIDs: []string{"good-tag"}},
				{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1, TagIDs: []string{"bad-tag"}},
			}
			if documents {
				for i, doc := range []string{"good-doc", "bad-doc"} {
					targets[i].Type, targets[i].KnowledgeIDs = types.SearchTargetTypeKnowledge, []string{doc}
				}
			}
			svc := &targetRecoverySearch{failed: true}
			e, _ := newRetrievalProbeEngine(t)
			e.config.SearchTargets = targets
			e.toolRegistry = agenttools.NewToolRegistry()
			e.toolRegistry.RegisterTool(agenttools.NewSearchKnowledgeTool(svc, nil, nil, targets, nil, nil))
			e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
			first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge,
				`{"query":"sandbox","mode":"keyword","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","action":"initial"}}`)
			require.True(t, first.Result.Success, first.Result.Error)
			require.Len(t, svc.calls, 2)
			for name, corrupt := range map[string]func(*retrievalState){
				"foreign tenant": func(s *retrievalState) { s.Attempts[0].Failures[0].Targets[0].TenantID = 2 },
				"unknown tag":    func(s *retrievalState) { s.Attempts[0].Failures[0].Targets[0].TagIDs = []string{"other"} },
				"broader target": func(s *retrievalState) { s.Attempts[0].Failures[0].Targets[0].TagIDs = nil },
				"nil target":     func(s *retrievalState) { s.Attempts[0].Failures[0].Targets[0] = nil },
				"missing stage":  func(s *retrievalState) { s.Attempts[0].Failures[0].Stage = "" },
			} {
				t.Run(name, func(t *testing.T) {
					saved := e.retrieval.snapshot()
					corrupt(&saved)
					_, err := restoreRetrievalControl(saved)
					require.Error(t, err)
				})
			}
			encoded, err := json.Marshal(e.retrieval.snapshot())
			require.NoError(t, err)
			var saved retrievalState
			require.NoError(t, json.Unmarshal(encoded, &saved))
			for name, alter := range map[string]func(*retrievalState){
				"legacy scope only": func(s *retrievalState) { s.Attempts[0].Failures[0].Targets = nil; s.Attempts[0].Failures[0].Stage = "" },
				"unsupported stage": func(s *retrievalState) { s.Attempts[0].Failures[0].Stage = "rerank" },
			} {
				t.Run(name, func(t *testing.T) {
					var altered retrievalState
					require.NoError(t, json.Unmarshal(encoded, &altered))
					alter(&altered)
					e.retrieval, err = restoreRetrievalControl(altered)
					require.NoError(t, err, "observations remain readable even without a safe recovery path")
					blocked := retrievalProbeCall(e, "blocked", agenttools.ToolSearchKnowledge,
						`{"query":"sandbox","mode":"keyword","knowledge_base_ids":["kb"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
					require.False(t, blocked.Result.Success)
					require.Len(t, svc.calls, 2)
					require.False(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed)
				})
			}
			e.retrieval, err = restoreRetrievalForScope(saved, retrievalScopeForTargets(types.SearchTargets{targets[0]}))
			require.NoError(t, err)
			blocked := retrievalProbeCall(e, "revoked", agenttools.ToolSearchKnowledge,
				`{"query":"sandbox","mode":"keyword","knowledge_base_ids":["kb"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
			require.False(t, blocked.Result.Success)
			require.Len(t, svc.calls, 2)
			require.False(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed)
			e.retrieval, err = restoreRetrievalForScope(saved, retrievalScopeForTargets(types.SearchTargets{targets[1]}))
			require.NoError(t, err)
			svc.failed = false
			recovered := retrievalProbeCall(e, "retry", agenttools.ToolSearchKnowledge,
				`{"query":"sandbox","mode":"keyword","knowledge_base_ids":["kb"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
			require.True(t, recovered.Result.Success, recovered.Result.Error)
			require.Len(t, svc.calls, 3, "recovery must not query the successful target again")
			require.Equal(t, []string{"bad-tag"}, svc.calls[2].TagIDs)
			require.Equal(t, targets[1].KnowledgeIDs, svc.calls[2].KnowledgeIDs)
			state := e.retrieval.snapshot()
			require.True(t, state.Gaps[0].RecoveryUsed)
			require.False(t, state.Gaps[0].SupplementUsed)
			require.Equal(t, retrievalScopeForTargets(types.SearchTargets{targets[1]}), state.Attempts[1].Scope)
			require.Equal(t, state.Attempts[0].ID, state.Attempts[1].RecoveryCallID)
			require.Equal(t, types.SearchTargets{targets[1]}, state.Attempts[1].RecoveryTargets)
			require.Empty(t, outstandingRetrievalFailures(state.Attempts, "gap-1"))
			for name, corrupt := range map[string]func(*retrievalState){
				"wrong original call": func(s *retrievalState) { s.Attempts[1].RecoveryCallID = s.Attempts[1].ID },
				"expanded recovery":   func(s *retrievalState) { s.Attempts[1].RecoveryTargets = targets.Clone() },
				"changed threshold":   func(s *retrievalState) { s.Attempts[1].RecoveryTargets[0].DisableRecallThresholds = true },
				"lost targets":        func(s *retrievalState) { s.Attempts[1].RecoveryTargets = nil },
				"changed query": func(s *retrievalState) {
					s.Attempts[1].Arguments = `{"query":"other","mode":"keyword","knowledge_base_ids":["kb"]}`
				},
			} {
				t.Run(name, func(t *testing.T) {
					corruptState := e.retrieval.snapshot()
					corrupt(&corruptState)
					_, err := restoreRetrievalControl(corruptState)
					require.Error(t, err)
				})
			}
			e.retrieval, err = restoreRetrievalControl(state)
			require.NoError(t, err)
			third := retrievalProbeCall(e, "again", agenttools.ToolSearchKnowledge,
				`{"query":"sandbox","mode":"keyword","knowledge_base_ids":["kb"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
			require.False(t, third.Result.Success)
			require.Len(t, svc.calls, 3)
		})
	}
}

func TestRetrievalRecoveryPreservesOtherCallsAndStages(t *testing.T) {
	target := &types.SearchTarget{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", KnowledgeIDs: []string{"doc"}, TagIDs: []string{"tag"}}
	failure := func(call, stage string) retrievalFailure {
		return retrievalFailure{CallID: call, KnowledgeBaseIDs: []string{"kb"}, Targets: types.SearchTargets{target}.Clone(), Stage: stage, Outcome: retrievalTransient}
	}
	attempts := []retrievalAttempt{
		{ID: "first", GapID: "gap", Completed: true, Failures: []retrievalFailure{failure("first", "search"), failure("first", "rerank")}},
		{ID: "second", GapID: "gap", Completed: true, Failures: []retrievalFailure{failure("second", "search")}},
		{ID: "recovery", GapID: "gap", Action: retrievalRecovery, Completed: true, Outcome: retrievalSuccess,
			RecoveryCallID: "first", RecoveryTargets: types.SearchTargets{target}.Clone()},
	}
	remaining := outstandingRetrievalFailures(attempts, "gap")
	require.Equal(t, []retrievalFailure{failure("first", "rerank"), failure("second", "search")}, remaining)
	require.Len(t, attempts[0].Failures[0].Targets, 1, "reading outstanding failures does not mutate history")
	remaining[0].Targets[0].KnowledgeIDs[0] = "changed"
	require.Equal(t, []string{"doc"}, attempts[0].Failures[1].Targets[0].KnowledgeIDs)
}

func TestRetrievalRecoveryResumesFailedEmbeddingBeforeSearch(t *testing.T) {
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 1,
		KnowledgeIDs: []string{"doc"}, TagIDs: []string{"tag"}}}
	svc := &targetRecoverySearch{embeddingFailed: true}
	e, _ := newRetrievalProbeEngine(t)
	e.config.SearchTargets = targets
	e.toolRegistry = agenttools.NewToolRegistry()
	e.toolRegistry.RegisterTool(agenttools.NewSearchKnowledgeTool(svc, nil, nil, targets, nil, nil))
	e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","mode":"semantic","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","action":"initial"}}`)
	require.False(t, first.Result.Success)
	require.Equal(t, 1, svc.embeddings)
	require.Empty(t, svc.calls)
	encoded, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	var saved retrievalState
	require.NoError(t, json.Unmarshal(encoded, &saved))
	require.Equal(t, "embedding", saved.Attempts[0].Failures[0].Stage)
	e.retrieval, err = restoreRetrievalControl(saved)
	require.NoError(t, err)
	svc.embeddingFailed = false
	recovered := retrievalProbeCall(e, "retry", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","mode":"semantic","retrieval":{"gap_id":"gap-1","action":"recovery"}}`)
	require.True(t, recovered.Result.Success, recovered.Result.Error)
	require.Equal(t, 2, svc.embeddings)
	require.Len(t, svc.calls, 1, "search begins only after the failed embedding prerequisite is recovered")
	require.Equal(t, []string{"doc"}, svc.calls[0].KnowledgeIDs)
	require.Equal(t, []string{"tag"}, svc.calls[0].TagIDs)
	state := e.retrieval.snapshot()
	require.True(t, state.Gaps[0].RecoveryUsed)
	require.Empty(t, outstandingRetrievalFailures(state.Attempts, "gap-1"))
}

func TestRetrievalRecoverySeparatesErrorsWithinOneKnowledgeBase(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		t.Run(map[bool]string{false: "independent", true: "overlapping"}[overlap], func(t *testing.T) {
			e, probe := newPartialRetrievalProbeEngine(t)
			target := &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "retryable", TagIDs: []string{"retry"}}
			other := types.SearchTargets{target}.Clone()
			if !overlap {
				other[0].TagIDs = []string{"unavailable"}
			}
			probe.result = &types.ToolResult{Success: true, Output: "sandbox facts", RetrievalFailures: []types.RetrievalFailure{
				{KnowledgeBaseIDs: []string{"retryable"}, Targets: types.SearchTargets{target}, Stage: "search", Cause: context.DeadlineExceeded},
				{KnowledgeBaseIDs: []string{"retryable"}, Targets: other, Stage: "search", Cause: errors.New("unavailable")},
			}}
			require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, partialRetrievalInitial).Result.Success)
			probe.result = nil
			probe.execute = func(ctx context.Context, _ json.RawMessage) (*types.ToolResult, error) {
				execution, ok := agenttools.ToolExecFromContext(ctx)
				require.True(t, ok)
				require.Equal(t, types.SearchTargets{target}, execution.RetrievalTargets)
				return &types.ToolResult{Success: true, Output: "recovered facts"}, nil
			}
			result := retrievalProbeCall(e, "recovery", agenttools.ToolSearchKnowledge,
				`{"query":"sandbox","knowledge_base_ids":["retryable"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
			require.Equal(t, !overlap, result.Result.Success, result.Result.Error)
			state := e.retrieval.snapshot()
			require.Equal(t, !overlap, state.Gaps[0].RecoveryUsed)
			if overlap {
				require.Len(t, probe.calls, 1)
			} else {
				require.Len(t, probe.calls, 2)
				remaining := outstandingRetrievalFailures(state.Attempts, "gap-1")
				require.Len(t, remaining, 1)
				require.Equal(t, other, remaining[0].Targets)
				_, err := restoreRetrievalControl(state)
				require.NoError(t, err)
			}
		})
	}
}
