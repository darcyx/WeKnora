package service

import (
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// Domain-specific fast-answer eligibility hook isolated from session_agent_qa.go
// so upstream QA orchestration stays free of vertical Chinese navigation regexes.
var resourceNavigationQuestion = regexp.MustCompile(`^(?:(?:服务端|客户端|SDK|PCSDK|接口|开发|接入|项目)文档(?:是什么|在哪里|在哪|入口在哪|入口在哪里)|什么是(?:服务端|客户端|SDK|PCSDK|接口|开发|接入|项目)文档)[？?。]?$`)

func eligibleResourceNavigation(req *types.QARequest, cfg *types.AgentConfig) bool {
	if req == nil || cfg == nil || req.ReasoningEffort != "" || cfg.PlanningEnabled ||
		(cfg.ReasoningEffort != "" && cfg.ReasoningEffort != "minimal") || req.Resume != nil || req.RunNote != "" ||
		req.Session == nil || req.Session.Kind == types.SessionKindAgentDelegated || len(req.SkillNames) > 0 || len(req.MCPServiceIDs) > 0 ||
		len(req.ImageURLs) > 0 || req.ImageDescription != "" || len(req.Attachments) > 0 || req.QuotedContext != "" ||
		req.PageContext != nil || req.LocalBrowserEnabled || req.WebSearchEnabled {
		return false
	}
	return resourceNavigationQuestion.MatchString(strings.TrimSpace(req.Query))
}
