package agent

import (
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// EngineHooks isolates downstream/vertical domain customizations from the
// generic WeKnora AgentEngine core. Implementations must be stateless and safe
// for concurrent multi-replica use.
type EngineHooks interface {
	// RenderAppInfo wraps sanitized app_info JSON into the runtime context XML envelope.
	RenderAppInfo(appInfo string) string
	// OnBeforeToolDispatch may rewrite or expand arguments before KB/search tool dispatch.
	OnBeforeToolDispatch(e *AgentEngine, response *types.ChatResponse)
	// ExpandSearchKnowledgeBases may enrich search_knowledge knowledge_base_ids within
	// the already authorized SearchTargets scope (e.g., platform-to-KB routing).
	ExpandSearchKnowledgeBases(e *AgentEngine, args map[string]any)
	// PostProcessAnswer applies domain-specific output normalization (e.g. A2UI follow-up buttons).
	PostProcessAnswer(e *AgentEngine, answer string) string
	// StripFollowUpButtons removes domain follow-up button markup from standalone text.
	StripFollowUpButtons(answer string) string
	// IsResourceNavigationFastAnswer determines whether a single-wiki read can skip
	// extended thinking on the answering round.
	IsResourceNavigationFastAnswer(e *AgentEngine, messages []chat.Message) bool
}

// NoopEngineHooks provides a pure generic WeKnora engine with zero vertical business rules.
type NoopEngineHooks struct{}

func (NoopEngineHooks) RenderAppInfo(appInfo string) string {
	return renderGenericAppInfoBlock(appInfo)
}

func (NoopEngineHooks) OnBeforeToolDispatch(*AgentEngine, *types.ChatResponse)  {}
func (NoopEngineHooks) ExpandSearchKnowledgeBases(*AgentEngine, map[string]any) {}
func (NoopEngineHooks) PostProcessAnswer(_ *AgentEngine, answer string) string  { return answer }
func (NoopEngineHooks) StripFollowUpButtons(answer string) string               { return answer }
func (NoopEngineHooks) IsResourceNavigationFastAnswer(*AgentEngine, []chat.Message) bool {
	return false
}

// SetHooks installs custom downstream extension hooks on the engine.
// Passing nil resets to NoopEngineHooks for pure generic behavior.
func (e *AgentEngine) SetHooks(hooks EngineHooks) {
	if e == nil {
		return
	}
	if hooks == nil {
		e.hooks = NoopEngineHooks{}
		return
	}
	e.hooks = hooks
}

func (e *AgentEngine) extHooks() EngineHooks {
	if e != nil && e.hooks != nil {
		return e.hooks
	}
	return defaultEngineHooks
}
