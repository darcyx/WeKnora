package agent

import (
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestProjectIntegrationLibrariesRespectTurnScope(t *testing.T) {
	for _, tc := range []struct {
		name, device string
		targets      types.SearchTargets
		ids          []any
		want         []any
	}{
		{"both", "手游、端游", types.SearchTargets{retrievalScopeTarget("mobile"), retrievalScopeTarget("pc"), retrievalScopeTarget("faq")}, []any{"faq", "mobile"}, []any{"faq", "mobile", "pc"}},
		{"mobile only", "手游", types.SearchTargets{retrievalScopeTarget("mobile"), retrievalScopeTarget("pc"), retrievalScopeTarget("faq")}, []any{"faq"}, []any{"faq", "mobile"}},
		{"scoped to faq", "手游、端游", types.SearchTargets{retrievalScopeTarget("faq")}, []any{"faq"}, []any{"faq"}},
		{"unknown", "手游版", types.SearchTargets{retrievalScopeTarget("mobile"), retrievalScopeTarget("faq")}, []any{"faq"}, []any{"faq"}},
		{"invalid selected id", "端游", types.SearchTargets{retrievalScopeTarget("pc")}, []any{"unknown"}, []any{"unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, &mockChat{})
			e.config.SearchTargets = tc.targets
			e.appInfo = `{"gameInfo":{"device_type":"` + tc.device + `"}}`
			e.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "mobile", Name: "docs-client"}, {ID: "pc", Name: "docs-pc"}}
			e.retrieval = newRetrievalControl("taptap沙盒支付怎么设置", retrievalScopeForTargets(tc.targets))
			e.retrieval.state.UserInputIDs = []string{"q"}
			e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "q", Tool: "search_knowledge", Completed: true, Outcome: retrievalSuccess}}
			args := map[string]any{"knowledge_base_ids": tc.ids}
			e.includeProjectIntegrationLibraries(args)
			require.Equal(t, tc.want, args["knowledge_base_ids"])
		})
	}
}

func TestProjectIntegrationLibrariesReachFollowupSearch(t *testing.T) {
	e, search := newRuntimeRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.appInfo = `{"gameInfo":{"device_type":"手游、端游"}}`
	e.config.SearchTargets = types.SearchTargets{retrievalScopeTarget("mobile"), retrievalScopeTarget("pc"), retrievalScopeTarget("faq")}
	e.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "mobile", Name: "docs-client"}, {ID: "pc", Name: "docs-pc"}}
	e.SetRetrievalUserMessage("q", "taptap沙盒支付怎么设置")
	e.beginTurn("taptap沙盒支付怎么设置", nil, &types.AgentState{})
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
	require.True(t, first.Result.Success, first.Result.Error)
	second := retrievalProbeCall(e, "second", agenttools.ToolSearchKnowledge, `{"query":"sandbox setup","knowledge_base_ids":["faq","mobile"]}`)
	require.True(t, second.Result.Success, second.Result.Error)
	require.Len(t, search.calls, 2)
	require.JSONEq(t, `{"query":"sandbox setup","knowledge_base_ids":["faq","mobile","pc"]}`, string(search.calls[1]))
}

func TestProjectIntegrationLibrariesLeaveRecoveryAndFirstSearchAlone(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	e.config.SearchTargets = types.SearchTargets{retrievalScopeTarget("pc"), retrievalScopeTarget("faq")}
	e.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "pc", Name: "docs-pc"}}
	e.appInfo = `{"gameInfo":{"device_type":"端游"}}`
	e.retrieval = newRetrievalControl("question", retrievalScopeForTargets(e.config.SearchTargets))
	e.retrieval.state.UserInputIDs = []string{"q"}
	args := map[string]any{"knowledge_base_ids": []any{"faq"}}
	e.includeProjectIntegrationLibraries(args)
	require.Equal(t, []any{"faq"}, args["knowledge_base_ids"])
	e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "q", Tool: "search_knowledge", Completed: true, Outcome: retrievalSuccess}}
	args["retrieval"] = map[string]any{"action": "recovery"}
	e.includeProjectIntegrationLibraries(args)
	require.Equal(t, []any{"faq"}, args["knowledge_base_ids"])
	args["retrieval"] = map[string]any{"scope_from": "gap-1"}
	e.includeProjectIntegrationLibraries(args)
	require.Equal(t, []any{"faq"}, args["knowledge_base_ids"])
}

func TestProjectIntegrationLibrariesPreserveImplicitRecovery(t *testing.T) {
	e, probe := newPartialRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("q", "taptap沙盒支付怎么设置")
	e.config.SearchTargets = append(e.config.SearchTargets, retrievalScopeTarget("pc"))
	e.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "pc", Name: "docs-pc"}}
	e.appInfo = `{"gameInfo":{"device_type":"端游"}}`
	e.beginTurn("taptap沙盒支付怎么设置", nil, &types.AgentState{})
	probe.result = partialRetrievalResult()
	first := retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`)
	require.True(t, first.Result.Success, first.Result.Error)
	probe.result = nil
	second := retrievalProbeCall(e, "second", agenttools.ToolSearchKnowledge, `{"query":"other","knowledge_base_ids":["healthy"]}`)
	require.True(t, second.Result.Success, second.Result.Error)
	recovery := retrievalProbeCall(e, "recovery", agenttools.ToolSearchKnowledge, `{"query":"sandbox","knowledge_base_ids":["retryable"]}`)
	require.True(t, recovery.Result.Success, recovery.Result.Error)
	require.JSONEq(t, `{"query":"sandbox","knowledge_base_ids":["retryable"]}`, string(probe.calls[2]))
	require.Equal(t, retrievalRecovery, e.retrieval.snapshot().Attempts[2].Action)
}

func TestProjectIntegrationLibrariesAllowIndependentSearchAfterFailure(t *testing.T) {
	e, probe := newPartialRetrievalProbeEngine(t)
	e.EnableRuntimeRetrieval()
	e.SetRetrievalUserMessage("q", "taptap沙盒支付怎么设置")
	e.config.SearchTargets = append(e.config.SearchTargets, retrievalScopeTarget("pc"))
	e.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "pc", Name: "docs-pc"}}
	e.appInfo = `{"gameInfo":{"device_type":"端游"}}`
	e.beginTurn("taptap沙盒支付怎么设置", nil, &types.AgentState{})
	probe.result = partialRetrievalResult()
	require.True(t, retrievalProbeCall(e, "first", agenttools.ToolSearchKnowledge, `{"query":"sandbox"}`).Result.Success)
	probe.result = nil
	require.True(t, retrievalProbeCall(e, "second", agenttools.ToolSearchKnowledge, `{"query":"other","knowledge_base_ids":["healthy"]}`).Result.Success)
	next := retrievalProbeCall(e, "next", agenttools.ToolSearchKnowledge, `{"query":"another","knowledge_base_ids":["healthy"]}`)
	require.True(t, next.Result.Success, next.Result.Error)
	require.JSONEq(t, `{"query":"another","knowledge_base_ids":["healthy","pc"]}`, string(probe.calls[2]))
}

func TestProjectIntegrationLibrariesSkipFAQOnlySearch(t *testing.T) {
	targets := types.SearchTargets{
		retrievalScopeTarget("faq"),
		retrievalScopeTarget("server"),
		retrievalScopeTarget("mobile"),
		retrievalScopeTarget("pc"),
	}
	kbInfos := []*KnowledgeBaseInfo{
		{ID: "faq", Name: "docs-faq", Type: types.KnowledgeBaseTypeFAQ},
		{ID: "server", Name: "docs-server", Type: types.KnowledgeBaseTypeDocument},
		{ID: "mobile", Name: "docs-client", Type: types.KnowledgeBaseTypeDocument},
		{ID: "pc", Name: "docs-pc", Type: types.KnowledgeBaseTypeDocument},
	}
	for _, tc := range []struct {
		name string
		ids  []any
		want []any
	}{
		{"faq only", []any{"faq"}, []any{"faq"}},
		{"mixed faq and mobile", []any{"faq", "mobile"}, []any{"faq", "mobile", "pc"}},
		{"document kb still expands", []any{"server"}, []any{"server", "mobile", "pc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, &mockChat{})
			e.config.SearchTargets = targets
			e.appInfo = `{"gameInfo":{"device_type":"手游、端游"}}`
			e.knowledgeBasesInfo = kbInfos
			e.retrieval = newRetrievalControl("支付未到账", retrievalScopeForTargets(targets))
			e.retrieval.state.UserInputIDs = []string{"q"}
			e.retrieval.state.Attempts = []retrievalAttempt{{QuestionID: "q", Tool: "search_knowledge", Completed: true, Outcome: retrievalSuccess}}
			args := map[string]any{"knowledge_base_ids": tc.ids}
			e.includeProjectIntegrationLibraries(args)
			require.Equal(t, tc.want, args["knowledge_base_ids"])
		})
	}
}

func TestTurnShownChunksPropagatedAndResetOnBeginTurn(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	e.beginTurn("支付未到账", nil, &types.AgentState{})
	require.Nil(t, e.snapshotTurnShownChunks())

	e.recordTurnShownChunks(&types.ToolResult{
		Success: true,
		Data: map[string]interface{}{
			"results": []map[string]interface{}{
				{"chunk_id": "chunk-1"},
				{"faq_id": "faq-2"},
			},
		},
	})
	seen := e.snapshotTurnShownChunks()
	require.Equal(t, map[string]bool{"chunk-1": true, "faq-2": true}, seen)

	seen["chunk-1"] = false
	require.True(t, e.snapshotTurnShownChunks()["chunk-1"], "snapshot must be isolated from caller mutation")

	e.beginTurn("下一轮问题", nil, &types.AgentState{})
	require.Nil(t, e.snapshotTurnShownChunks(), "new turn must clear shown chunks")
}
