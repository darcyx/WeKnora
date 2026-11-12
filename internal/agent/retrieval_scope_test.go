package agent

import (
	"encoding/json"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func retrievalScopeProbe(t *testing.T, targets ...*types.SearchTarget) (*AgentEngine, *retrievalProbe) {
	t.Helper()
	e, p := newRetrievalProbeEngine(t)
	e.config.SearchTargets = targets
	e.beginTurn("TapTap sandbox configuration", nil, &types.AgentState{})
	return e, p
}

func retrievalScopeTarget(kb string, docs ...string) *types.SearchTarget {
	target := &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: kb, TenantID: 1}
	if len(docs) > 0 {
		target.Type, target.KnowledgeIDs = types.SearchTargetTypeKnowledge, docs
	}
	return target
}

func TestRetrievalScopeIncludesEffectiveTargetsWithoutOrderingNoise(t *testing.T) {
	base, _ := retrievalScopeProbe(t, retrievalScopeTarget("kb", "a", "b"))
	same, _ := retrievalScopeProbe(t, retrievalScopeTarget("kb", "b", "a", "a"))
	require.Equal(t, base.retrieval.snapshot().Scope, same.retrieval.snapshot().Scope)
	for _, target := range []*types.SearchTarget{
		retrievalScopeTarget("kb", "c"),
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 2},
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1, TagIDs: []string{"tag-a"}},
	} {
		other, _ := retrievalScopeProbe(t, target)
		require.NotEqual(t, base.retrieval.snapshot().Scope, other.retrieval.snapshot().Scope)
	}
	tagA, _ := retrievalScopeProbe(t, &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1, TagIDs: []string{"a"}})
	tagB, _ := retrievalScopeProbe(t, &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1, ScopeTagIDs: []string{"b"}})
	require.NotEqual(t, tagA.retrieval.snapshot().Scope, tagB.retrieval.snapshot().Scope)
}

func TestRetrievalScopeCannotBeChangedByModelMetadata(t *testing.T) {
	e, probe := retrievalScopeProbe(t, retrievalScopeTarget("kb", "a"))
	first := retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap","fact":"configuration","action":"initial"}}`)
	require.True(t, first.Result.Success)
	before := e.retrieval.snapshot()
	changed := retrievalProbeCall(e, "fake-scope", agenttools.ToolSearchKnowledge,
		`{"query":"other","retrieval":{"goal_quote":"TapTap","fact":"configuration","scope_from":"gap-1","distinct_from":[{"gap_id":"gap-1","reason":"I changed the query"}],"action":"initial"}}`)
	require.False(t, changed.Result.Success)
	require.Equal(t, before, e.retrieval.snapshot())
	require.Len(t, probe.calls, 1)
}

func TestRetrievalScopePreservesPhysicalTagIntersection(t *testing.T) {
	scope := func(docs, tags, logical []string) string {
		return retrievalScopeForTargets(types.SearchTargets{{Type: types.SearchTargetTypeKnowledge,
			KnowledgeBaseID: "kb", TenantID: 1, KnowledgeIDs: docs, TagIDs: tags, ScopeTagIDs: logical}})
	}
	doc := scope([]string{"a"}, nil, nil)
	resolvedDoc := scope([]string{"a"}, nil, []string{"resolved-tag"})
	require.Equal(t, doc, resolvedDoc, "resolved document tag membership does not add another index filter")
	tagA, tagB := scope([]string{"a"}, []string{"tag-a"}, nil), scope([]string{"a"}, []string{"tag-b"}, nil)
	require.NotEqual(t, tagA, tagB, "FAQ document selection still intersects its physical tag filter")
	require.True(t, retrievalScopeCovers(doc, tagA))
	require.False(t, retrievalScopeCovers(tagA, doc), "tag-constrained evidence must not authorize unfiltered reuse")
	require.False(t, retrievalScopeCovers(tagA, tagB))
	wholeTag := retrievalScopeForTargets(types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase,
		KnowledgeBaseID: "kb", TenantID: 1, TagIDs: []string{"tag-a"}}})
	require.True(t, retrievalScopeCovers(wholeTag, tagA))
	require.False(t, retrievalScopeCovers(wholeTag, doc))
	require.True(t, retrievalScopesCover([]string{tagA, tagB}, scope([]string{"a"}, []string{"tag-b", "tag-a"}, nil)))
}

func TestRetrievalScopeRestoreRejectsInvalidRevision(t *testing.T) {
	a := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("kb", "a")})
	b := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("kb", "b")})
	c := newRetrievalControl("TapTap sandbox configuration", a)
	_, err := c.register(retrievalFactProposal{GoalQuote: "TapTap", Fact: "configuration"})
	require.NoError(t, err)
	require.NoError(t, c.activateScope(b))
	_, err = c.register(retrievalFactProposal{GoalQuote: "TapTap", Fact: "configuration", ScopeFrom: "gap-1",
		DistinctFrom: []retrievalDistinction{{GapID: "gap-1", Reason: "new document b"}}})
	require.NoError(t, err)
	_, err = restoreRetrievalControl(c.snapshot())
	require.NoError(t, err)
	for name, corrupt := range map[string]func(*retrievalState){
		"missing parent": func(s *retrievalState) { s.Gaps[1].ScopeFrom = "" },
		"unknown scope":  func(s *retrievalState) { s.Scopes = append(s.Scopes, "unverified scope") },
		"narrow revision": func(s *retrievalState) {
			whole := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("kb")})
			s.Gaps[0].Scope, s.Scopes[0] = whole, whole
		},
		"duplicate identity": func(s *retrievalState) { s.Gaps[1].Scope, s.Gaps[1].ScopeFrom = a, "" },
		"forward parent":     func(s *retrievalState) { s.Gaps[0].ScopeFrom = "gap-2" },
	} {
		t.Run(name, func(t *testing.T) {
			state := c.snapshot()
			corrupt(&state)
			_, err := restoreRetrievalControl(state)
			require.Error(t, err)
		})
	}
}

func TestRetrievalScopeOldEvidenceCannotSatisfyDependency(t *testing.T) {
	a := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("kb", "a")})
	b := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("kb", "b")})
	c := newRetrievalControl("TapTap sandbox configuration", a)
	parent, err := c.register(retrievalFactProposal{GoalQuote: "TapTap", Fact: "support"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(parent, "search", retrievalInitial, "", agenttools.ToolSearchKnowledge, `{}`))
	require.NoError(t, c.observe("search", "sandbox is supported", retrievalSuccess))
	require.NoError(t, c.applyReview(retrievalReview{GapID: parent, CallID: "search", Status: retrievalSatisfied, Quotes: []string{"sandbox is supported"}}))
	require.NoError(t, c.activateScope(b))
	child, err := c.register(retrievalFactProposal{GoalQuote: "TapTap", Fact: "configuration", DependsOn: []string{parent},
		DistinctFrom: []retrievalDistinction{{GapID: parent, Reason: "steps depend on support"}}})
	require.NoError(t, err)
	// An in-memory snapshot may carry raw results; restoration must enforce its active scope too.
	state := c.snapshot()
	state.Attempts[0].Content = "sandbox is supported"
	restored, err := restoreRetrievalForScope(state, b)
	require.NoError(t, err)
	require.Empty(t, restored.snapshot().Attempts[0].Content)
	require.Error(t, restored.acquire(child, "dependent", retrievalInitial, "", agenttools.ToolSearchKnowledge, `{}`))
	e, _ := retrievalScopeProbe(t, retrievalScopeTarget("kb", "b"))
	e.retrieval = restored
	require.Contains(t, e.retrievalStatePrompt(), `"evidence_available":false`, "a saved sufficient verdict alone cannot stand in for available evidence")
}

func TestRetrievalScopeExpansionRestorationRejectsMixedReservation(t *testing.T) {
	a := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("old-kb")})
	ab := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("old-kb"), retrievalScopeTarget("new-kb")})
	c := newRetrievalControl("TapTap sandbox configuration", a)
	id, err := c.register(retrievalFactProposal{GoalQuote: "TapTap", Fact: "configuration"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(id, "old", retrievalInitial, "", agenttools.ToolSearchKnowledge, `{}`))
	require.NoError(t, c.observe("old", "no relevant evidence", retrievalSuccess))
	require.NoError(t, c.applyReview(retrievalReview{GapID: id, CallID: "old", Status: retrievalExhausted}))
	require.NoError(t, c.activateScope(ab))
	nextID, err := c.register(retrievalFactProposal{GoalQuote: "TapTap", Fact: "configuration", ScopeFrom: id,
		DistinctFrom: []retrievalDistinction{{GapID: id, Reason: "new library"}}})
	require.NoError(t, err)
	require.NoError(t, c.acquire(nextID, "new", retrievalInitial, "", agenttools.ToolSearchKnowledge, `{"knowledge_base_ids":["new-kb"]}`))
	saved := c.snapshot()
	_, err = restoreRetrievalForScope(saved, ab)
	require.NoError(t, err, "an interrupted disjoint attempt retains its reservation")
	saved.Attempts[1].Scope, saved.Attempts[1].Arguments = ab, `{}`
	_, err = restoreRetrievalForScope(saved, ab)
	require.Error(t, err, "restoration must not legitimize a mixed old/new range as a new allowance")
}

func TestRetrievalSourceOverlapRespectsTargetIntersections(t *testing.T) {
	target := func(docs, tags []string) *types.SearchTarget {
		return &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, TenantID: 1,
			KnowledgeBaseID: "kb", KnowledgeIDs: docs, TagIDs: tags}
	}
	for _, tc := range []struct {
		name     string
		old, now types.SearchTargets
		overlap  bool
	}{
		{"new library", types.SearchTargets{retrievalScopeTarget("a")}, types.SearchTargets{retrievalScopeTarget("b")}, false},
		{"library union", types.SearchTargets{retrievalScopeTarget("a")}, types.SearchTargets{retrievalScopeTarget("a"), retrievalScopeTarget("b")}, true},
		{"document union", types.SearchTargets{target([]string{"a"}, nil)}, types.SearchTargets{target([]string{"a", "b"}, nil)}, true},
		{"disjoint documents", types.SearchTargets{target([]string{"a"}, nil)}, types.SearchTargets{target([]string{"b"}, nil)}, false},
		{"whole library and document", types.SearchTargets{target(nil, nil)}, types.SearchTargets{target([]string{"b"}, nil)}, true},
		{"tag union", types.SearchTargets{target(nil, []string{"a"})}, types.SearchTargets{target(nil, []string{"a", "b"})}, true},
		{"different physical tags", types.SearchTargets{target([]string{"doc"}, []string{"a"})}, types.SearchTargets{target([]string{"doc"}, []string{"b"})}, false},
		{"document and tag intersection", types.SearchTargets{target([]string{"doc"}, nil)}, types.SearchTargets{target([]string{"doc"}, []string{"a"})}, true},
		{"empty document target", types.SearchTargets{{Type: types.SearchTargetTypeKnowledge, TenantID: 1, KnowledgeBaseID: "kb"}}, types.SearchTargets{target(nil, nil)}, false},
		{"different owner", types.SearchTargets{target(nil, nil)}, types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, TenantID: 2, KnowledgeBaseID: "kb"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, now := retrievalScopeForTargets(tc.old), retrievalScopeForTargets(tc.now)
			require.Equal(t, tc.overlap, retrievalScopesOverlap([]string{old}, now))
			require.Equal(t, tc.overlap, retrievalScopesOverlap([]string{now}, old))
		})
	}
	require.True(t, retrievalScopesOverlap([]string{`legacy`}, `unknown`), "unknown bounds cannot prove independence")
}

func TestRetrievalScopeSelectorsMatchTheActualTool(t *testing.T) {
	a := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("a")})
	ab := retrievalScopeForTargets(types.SearchTargets{retrievalScopeTarget("a"), retrievalScopeTarget("b")})
	for _, tc := range []struct{ tool, args, want string }{
		{"search_knowledge", `{"knowledge_base_ids":["a"]}`, a},
		{"query_knowledge_graph", `{"knowledge_base_ids":["a"]}`, a},
		{"wiki_search", `{"knowledge_base_ids":["a"]}`, a},
		{"wiki_search", `{"knowledge_base_id":"a"}`, a},
		{"wiki_search", `{"knowledge_base_ids":["a"],"knowledge_base_id":"b"}`, ab},
		{"read_document", `{"id":"old-document","knowledge_base_ids":["a"]}`, ab},
		{"search_knowledge", `{"knowledge_base_ids":["unknown"]}`, ab},
	} {
		t.Run(tc.tool+tc.args, func(t *testing.T) {
			require.Equal(t, tc.want, retrievalExecutionScope(ab, tc.tool, tc.args))
		})
	}
}

func TestRetrievalScopeResumeOmitsRevokedTextAndImages(t *testing.T) {
	const evidence = "REVOKED-SOURCE-EVIDENCE"
	first, probe := retrievalScopeProbe(t, retrievalScopeTarget("old-kb"))
	probe.result = &types.ToolResult{Success: true, Output: evidence,
		Images: []string{"data:image/png;base64,private"}, ImageDescriptions: []string{evidence + " image"}}
	first.config.ChatModelSupportsVision = true
	asker := &retrievalAsker{}
	first.toolRegistry.RegisterTool(newLegacyAskUserTool(asker))
	first.SetRunMode(types.RunModeAttended)
	first.EnableSuspension(json.RawMessage(`{}`))
	state := executeRetrievalIdentityTurn(t, first, "turn-a", nil,
		retrievalIdentityCall("search", agenttools.ToolSearchKnowledge, `{"query":"sandbox","retrieval":{"goal_quote":"TapTap","fact":"configuration","action":"initial"}}`),
		retrievalIdentityCall("ask", agenttools.ToolAskUser, `{"question":"Which platform?"}`))
	require.NotNil(t, state.Suspended)
	for _, vision := range []bool{false, true} {
		t.Run(map[bool]string{false: "descriptions", true: "images"}[vision], func(t *testing.T) {
			next, searches := retrievalScopeProbe(t, retrievalScopeTarget("new-kb"))
			next.config.ChatModelSupportsVision = vision
			next.toolRegistry.RegisterTool(agenttools.NewAskUserTool(asker))
			model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer,
				Content: "Current evidence is unavailable.", Done: true, FinishReason: "stop"}}}}}
			next.chatModel = model
			_, err := next.Resume(t.Context(), "session", "turn-a", nil, asker.snapshot,
				&types.AgentPrompt{ID: "p1", Kind: types.AgentPromptKindAskUser, ToolCallID: "ask",
					Status: types.AgentPromptAnswered, Response: types.JSON(`{"text":"iOS"}`)})
			require.NoError(t, err)
			require.NotEmpty(t, model.calls)
			for _, messages := range model.calls {
				for _, msg := range messages {
					require.NotContains(t, msg.Content, evidence)
					require.Empty(t, msg.Images)
				}
			}
			require.Empty(t, next.retrieval.snapshot().Attempts[0].Content)
			require.Empty(t, searches.calls)
		})
	}
}

func TestRetrievalScopeRejectedReplayCannotReplaceMissingEvidence(t *testing.T) {
	e, probe := retrievalScopeProbe(t, retrievalScopeTarget("kb"))
	args := `{"query":"sandbox","retrieval":{"goal_quote":"TapTap","fact":"configuration","action":"initial"}}`
	first := retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, args)
	require.True(t, first.Result.Success)
	e.retrieval.state.Attempts[0].Content = ""
	rejected := retrievalProbeCall(e, "search", agenttools.ToolSearchKnowledge, args)
	require.False(t, rejected.Result.Success)
	e.appendToolResults(nil, types.AgentStep{ToolCalls: []types.ToolCall{rejected}})
	require.Empty(t, e.retrieval.snapshot().Attempts[0].Content, "decision feedback is not the previous successful observation")
	require.Len(t, probe.calls, 1)
}
