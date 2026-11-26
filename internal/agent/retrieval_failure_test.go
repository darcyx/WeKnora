package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func partialRetrievalResult() *types.ToolResult {
	return &types.ToolResult{Success: true, Output: "The supported account region is US; configuration is still needed.",
		RetrievalFailures: []types.RetrievalFailure{
			{KnowledgeBaseIDs: []string{"retryable"}, Targets: types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "retryable"}}, Stage: "search", Cause: context.DeadlineExceeded},
			{KnowledgeBaseIDs: []string{"unavailable"}, Targets: types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "unavailable"}}, Stage: "search", Cause: errors.New("authentication unavailable")},
		}}
}

func newPartialRetrievalProbeEngine(t *testing.T) (*AgentEngine, *retrievalProbe) {
	t.Helper()
	e, probe := newRetrievalProbeEngine(t)
	for _, id := range []string{"retryable", "unavailable", "healthy"} {
		e.config.SearchTargets = append(e.config.SearchTargets, &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: id})
	}
	e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
	return e, probe
}

const partialRetrievalInitial = `{"query":"sandbox","retrieval":{"goal_quote":"TapTap sandbox configuration","fact":"configuration","action":"initial"}}`

func TestRetrievalPartialEvidenceCanFinishWithoutRecovery(t *testing.T) {
	e, probe := newPartialRetrievalProbeEngine(t)
	probe.result = partialRetrievalResult()
	require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, partialRetrievalInitial).Result.Success)
	review := retrievalProbeCall(e, "review", reviewRetrievalTool,
		`{"gap_id":"gap-1","call_id":"first","status":"satisfied","quotes":["supported account region is US"]}`)
	require.True(t, review.Result.Success, review.Result.Error)
	blocked := retrievalProbeCall(e, "unneeded", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","knowledge_base_ids":["retryable"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
	require.False(t, blocked.Result.Success)
	require.Len(t, probe.calls, 1)
	require.False(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed)
}

func TestRetrievalPartialRecoveryRestrictsScopeAndKeepsAllowance(t *testing.T) {
	e, probe := newPartialRetrievalProbeEngine(t)
	probe.result = partialRetrievalResult()
	require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, partialRetrievalInitial).Result.Success)
	for i, args := range []string{
		`{"query":"sandbox"}`,
		`{"query":"renamed","knowledge_base_ids":["retryable"]}`,
		`{"query":"sandbox","knowledge_base_ids":["healthy"]}`,
		`{"query":"sandbox","knowledge_base_ids":["unavailable"]}`,
		`{"query":"sandbox","knowledge_base_ids":["retryable","unavailable"]}`,
	} {
		var input map[string]any
		require.NoError(t, json.Unmarshal([]byte(args), &input))
		input["retrieval"] = map[string]any{"gap_id": "gap-1", "action": "recovery", "lead": "sandbox"}
		encoded, _ := json.Marshal(input)
		result := retrievalProbeCall(e, fmt.Sprintf("invalid-%d", i), agenttools.ToolSearchKnowledge, string(encoded))
		require.False(t, result.Result.Success, args)
	}
	require.Len(t, probe.calls, 1)
	require.False(t, e.retrieval.snapshot().Gaps[0].RecoveryUsed, "blocked calls do not consume the allowance")
	probe.result = nil
	result := retrievalProbeCall(e, "recovery", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","knowledge_base_ids":["retryable"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
	require.True(t, result.Result.Success, result.Result.Error)
	require.Len(t, probe.calls, 2)
	require.JSONEq(t, `{"query":"sandbox","knowledge_base_ids":["retryable"]}`, string(probe.calls[1]))
	state := e.retrieval.snapshot()
	require.True(t, state.Gaps[0].RecoveryUsed)
	require.False(t, state.Gaps[0].SupplementUsed)
	failures := outstandingRetrievalFailures(state.Attempts, "gap-1")
	require.Len(t, failures, 1)
	require.Equal(t, []string{"unavailable"}, failures[0].KnowledgeBaseIDs)
	require.Contains(t, state.Attempts[0].Content, "supported account region is US")
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	var restored retrievalState
	require.NoError(t, json.Unmarshal(encoded, &restored))
	control, err := restoreRetrievalControl(restored)
	require.NoError(t, err)
	require.Error(t, control.acquire("gap-1", "again", retrievalRecovery, "sandbox", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","knowledge_base_ids":["retryable"]}`, "first"))
}

func TestRetrievalIndependentSupplementDoesNotEraseFailedSource(t *testing.T) {
	e, probe := newPartialRetrievalProbeEngine(t)
	probe.result = partialRetrievalResult()
	require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, partialRetrievalInitial).Result.Success)
	require.NoError(t, e.retrieval.review("gap-1", "first", retrievalNoProgress, nil))
	blocked := retrievalProbeCall(e, "disguised", agenttools.ToolSearchKnowledge,
		`{"query":"another sandbox phrase","knowledge_base_ids":["retryable"],"retrieval":{"gap_id":"gap-1","action":"supplement","lead":"sandbox"}}`)
	require.False(t, blocked.Result.Success)
	require.Len(t, probe.calls, 1)
	probe.result = nil
	independent := retrievalProbeCall(e, "supplement", agenttools.ToolSearchKnowledge,
		`{"query":"another sandbox phrase","knowledge_base_ids":["healthy"],"retrieval":{"gap_id":"gap-1","action":"supplement","lead":"sandbox"}}`)
	require.True(t, independent.Result.Success, independent.Result.Error)
	require.NoError(t, e.retrieval.review("gap-1", "supplement", retrievalNoProgress, nil))
	require.Contains(t, e.retrievalStatePrompt(), "retryable", "successful independent retrieval cannot erase the unresolved failure")
	recovery := retrievalProbeCall(e, "recovery", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox","knowledge_base_ids":["retryable"],"retrieval":{"gap_id":"gap-1","action":"recovery","recovery_call_id":"first","lead":"supported account region"}}`)
	require.True(t, recovery.Result.Success, recovery.Result.Error)
	require.NoError(t, e.retrieval.review("gap-1", "recovery", retrievalNoProgress, nil))
	state := e.retrieval.snapshot()
	require.True(t, state.Gaps[0].SupplementUsed)
	require.True(t, state.Gaps[0].RecoveryUsed)
	require.Equal(t, retrievalExhausted, state.Gaps[0].Status)
	require.Len(t, probe.calls, 3)
	require.Len(t, state.Attempts[0].Failures, 2, "computing outstanding failures must not mutate the historical record")
	require.Equal(t, []string{"retryable"}, state.Attempts[0].Failures[0].KnowledgeBaseIDs)
}

func TestRetrievalPartialSupplementStillHasItsSingleRecovery(t *testing.T) {
	e, probe := newPartialRetrievalProbeEngine(t)
	require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, partialRetrievalInitial).Result.Success)
	require.NoError(t, e.retrieval.review("gap-1", "first", retrievalNoProgress, nil))
	probe.result = partialRetrievalResult()
	supplement := retrievalProbeCall(e, "supplement", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox account","retrieval":{"gap_id":"gap-1","action":"supplement","lead":"sandbox"}}`)
	require.True(t, supplement.Result.Success)
	require.NoError(t, e.retrieval.review("gap-1", "supplement", retrievalNoProgress, nil))
	require.Equal(t, retrievalPending, e.retrieval.snapshot().Gaps[0].Status)
	encoded, err := json.Marshal(e.retrieval.snapshot())
	require.NoError(t, err)
	var state retrievalState
	require.NoError(t, json.Unmarshal(encoded, &state))
	e.retrieval, err = restoreRetrievalControl(state)
	require.NoError(t, err)
	probe.result = nil
	recovery := retrievalProbeCall(e, "recovery", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox account","knowledge_base_ids":["retryable"],"retrieval":{"gap_id":"gap-1","action":"recovery","lead":"sandbox"}}`)
	require.True(t, recovery.Result.Success, recovery.Result.Error)
	require.NoError(t, e.retrieval.review("gap-1", "recovery", retrievalNoProgress, nil))
	require.Equal(t, retrievalExhausted, e.retrieval.snapshot().Gaps[0].Status)
	require.Len(t, probe.calls, 3)
	state.Attempts[1].Failures[0].CallID = "forged-observation"
	_, err = restoreRetrievalControl(state)
	require.Error(t, err)
}

func TestRetrievalMixedAndCancelledErrorsAreNotBlindlyRetried(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want retrievalOutcome
	}{
		{fmt.Errorf("adapter: %w", context.DeadlineExceeded), retrievalTransient},
		{errors.Join(context.DeadlineExceeded, context.DeadlineExceeded), retrievalTransient},
		{fmt.Errorf("adapter: %w", errors.Join(context.DeadlineExceeded, errors.New("denied"))), retrievalUnknown},
		{errors.New("timeout, please retry"), retrievalUnknown},
		{context.Canceled, retrievalUnknown},
	} {
		require.Equal(t, tc.want, retrievalFailureOutcome(tc.err))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, failure := range retrievalCallFailures(ctx, partialRetrievalResult()) {
		require.Equal(t, retrievalUnknown, failure.Outcome)
	}
}

func TestRetrievalSearchCannotClaimNecessaryRead(t *testing.T) {
	e, probe := newRetrievalProbeEngine(t)
	probe.output = "A new applicable fact; a document has the remaining steps."
	require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, partialRetrievalInitial).Result.Success)
	require.NoError(t, e.retrieval.review("gap-1", "first", retrievalProgress, []string{"new applicable fact"}))
	blocked := retrievalProbeCall(e, "search-as-read", agenttools.ToolSearchKnowledge,
		`{"query":"sandbox again","retrieval":{"gap_id":"gap-1","action":"read","lead":"sandbox"}}`)
	require.False(t, blocked.Result.Success)
	require.Len(t, probe.calls, 1)
}

type retrievalRetryTransport struct{ attempts int }

func (r *retrievalRetryTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.attempts++
	return nil, context.DeadlineExceeded
}

func TestRetrievalDispatchOwnsTransportRecoveryBudget(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	e, probe := newRetrievalProbeEngine(t)
	transport := &retrievalRetryTransport{}
	endpoint := api.Endpoint{Client: &http.Client{Transport: transport}}
	probe.execute = func(ctx context.Context, _ json.RawMessage) (*types.ToolResult, error) {
		var decoded map[string]any
		err := endpoint.PostJSONWithRetry(ctx, "http://127.0.0.1/retrieval", map[string]any{}, &decoded,
			api.RetryPolicy{MaxRetries: 3}, "test retrieval")
		return nil, err
	}
	require.False(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, partialRetrievalInitial).Result.Success)
	require.Equal(t, 1, transport.attempts)
	retry := `{"query":"sandbox","retrieval":{"gap_id":"gap-1","action":"recovery"}}`
	require.False(t, retrievalProbeCall(e, "retry", agenttools.ToolSearchKnowledge, retry).Result.Success)
	require.Equal(t, 2, transport.attempts)
	require.False(t, retrievalProbeCall(e, "third", agenttools.ToolSearchKnowledge, retry).Result.Success)
	require.Equal(t, 2, transport.attempts, "outer recovery must not multiply a four-attempt transport policy")
	require.Equal(t, retrievalUnavailable, e.retrieval.snapshot().Gaps[0].Status)
}
