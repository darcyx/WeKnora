package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

type retrievalSourceScope struct {
	Version int                     `json:"version"`
	Targets []retrievalSourceTarget `json:"targets"`
}

type retrievalSourceTarget struct {
	Type      types.SearchTargetType `json:"type"`
	KBID      string                 `json:"knowledge_base_id"`
	TenantID  uint64                 `json:"tenant_id"`
	Documents []string               `json:"knowledge_ids,omitempty"`
	Tags      []string               `json:"tag_ids,omitempty"`
}

func sortedRetrievalIDs(ids []string) []string {
	ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return strings.TrimSpace(id) == "" })
	slices.Sort(ids)
	return slices.Compact(ids)
}

func retrievalScopeForTargets(targets types.SearchTargets) string {
	scope := retrievalSourceScope{Version: 1, Targets: make([]retrievalSourceTarget, 0, len(targets))}
	for _, target := range targets {
		if target == nil || target.KnowledgeBaseID == "" {
			continue
		}
		item := retrievalSourceTarget{Type: target.Type, KBID: target.KnowledgeBaseID, TenantID: target.TenantID,
			Documents: sortedRetrievalIDs(target.KnowledgeIDs)}
		if len(item.Documents) > 0 {
			// ScopeTagIDs may already be resolved into documents; physical TagIDs still intersect FAQ results.
			item.Type = types.SearchTargetTypeKnowledge
			item.Tags = sortedRetrievalIDs(target.TagIDs)
		} else {
			item.Tags = sortedRetrievalIDs(append(slices.Clone(target.TagIDs), target.ScopeTagIDs...))
		}
		scope.Targets = append(scope.Targets, item)
	}
	return encodeRetrievalScope(scope)
}

func encodeRetrievalScope(scope retrievalSourceScope) string {
	key := func(t retrievalSourceTarget) string { data, _ := json.Marshal(t); return string(data) }
	slices.SortFunc(scope.Targets, func(a, b retrievalSourceTarget) int { return strings.Compare(key(a), key(b)) })
	scope.Targets = slices.CompactFunc(scope.Targets, func(a, b retrievalSourceTarget) bool { return key(a) == key(b) })
	data, _ := json.Marshal(scope)
	return string(data)
}

func decodeRetrievalScope(encoded string) (retrievalSourceScope, bool) {
	var scope retrievalSourceScope
	if json.Unmarshal([]byte(encoded), &scope) != nil || scope.Version != 1 || scope.Targets == nil {
		return scope, false
	}
	for _, target := range scope.Targets {
		if target.KBID == "" || (target.Type != types.SearchTargetTypeKnowledgeBase && target.Type != types.SearchTargetTypeKnowledge) {
			return scope, false
		}
	}
	return scope, true
}

func retrievalScopeCovers(available, requested string) bool {
	return retrievalScopesCover([]string{available}, requested)
}

// Targets are a union; each target's document and physical tag filters intersect.
// Unknown legacy scopes may only match exactly; their permissions cannot be guessed.
func retrievalScopesCover(available []string, requested string) bool {
	if slices.Contains(available, requested) {
		return true
	}
	wanted, ok := decodeRetrievalScope(requested)
	if !ok {
		return false
	}
	var targets []retrievalSourceTarget
	for _, encoded := range available {
		if scope, ok := decodeRetrievalScope(encoded); ok {
			targets = append(targets, scope.Targets...)
		}
	}
	for _, need := range wanted.Targets {
		if need.Type == types.SearchTargetTypeKnowledge && len(need.Documents) == 0 && len(need.Tags) == 0 {
			continue
		}
		docs, tags := need.Documents, need.Tags
		if len(docs) == 0 {
			docs = []string{""}
		}
		if len(tags) == 0 {
			tags = []string{""}
		}
		for _, doc := range docs {
			for _, tag := range tags {
				covered := slices.ContainsFunc(targets, func(have retrievalSourceTarget) bool {
					if have.KBID != need.KBID || have.TenantID != need.TenantID {
						return false
					}
					allDocs := len(have.Documents) == 0 && (have.Type == types.SearchTargetTypeKnowledgeBase || len(have.Tags) > 0)
					return (allDocs || doc != "" && slices.Contains(have.Documents, doc)) &&
						(len(have.Tags) == 0 || tag != "" && slices.Contains(have.Tags, tag))
				})
				if !covered {
					return false
				}
			}
		}
	}
	return true
}

// An expanded scope may spend new allowances only on ranges absent from the old scope.
func retrievalScopesOverlap(previous []string, requested string) bool {
	wanted, ok := decodeRetrievalScope(requested)
	if !ok {
		return true // Unverified boundaries cannot establish an independent range.
	}
	intersects := func(a, b []string) bool {
		return len(a) == 0 || len(b) == 0 || slices.ContainsFunc(a, func(id string) bool { return slices.Contains(b, id) })
	}
	empty := func(t retrievalSourceTarget) bool {
		return t.Type == types.SearchTargetTypeKnowledge && len(t.Documents) == 0 && len(t.Tags) == 0
	}
	for _, encoded := range previous {
		old, ok := decodeRetrievalScope(encoded)
		if !ok {
			return true
		}
		for _, have := range old.Targets {
			for _, need := range wanted.Targets {
				if !empty(have) && !empty(need) && have.KBID == need.KBID && have.TenantID == need.TenantID &&
					intersects(have.Documents, need.Documents) && intersects(have.Tags, need.Tags) {
					return true
				}
			}
		}
	}
	return false
}

func (c *retrievalControl) activateScope(scope string) error {
	if c.state.Scope != scope {
		if _, ok := decodeRetrievalScope(c.state.Scope); !ok {
			return fmt.Errorf("legacy retrieval scope cannot be re-evaluated without verified source boundaries")
		}
		if _, ok := decodeRetrievalScope(scope); !ok {
			return fmt.Errorf("current retrieval scope has no verified source boundaries")
		}
		c.state.Scope = scope
		if !slices.Contains(c.state.Scopes, scope) {
			c.state.Scopes = append(c.state.Scopes, scope)
		}
	}
	for i := range c.state.Attempts {
		if !retrievalScopeCovers(scope, c.state.Attempts[i].Scope) {
			c.state.Attempts[i].Content = ""
		}
	}
	return nil
}

func restoreRetrievalForScope(saved retrievalState, scope string) (*retrievalControl, error) {
	c, err := restoreRetrievalControl(saved)
	if err != nil {
		return nil, err
	}
	if err = c.activateScope(scope); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *retrievalControl) restoreScopes(legacy bool) error {
	if legacy {
		if len(c.state.Scopes) != 0 {
			return fmt.Errorf("legacy retrieval state cannot contain scope records")
		}
		c.state.Scopes = []string{c.state.Scope}
		for i := range c.state.Gaps {
			g := &c.state.Gaps[i]
			if g.Scope != "" || g.ScopeFrom != "" {
				return fmt.Errorf("legacy fact cannot contain scope revisions")
			}
			g.Scope = c.state.Scope
		}
		for i := range c.state.Attempts {
			if c.state.Attempts[i].Scope != "" {
				return fmt.Errorf("legacy call cannot contain a scope revision")
			}
			c.state.Attempts[i].Scope = c.state.Scope
		}
		c.state.Version = 3
	}
	seen := make(map[string]bool)
	for _, scope := range c.state.Scopes {
		if seen[scope] {
			return fmt.Errorf("duplicate saved retrieval scope")
		}
		if _, ok := decodeRetrievalScope(scope); !ok && len(c.state.Scopes) > 1 {
			return fmt.Errorf("scope revisions require verified source boundaries")
		}
		seen[scope] = true
	}
	if !seen[c.state.Scope] {
		return fmt.Errorf("active retrieval scope has no saved record")
	}
	return nil
}

func (c *retrievalControl) validateGapScope(g *retrievalGap) error {
	if !slices.Contains(c.state.Scopes, g.Scope) {
		return fmt.Errorf("fact has no registered source scope")
	}
	var priorScopes []string
	parentFound := false
	for i := range c.state.Gaps {
		prior := &c.state.Gaps[i]
		if prior.ID == g.ID {
			break
		}
		if sameRetrievalFact(prior, g.Fact, g.Conditions) {
			priorScopes = append(priorScopes, prior.Scope)
			parentFound = parentFound || prior.ID == g.ScopeFrom
		}
	}
	if len(priorScopes) == 0 && g.ScopeFrom == "" {
		return nil
	}
	if !parentFound || retrievalScopesCover(priorScopes, g.Scope) {
		return fmt.Errorf("fact scope revision must reference an earlier matching fact and add a verified source range")
	}
	for _, attempt := range c.state.Attempts {
		if attempt.GapID == g.ID && retrievalScopesOverlap(priorScopes, attempt.Scope) {
			return fmt.Errorf("expanded fact reservation includes an earlier source range")
		}
	}
	return nil
}

func (c *retrievalControl) observationInScope(callID, tool string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempt(callID)
	return a != nil && a.Completed && a.Tool == tool && retrievalScopeCovers(c.state.Scope, a.Scope)
}

func (e *AgentEngine) retrievalResultInScope(call types.ToolCall) bool {
	if e.retrieval == nil || !kbToolNames[call.Name] {
		return true
	}
	if call.Result != nil && call.Result.Data[retrievalDispatchBlocked] == true {
		return true // Rejected decisions contain feedback, not retrieved evidence.
	}
	return e.retrieval.observationInScope(e.retrievalCallReference(call.ID), call.Name)
}

// Narrow only from selectors the actual tool consumes, never from ignored fields or query wording.
// shortcut: only supported KB selectors narrow reservations; cross-scope readers still need verified document/chunk provenance.
func retrievalExecutionScope(scope, tool, arguments string) string {
	if tool != "search_knowledge" && tool != "query_knowledge_graph" && tool != "wiki_search" {
		return scope
	}
	parsed, ok := decodeRetrievalScope(scope)
	if !ok {
		return scope
	}
	var args struct {
		KBIDs []string `json:"knowledge_base_ids"`
		KBID  string   `json:"knowledge_base_id"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return scope
	}
	if tool == "wiki_search" && args.KBID != "" {
		args.KBIDs = append(args.KBIDs, args.KBID) // Wiki search unions its legacy singular selector.
	}
	if len(args.KBIDs) == 0 {
		return scope
	}
	for _, id := range args.KBIDs {
		if !slices.ContainsFunc(parsed.Targets, func(t retrievalSourceTarget) bool { return t.KBID == id }) {
			return scope // The tool still validates invalid parameters; do not infer an empty authorized scope.
		}
	}
	parsed.Targets = slices.DeleteFunc(parsed.Targets, func(t retrievalSourceTarget) bool { return !slices.Contains(args.KBIDs, t.KBID) })
	return encodeRetrievalScope(parsed)
}
