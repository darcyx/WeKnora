package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	appconfig "github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	modelruntime "github.com/Tencent/WeKnora/internal/models/runtime"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// modelMaxOutputTokens returns the configured or cataloged output limit of a
// chat model, or zero when neither declares one.
func modelMaxOutputTokens(m *types.Model) int {
	if m.Parameters.MaxOutputTokens > 0 {
		return m.Parameters.MaxOutputTokens
	}
	if m.Source != types.ModelSourceRemote {
		return 0
	}
	resolved, err := modelruntime.Resolve(modelruntime.Ref{
		Provider: m.Parameters.Provider, Model: m.Name, BaseURL: m.Parameters.BaseURL,
		ModelType: types.ModelTypeKnowledgeQA, Extra: m.Parameters.ExtraConfig, Override: m.Parameters.Spec,
	})
	if err != nil {
		return 0
	}
	return resolved.Capabilities().MaxOutputTokens
}

func (s *sessionService) resolveFAQPreflightModel(
	ctx context.Context,
	modelCtx context.Context,
	req *types.QARequest,
	fallback chat.Chat,
) chat.Chat {
	var modelID string
	if req != nil && req.CustomAgent != nil {
		modelID = strings.TrimSpace(req.CustomAgent.Config.FAQPreflightModelID)
	}
	if modelID == "" && s.cfg != nil && s.cfg.Agent != nil {
		modelID = strings.TrimSpace(s.cfg.Agent.FAQPreflightModelID)
	}
	if modelID == "" || s.modelService == nil {
		return fallback
	}
	model, err := s.modelService.GetChatModel(modelCtx, modelID)
	if err != nil || model == nil {
		logger.Warnf(ctx, "Failed to get FAQ preflight model %s, falling back to agent chat model: %v", modelID, err)
		return fallback
	}
	return model
}

// AgentQA performs agent-based question answering with conversation history and streaming support
// customAgent is optional - if provided, uses custom agent configuration instead of tenant defaults
// summaryModelID is optional - if provided, overrides the model from customAgent config
func (s *sessionService) AgentQA(
	ctx context.Context,
	req *types.QARequest,
	eventBus *event.EventBus,
) error {
	ctx = types.WithObservationAppInfo(ctx, req.AppInfo)
	if s.cfg != nil && s.cfg.Agent != nil && s.cfg.Agent.QATotalTimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(s.cfg.Agent.QATotalTimeoutSeconds)*time.Second)
		defer cancel()
	}

	ctx, setup, err := s.prepareAgentTurnSetup(ctx, req)
	if err != nil {
		return err
	}

	if !req.TurnLeaseHeld {
		turnCtx, releaseTurn, err := s.holdSandboxTurn(ctx, setup.sessionID, setup.agentConfig.SandboxConfigID)
		if err != nil {
			return err
		}
		defer releaseTurn()
		ctx = turnCtx
	}

	qaCtx := tools.WithFAQPreflightCache(ctx)
	prepCtx, cancelPrep := context.WithCancel(qaCtx)
	defer cancelPrep()

	stager, ok := s.agentService.(sessionAttachmentStager)
	if !ok {
		return errors.New("agent service does not support session attachment staging")
	}

	baseAgentConfig := cloneAgentConfigForRun(setup.agentConfig)
	faqModel, faqPreflight := s.startAgentFAQPreflight(qaCtx, cancelPrep, req, setup, baseAgentConfig, eventBus)
	defer faqPreflight.stop()

	baseQACtx := qaCtx
	var (
		prep          *agentTurnPrep
		engineStarted bool
	)
	releaseEngine := func() {
		if prep == nil || prep.engine == nil || engineStarted {
			return
		}
		if cleaner, ok := prep.engine.(interface{ Cleanup(context.Context) }); ok {
			cleaner.Cleanup(context.WithoutCancel(qaCtx))
		}
		prep.engine = nil
	}
	defer releaseEngine()

	var prepErr error
	prep, prepErr = s.prepareAgentTurnEngine(
		prepCtx, baseQACtx, req, setup, baseAgentConfig, stager, eventBus, faqModel, faqPreflight.pending,
	)

	if faqPreflight.pending {
		faqPreflight.pending = false
		if prepErr != nil {
			faqPreflight.stop()
		}
		answered, withdrawn, faqErr := faqPreflight.finish(qaCtx, prepCtx, req, eventBus)
		switch {
		case answered:
			return nil
		case faqErr != nil && (prepErr == nil || (prepCtx.Err() != nil && qaCtx.Err() == nil)):
			return faqErr
		case faqErr != nil && !errors.Is(faqErr, context.Canceled):
			logger.Warnf(qaCtx, "FAQ preflight not published: %v", faqErr)
		case withdrawn:
			logger.Infof(qaCtx, "FAQ answer withdrawn by a steer, preparing the agent again (session=%s, message=%s)", setup.sessionID, req.AssistantMessageID)
			releaseEngine()
			prep, prepErr = s.prepareAgentTurnEngine(
				baseQACtx, baseQACtx, req, setup, baseAgentConfig, stager, eventBus, faqModel, false,
			)
		}
		if faqErr == nil && !withdrawn && faqPreflight.evaluation != nil && faqPreflight.evaluation.Outcome == "miss_insufficient" &&
			faqPreflight.evaluation.BroadDocumentSearch && !faqHistoryRestrictsSources(setup.llmContext) && prep != nil && prep.engine != nil && faqTurnUnsteered(qaCtx, req) {
			if receiver, ok := prep.engine.(interface{ SetFAQBroadDocumentSearch(string) }); ok {
				receiver.SetFAQBroadDocumentSearch(req.Query)
			}
		}
		if faqPreflight.cfg.FAQPreflightUsage != nil && prep != nil {
			addFAQPreflightUsage(prep.config, *faqPreflight.cfg.FAQPreflightUsage)
		}
		if faqPreflight.cfg.FAQPreflightChecked && prep != nil {
			prep.config.FAQPreflightChecked = true
		}
		if len(faqPreflight.cfg.FAQPreflightQuestions) > 0 && prep != nil {
			prep.config.FAQPreflightQuestions = append(prep.config.FAQPreflightQuestions, faqPreflight.cfg.FAQPreflightQuestions...)
		}
		if prep != nil && len(faqPreflight.cfg.FAQPreflightRejections) > 0 {
			if prep.config.FAQPreflightRejections == nil {
				prep.config.FAQPreflightRejections = make(map[string]types.FAQPreflightRejection)
			}
			maps.Copy(prep.config.FAQPreflightRejections, faqPreflight.cfg.FAQPreflightRejections)
		}
		if prep != nil && prep.engine != nil && (faqPreflight.cfg.FAQPreflightUsage != nil || len(faqPreflight.cfg.FAQPreflightQuestions) > 0 || len(faqPreflight.cfg.FAQPreflightRejections) > 0) {
			if applier, ok := prep.engine.(interface {
				ApplyFAQPreflight(*types.TokenUsage, []string, map[string]types.FAQPreflightRejection)
			}); ok {
				applier.ApplyFAQPreflight(faqPreflight.cfg.FAQPreflightUsage, faqPreflight.cfg.FAQPreflightQuestions, faqPreflight.cfg.FAQPreflightRejections)
			}
		}
	}
	if prepErr != nil {
		return prepErr
	}
	return s.executePreparedAgentTurn(qaCtx, req, setup, prep, stager, eventBus, func() {
		engineStarted = true
	})
}

func faqSourceHistory(history []chat.Message) ([]string, bool) {
	var messages []string
	length := 0
	for _, message := range history {
		if message.Role != "user" {
			continue
		}
		content := message.Content
		if message.OriginalUserText != nil {
			content = *message.OriginalUserText
		}
		length += len([]rune(content))
		if length > 16000 {
			return nil, false
		}
		messages = append(messages, content)
	}
	return messages, true
}

func faqHistoryRestrictsSources(history []chat.Message) bool {
	for _, message := range history {
		if message.Role != "user" {
			continue
		}
		content := message.Content
		if message.OriginalUserText != nil {
			content = *message.OriginalUserText
		}
		if agent.HasFAQSourceRestriction(content) {
			return true
		}
	}
	return false
}

type agentTurnPrep struct {
	config            *types.AgentConfig
	qaCtx             context.Context
	layout            sandbox.WorkspaceLayout
	deferEmptyStaging bool
	engine            interfaces.AgentEngine
	recall            interfaces.MemoryRecall
	agentQuery        string
	agentImageURLs    []string
}

func cloneAgentConfigForRun(cfg *types.AgentConfig) *types.AgentConfig {
	cloned := cloneAgentConfig(cfg)
	if cloned != nil {
		cloned.AttachmentSandboxCopies = nil
	}
	return cloned
}

type dataURIImageDescriber interface {
	DescribeDataURIImages(context.Context, []string) []string
}

func agentImageDescriber(engine any) func(context.Context, []string) []string {
	describer, ok := engine.(dataURIImageDescriber)
	if !ok || describer == nil {
		return nil
	}
	return func(ctx context.Context, urls []string) []string {
		imageCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return describer.DescribeDataURIImages(imageCtx, urls)
	}
}

// composeAgentImageQuery keeps images on the query when the chat model can
// see them. Otherwise it uses an existing description, or asks the VLM
// describer, so a text-only agent still receives the upload.
func composeAgentImageQuery(
	ctx context.Context,
	query, description string,
	vision bool,
	urls []string,
	describe func(context.Context, []string) []string,
) (string, []string) {
	if vision && len(urls) > 0 {
		return query, urls
	}
	text := strings.TrimSpace(description)
	if text == "" && len(urls) > 0 && describe != nil {
		var kept []string
		for _, part := range describe(ctx, urls) {
			if part = strings.TrimSpace(part); part != "" {
				kept = append(kept, part)
			}
		}
		text = strings.Join(kept, "\n")
	}
	if text != "" {
		return query + "\n\n[用户上传图片内容]\n" + text, nil
	}
	return query, nil
}

// resumableAgentRequest keeps the durable part of req needed to rebuild the
// run elsewhere. Session, attachments and images are reloaded from storage.
func resumableAgentRequest(ctx context.Context, req *types.QARequest) types.ResumableAgentRequest {
	r := types.ResumableAgentRequest{
		ActingVia:           types.ActingViaFromContext(ctx),
		Query:               req.Query,
		SummaryModelID:      req.SummaryModelID,
		ReasoningEffort:     req.ReasoningEffort,
		SharedAgentReadOnly: req.SharedAgentReadOnly,
		KnowledgeBaseIDs:    req.KnowledgeBaseIDs,
		KnowledgeIDs:        req.KnowledgeIDs,
		TagScopes:           req.TagScopes,
		MCPServiceIDs:       req.MCPServiceIDs,
		SkillNames:          req.SkillNames,
		WebSearchEnabled:    req.WebSearchEnabled,
		QuestionOrigin:      req.QuestionOrigin,
		PageContext:         req.PageContext,
		AssistantAccess:     req.AssistantAccess,
		UserMessageID:       req.UserMessageID,
		Language:            types.LanguageFromContextOrDefault(ctx),
		AppInfo:             types.SanitizeLiveAppInfoJSON(req.AppInfo),
		LocalBrowserEnabled: req.LocalBrowserEnabled,
		RunNote:             req.RunNote,
		QuotedContext:       req.QuotedContext,
	}
	if req.CustomAgent != nil {
		r.AgentID, r.AgentTenantID = req.CustomAgent.ID, req.CustomAgent.TenantID
	}
	return r
}

// buildAgentConfig creates a runtime AgentConfig from the QARequest's custom agent configuration,
// tenant info, and resolved knowledge bases / search targets.
// hardIterationCap reads the workspace's absolute round ceiling for agents
// that declare unlimited iterations.
func hardIterationCap(cfg *appconfig.Config) int {
	if cfg == nil || cfg.Agent == nil {
		return 0 // AgentConfig.HardIterationLimit applies its default.
	}
	return cfg.Agent.HardIterationCap()
}

// Only explicit, standalone documentation definition/navigation requests qualify.
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

func (s *sessionService) buildAgentConfig(
	ctx context.Context,
	req *types.QARequest,
	tenantInfo *types.Tenant,
	agentTenantID uint64,
) (*types.AgentConfig, error) {
	if inherited, err := inheritedConfigFromBinding(ctx); err != nil {
		return nil, err
	} else if inherited != nil {
		return s.restoreInheritedConfig(ctx, inherited, tenantInfo)
	}
	customAgent := req.CustomAgent
	agentConfig := &types.AgentConfig{
		MaxIterations:                 customAgent.Config.MaxIterations,
		PlanningEnabled:               customAgent.Config.PlanningEnabled,
		Temperature:                   customAgent.Config.Temperature,
		WebSearchEnabled:              customAgent.Config.WebSearchEnabled && req.WebSearchEnabled,
		LocalBrowserEnabled:           req.LocalBrowserEnabled,
		WebSearchMaxResults:           customAgent.Config.WebSearchMaxResults,
		WebSearchProviderID:           customAgent.Config.WebSearchProviderID,
		MultiTurnEnabled:              customAgent.Config.MultiTurnEnabled,
		MemoryEnabled:                 customAgent.Config.MemoryEnabled,
		MCPSelectionMode:              customAgent.Config.MCPSelectionMode,
		MCPServices:                   customAgent.Config.MCPServices,
		MCPAuthWaitTimeout:            customAgent.Config.MCPAuthWaitTimeout,
		AttachmentParseWaitTimeoutSec: customAgent.Config.AttachmentParseWaitTimeoutSec,
		Thinking:                      customAgent.Config.Thinking,
		ReasoningEffort:               customAgent.Config.ReasoningEffort,
		CitationEnabled:               customAgent.Config.CitationEnabled,
		RetrieveKBOnlyWhenMentioned:   customAgent.Config.RetrieveKBOnlyWhenMentioned,
		FallbackStrategy:              customAgent.Config.FallbackStrategy,
		FallbackResponse:              customAgent.Config.FallbackResponse,
		FixedResponses:                customAgent.Config.FixedResponses,
		LLMCallTimeout:                customAgent.Config.LLMCallTimeout,
		MaxCompletionTokens:           customAgent.Config.MaxCompletionTokens,
		MaxToolOutputChars:            customAgent.Config.MaxToolOutputChars,
		EmbeddingTopK:                 customAgent.Config.EmbeddingTopK,
		RetainRetrievalHistory:        customAgent.Config.RetainRetrievalHistory,
		AppInfo:                       types.SanitizeLiveAppInfoJSON(req.AppInfo),
		SharedAgentReadOnly:           req.SharedAgentReadOnly,
		AssistantAccess:               types.CapAccessLevel(req.AssistantAccess, customAgent.Config.AccessLevel),
		// The model is always told it may call tools in parallel; without
		// this the engine still ran them one by one, so three searches in
		// one reply cost three sequential embed/retrieve/rerank rounds.
		// Only read-only tools overlap (agenttools.CanRunConcurrently);
		// anything else is a barrier that runs alone, in model order.
		ParallelToolCalls: true,
		// An agent may declare unlimited rounds (smart-reasoning). The absolute
		// ceiling keeps an unstable model that keeps changing its arguments from
		// running until the user cancels; 0 in config means the default.
		HardMaxIterations: hardIterationCap(s.cfg),
	}
	applyRequestReasoningEffort(req.ReasoningEffort, &agentConfig.Thinking, &agentConfig.ReasoningEffort)
	agentConfig.ResourceNavigationFastAnswer = eligibleResourceNavigation(req, agentConfig)

	// Falls back to global configuration if no specific timeout is set for the agent.
	if agentConfig.LLMCallTimeout == 0 && s.cfg.Agent != nil && s.cfg.Agent.LLMCallTimeout > 0 {
		agentConfig.LLMCallTimeout = s.cfg.Agent.LLMCallTimeout
	}

	// Configure skills based on CustomAgentConfig
	s.configureSkillsFromAgent(ctx, agentConfig, customAgent)

	// Then add the skills installed into the sandbox config this run boots.
	//
	// The workspace is the one on the context rather than the agent's owner,
	// because that is where resolveSandboxForExecution reads it; skillsForRun
	// picks the config the same way the sandbox resolution does.
	sandboxTenantID, _ := types.TenantIDFromContext(ctx)
	var (
		skillConfigID string
		tenantSkills  []*types.TenantSkillEntity
	)
	if s.hostDesktop {
		skillConfigID, tenantSkills = hostSkillsForRun(ctx, s.tenantSkillRepo, s.hostSkillTree, sandboxTenantID)
	} else {
		skillConfigID, tenantSkills = skillsForRun(
			ctx, s.sandboxPinner, s.sandboxConfigRepo, s.tenantSkillRepo,
			sandboxTenantID, req.Session.ID, agentConfig.SandboxConfigID,
		)
	}
	agentConfig.TenantSkills = tenantSkills
	if len(tenantSkills) > 0 {
		// The config named here is the one the skills came from, which is the
		// pinned one whenever it differs from the agent's - the only case the
		// line is worth reading.
		logger.Infof(ctx, "Sandbox config %s offers %d installed skill(s) to this run",
			skillConfigID, len(tenantSkills))
	}

	// Resolve knowledge bases using shared helper
	kbIDs, knowledgeIDs, err := s.resolveKnowledgeBases(ctx, req)
	if err != nil {
		return nil, err
	}
	agentConfig.KnowledgeBases = kbIDs
	agentConfig.KnowledgeIDs = knowledgeIDs

	// An unset list selects the default tools; an empty one means none.
	agentConfig.AllowedTools = tools.EffectiveAllowedTools(customAgent.Config.AllowedTools)
	// Apply per-turn @Skill / @MCP priority. Each helper records the authorized
	// mentioned items as the pinned set for the <must_use> hint without narrowing
	// the agent's configured scope.
	isSharedAgent := req.SharedAgentReadOnly
	applyPerRequestSkillScope(ctx, agentConfig, customAgent.Config.SkillsSelectionMode, req.SkillNames)
	applyPerRequestMCPScope(ctx, agentConfig, customAgent.Config.MCPServices, isSharedAgent, req.MCPServiceIDs)

	// Use custom agent's system prompt if specified
	if systemPrompt, _ := s.cfg.ResolveCustomAgentPrompts(customAgent); systemPrompt != "" {
		agentConfig.UseCustomSystemPrompt = true
		agentConfig.SystemPrompt = systemPrompt
	}

	logger.Infof(ctx, "Custom agent config applied: MaxIterations=%d, Temperature=%.2f, AllowedTools=%v, WebSearchEnabled=%v",
		agentConfig.MaxIterations, agentConfig.Temperature, agentConfig.AllowedTools, agentConfig.WebSearchEnabled)

	// Set web search max results from tenant config if not set (default: 5)
	if agentConfig.WebSearchMaxResults == 0 {
		agentConfig.WebSearchMaxResults = 5
		if tenantInfo.WebSearchConfig != nil && tenantInfo.WebSearchConfig.MaxResults > 0 {
			agentConfig.WebSearchMaxResults = tenantInfo.WebSearchConfig.MaxResults
		}
	}

	// Resolve web search provider ID: agent-level > tenant default (is_default=true)
	if agentConfig.WebSearchProviderID == "" {
		if defaultProvider, err := s.webSearchProviderRepo.GetDefault(ctx, tenantInfo.ID); err == nil && defaultProvider != nil {
			agentConfig.WebSearchProviderID = defaultProvider.ID
		}
	}

	logger.Infof(ctx, "Merged agent config from tenant %d and session %s", tenantInfo.ID, req.Session.ID)

	// Log knowledge bases if present
	if len(agentConfig.KnowledgeBases) > 0 || len(req.TagScopes) > 0 {
		if len(agentConfig.KnowledgeBases) > 0 {
			logger.Infof(ctx, "Agent configured with %d knowledge base(s): %v",
				len(agentConfig.KnowledgeBases), agentConfig.KnowledgeBases)
		} else {
			logger.Infof(ctx, "Agent configured with %d tag-scoped search target(s)", len(req.TagScopes))
		}
	} else {
		logger.Infof(ctx, "No knowledge bases specified for agent, running in pure agent mode")
	}

	// Build search targets using agent's tenant (handler has validated access for shared agent)
	searchTargets, err := s.buildSearchTargets(ctx, agentTenantID, agentConfig.KnowledgeBases, agentConfig.KnowledgeIDs, req.TagScopes)
	if err != nil {
		return nil, fmt.Errorf("build search targets: %w", err)
	}
	agentConfig.SearchTargets = searchTargets
	if !req.SharedAgentReadOnly {
		roleEnforced := s.cfg != nil && s.cfg.Tenant.IsRBACEnforced()
		agentConfig.WritableKBIDs = kbWritableIDs(ctx, s.kbShareService, searchTargets, roleEnforced)
		if types.GrantsOperationCatalog(req.CustomAgent) {
			agentConfig.OperationsAgentID = req.CustomAgent.ID
			agentConfig.AssistantAccess = tenantInfo.AssistantConfig.EffectiveAccessLevel(req.AssistantAccess)
			if cfg := tenantInfo.AssistantConfig; cfg != nil {
				agentConfig.AssistantName = strings.TrimSpace(cfg.DisplayName)
			}
		}
		if agentConfig.AssistantAccess == types.AssistantAccessReadOnly {
			// Wiki tools write outside the catalog; read-only covers them too.
			agentConfig.WritableKBIDs = nil
		}
	}
	agentConfig.QuestionOrigin = questionOriginInTargets(ctx, req.QuestionOrigin, searchTargets)
	agentConfig.PageContext = pageContextInTargets(ctx, req.PageContext, searchTargets)
	agentConfig.ProductDocs = types.GrantsProductDocs(req.CustomAgent)
	// Document tags are stored in knowledge_tag_relations, so document-KB tag
	// scopes are resolved to concrete knowledge IDs before retrieval. Preserve
	// those resolved IDs as this turn's pinned documents as well: otherwise the
	// Agent tools are correctly constrained behind the scenes, but the model only
	// sees a bound KB and does not know which documents the user explicitly chose.
	if len(req.TagScopes) > 0 {
		agentConfig.KnowledgeIDs = mergeResolvedTagKnowledgeIDs(
			agentConfig.KnowledgeIDs,
			searchTargets,
			req.TagScopes,
		)
	}
	logger.Infof(ctx, "Agent search targets built: %d targets", len(searchTargets))

	// MaxContextTokens is deliberately left unset here. The caller fills it
	// from the resolved model's declared window, which is not known yet.

	return agentConfig, nil
}

func mergeResolvedTagKnowledgeIDs(
	existing []string,
	searchTargets types.SearchTargets,
	tagScopes []types.TagScope,
) []string {
	tagKBs := make(map[string]bool, len(tagScopes))
	for _, scope := range tagScopes {
		if scope.KnowledgeBaseID != "" && len(scope.TagIDs) > 0 {
			tagKBs[scope.KnowledgeBaseID] = true
		}
	}
	if len(tagKBs) == 0 {
		return uniqueNonEmptyStrings(existing)
	}

	merged := append([]string(nil), existing...)
	for _, target := range searchTargets {
		if target == nil || !tagKBs[target.KnowledgeBaseID] || target.Type != types.SearchTargetTypeKnowledge {
			continue
		}
		merged = append(merged, target.KnowledgeIDs...)
	}
	return uniqueNonEmptyStrings(merged)
}

// applyPerRequestSkillScope records the @Skill mentions for this turn as the
// pinned set that drives the <must_use> hint. It deliberately does NOT narrow
// the allow-gate: an agent whose prompt requires a skill the user did not
// @mention must still be able to read and execute it. Mentioning a skill only
// prioritizes it, it never revokes access to the agent's configured set.
//
// It is a no-op when no skills were mentioned or skills are disabled.
func applyPerRequestSkillScope(
	ctx context.Context,
	agentConfig *types.AgentConfig,
	skillsMode string,
	requested []string,
) {
	if len(requested) == 0 {
		return
	}
	if skillsMode == "none" || skillsMode == "" {
		logger.Warnf(ctx, "Ignoring @skill mention: agent skills selection is disabled (mode=%s)", skillsMode)
		return
	}
	if !agentConfig.SkillsEnabled {
		return
	}
	// PinnedSkillNames carries only mentioned skills that are currently
	// allowed, so the <must_use> hint never directs the model at a skill it
	// cannot load. An empty AllowedSkills means all skills are allowed,
	// matching Manager.isSkillAllowed, so every mention is pinned in that case.
	agentConfig.PinnedSkillNames = pinPreservingRequestOrder(requested, agentConfig.AllowedSkills)
	logger.Infof(ctx, "Applied per-request @skill scope: requested=%v effective=%v pinned=%v",
		requested, agentConfig.AllowedSkills, agentConfig.PinnedSkillNames)
}

// applyPerRequestMCPScope pins authorized @MCP mentions for the <must_use> hint,
// preserving access to the rest of the agent's configured services. It is a
// no-op when no services were mentioned or MCP selection is disabled.
func applyPerRequestMCPScope(
	ctx context.Context,
	agentConfig *types.AgentConfig,
	agentPresetMCPs []string,
	isSharedAgent bool,
	requested []string,
) {
	if len(requested) == 0 {
		return
	}
	if agentConfig.MCPSelectionMode == "" || agentConfig.MCPSelectionMode == "none" {
		logger.Warnf(ctx, "Ignoring @MCP mention: agent MCP selection is disabled (mode=%q)", agentConfig.MCPSelectionMode)
		return
	}
	mentioned := dedupPreservingOrder(requested)
	effective := resolvePerRequestMCPScope(
		mentioned, agentPresetMCPs, agentConfig.MCPSelectionMode, isSharedAgent,
	)
	if len(effective) == 0 {
		logger.Warnf(ctx, "Ignoring @MCP scope outside agent preset: requested=%v agent=%v shared=%v",
			requested, agentPresetMCPs, isSharedAgent)
		return
	}
	agentConfig.PinnedMCPServiceIDs = effective
	logger.Infof(ctx, "Applied per-request @MCP priority: requested=%v mode=%s pinned=%v",
		requested, agentConfig.MCPSelectionMode, effective)
}

// resolvePerRequestMCPScope selects authorized mentions for per-turn priority.
// It does not modify the registration scope. Shared agents may only pin services
// in the agent preset, and selectionMode "none" rejects all mentions.
func resolvePerRequestMCPScope(
	mentioned, agentMCPs []string,
	selectionMode string,
	isSharedAgent bool,
) []string {
	if len(mentioned) == 0 {
		return nil
	}
	if isSharedAgent {
		mentioned = intersectPreservingRequestOrder(mentioned, agentMCPs)
		if len(mentioned) == 0 {
			return nil
		}
	}
	var effective []string
	switch selectionMode {
	case "none", "":
		return nil
	case "selected":
		effective = intersectPreservingRequestOrder(mentioned, agentMCPs)
	case "all":
		effective = mentioned
	default:
		return nil
	}
	if len(effective) == 0 {
		return nil
	}
	return effective
}

func intersectPreservingRequestOrder(requested []string, allowed []string) []string {
	allowedSet := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		if value != "" {
			allowedSet[value] = true
		}
	}
	result := make([]string, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, value := range requested {
		if value == "" || seen[value] || !allowedSet[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// pinPreservingRequestOrder returns the requested skills that are allowed,
// preserving request order. Unlike intersectPreservingRequestOrder, an empty
// allowed list is treated as "all skills allowed" (matching
// Manager.isSkillAllowed), so every requested skill is pinned.
func pinPreservingRequestOrder(requested []string, allowed []string) []string {
	allowedAll := len(allowed) == 0
	allowedSet := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		if value != "" {
			allowedSet[value] = true
		}
	}
	result := make([]string, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, value := range requested {
		if value == "" || seen[value] {
			continue
		}
		if !allowedAll && !allowedSet[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func dedupPreservingOrder(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// configureSkillsFromAgent turns the agent's skill picker into runtime flags.
// The skills themselves come from the sandbox image (TenantSkills), not from
// a host skill directory — that copy is not what shell_exec would find
// inside the sandbox.
func (s *sessionService) configureSkillsFromAgent(
	ctx context.Context,
	agentConfig *types.AgentConfig,
	customAgent *types.CustomAgent,
) {
	if customAgent == nil {
		return
	}
	agentConfig.SandboxConfigID = customAgent.Config.SandboxConfigID
	switch customAgent.Config.SkillsSelectionMode {
	case "all":
		agentConfig.SkillsEnabled = true
		agentConfig.AllowedSkills = nil
		logger.Infof(ctx, "SkillsSelectionMode=all: using installed sandbox skills")
	case "selected":
		if len(customAgent.Config.SelectedSkills) > 0 {
			agentConfig.SkillsEnabled = true
			agentConfig.AllowedSkills = customAgent.Config.SelectedSkills
			logger.Infof(ctx, "SkillsSelectionMode=selected: enabled %d selected skills: %v",
				len(customAgent.Config.SelectedSkills), customAgent.Config.SelectedSkills)
		} else {
			agentConfig.SkillsEnabled = false
			logger.Infof(ctx, "SkillsSelectionMode=selected but no skills selected: skills disabled")
		}
	case "none", "":
		agentConfig.SkillsEnabled = false
		logger.Infof(ctx, "SkillsSelectionMode=%s: skills disabled", customAgent.Config.SkillsSelectionMode)
	default:
		agentConfig.SkillsEnabled = false
		logger.Warnf(ctx, "Unknown SkillsSelectionMode=%s: skills disabled", customAgent.Config.SkillsSelectionMode)
	}
}

// questionOriginInTargets keeps a suggested question's origin only when this
// turn's search targets reach it, so the hint never points the model at
// anything the tools cannot read. The base must be searched this turn (a whole
// base, or a document/tag scope inside it); the document is kept only when a
// target for that base covers it.
func questionOriginInTargets(
	ctx context.Context, origin *types.QuestionOrigin, targets types.SearchTargets,
) *types.QuestionOrigin {
	if origin == nil {
		return nil
	}
	kbID := strings.TrimSpace(origin.KnowledgeBaseID)
	if kbID == "" {
		return nil
	}
	if !targets.ContainsKB(kbID) {
		logger.Infof(ctx, "Ignoring question origin: knowledge base %s is outside this turn's search targets",
			secutils.SanitizeForLog(kbID))
		return nil
	}
	kept := &types.QuestionOrigin{KnowledgeBaseID: kbID}
	if docID := strings.TrimSpace(origin.KnowledgeID); docID != "" && targetsCoverDocument(targets, kbID, docID) {
		kept.KnowledgeID = docID
	}
	return kept
}

// pageContextInTargets keeps the page the user has open only when this turn's
// search targets reach its knowledge base, and its document only when a
// target covers it. The Wiki slug and issue IDs are kept as given (trimmed,
// deduplicated and bounded); the agent service checks they exist in that base
// before showing them to the model. Resources outside knowledge bases (agent,
// model, MCP service, chat message) and a known page identifier are kept as
// given; the agent service verifies them.
func pageContextInTargets(
	ctx context.Context, page *types.PageContext, targets types.SearchTargets,
) *types.PageContext {
	if page == nil {
		return nil
	}
	kept := &types.PageContext{
		AgentID:      strings.TrimSpace(page.AgentID),
		ModelID:      strings.TrimSpace(page.ModelID),
		MCPServiceID: strings.TrimSpace(page.MCPServiceID),
		Title:        types.NormalizePageView(page.Title),
		View:         types.NormalizePageView(page.View),
		Selection:    page.Selection.Normalize(),
	}
	if p := strings.TrimSpace(page.Page); types.IsPageContextPage(p) {
		kept.Page = p
		if section := strings.TrimSpace(page.Section); types.IsPageContextSection(section) {
			kept.Section = section
		}
	}
	if sessionID, messageID := strings.TrimSpace(page.SessionID), strings.TrimSpace(page.MessageID); sessionID != "" {
		kept.SessionID = sessionID
		kept.MessageID = messageID
	}
	kbID := strings.TrimSpace(page.KnowledgeBaseID)
	if kbID == "" || !targets.ContainsKB(kbID) {
		if kbID != "" {
			logger.Infof(ctx, "Ignoring page context: knowledge base %s is outside this turn's search targets",
				secutils.SanitizeForLog(kbID))
		}
		if kept.Page == "" && kept.AgentID == "" && kept.ModelID == "" &&
			kept.MCPServiceID == "" && kept.SessionID == "" && kept.Title == "" && kept.View == "" &&
			kept.Selection == nil {
			return nil
		}
		return kept
	}
	kept.KnowledgeBaseID = kbID
	kept.WikiSlug = strings.TrimSpace(page.WikiSlug)
	if docID := strings.TrimSpace(page.KnowledgeID); docID != "" && targetsCoverDocument(targets, kbID, docID) {
		kept.KnowledgeID = docID
	}
	seen := make(map[string]struct{}, len(page.WikiIssueIDs))
	for _, id := range page.WikiIssueIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if len(kept.WikiIssueIDs) == types.MaxPageContextIssues {
			break
		}
		seen[id] = struct{}{}
		kept.WikiIssueIDs = append(kept.WikiIssueIDs, id)
	}
	return kept
}

// targetsCoverDocument reports whether a target for kbID can read docID: an
// unfiltered whole-base target, or a document scope that lists it. A
// tag-filtered base cannot be checked per document here, so it does not count.
func targetsCoverDocument(targets types.SearchTargets, kbID, docID string) bool {
	for _, t := range targets {
		if t == nil || t.KnowledgeBaseID != kbID {
			continue
		}
		switch t.Type {
		case types.SearchTargetTypeKnowledgeBase:
			if len(t.TagIDs) == 0 {
				return true
			}
		case types.SearchTargetTypeKnowledge:
			if slices.Contains(t.KnowledgeIDs, docID) {
				return true
			}
		}
	}
	return false
}
