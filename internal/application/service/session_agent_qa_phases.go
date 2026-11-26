package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/collaboration"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
)

type agentTurnSetup struct {
	sessionID                string
	agentConfig              *types.AgentConfig
	effectiveModelID         string
	modelCtx                 context.Context
	summaryModel             chat.Chat
	agentModelSupportsVision bool
	rerankModel              rerank.Reranker
	llmContext               []chat.Message
}

type agentFAQPreflight struct {
	cfg        *types.AgentConfig
	done       chan agentFAQOutcome
	cancel     context.CancelFunc
	pending    bool
	evaluation *agentFAQEvaluation
}

func (p *agentFAQPreflight) stop() {
	if p != nil && p.cancel != nil {
		p.cancel()
	}
}

func (p *agentFAQPreflight) finish(
	qaCtx context.Context,
	prepCtx context.Context,
	req *types.QARequest,
	eventBus *event.EventBus,
) (answered, withdrawn bool, err error) {
	outcome := <-p.done
	p.evaluation = outcome.evaluation
	if outcome.err != nil {
		return false, false, outcome.err
	}
	answered, err = emitAgentFAQEvaluation(qaCtx, req, eventBus, outcome.evaluation)
	withdrawn = !answered && err == nil && prepCtx.Err() != nil && qaCtx.Err() == nil
	return answered, withdrawn, err
}

func (s *sessionService) prepareAgentTurnSetup(
	ctx context.Context,
	req *types.QARequest,
) (context.Context, *agentTurnSetup, error) {
	sessionID := req.Session.ID
	if req.Session.Kind == types.SessionKindAgentDelegated {
		b := collaboration.FromContext(ctx)
		if b == nil || b.Child == nil || b.Child.Member.SessionID != sessionID {
			return ctx, nil, errors.New("delegated conversations require a scheduler-owned execution")
		}
		if err := b.Runtime.CheckOwner(ctx, *b.Child); err != nil {
			return ctx, nil, err
		}
	}
	ctx = types.WithSessionID(ctx, sessionID)
	ctx = types.WithRunMode(ctx, req.Mode)
	if req.CustomAgent == nil {
		logger.Warnf(ctx, "Custom agent not provided for session: %s", sessionID)
		return ctx, nil, errors.New("custom agent configuration is required for agent QA")
	}

	agentTenantID := s.resolveRetrievalTenantID(ctx, req)
	logger.Infof(ctx, "Start agent-based question answering, session ID: %s, agent tenant ID: %d, query length: %d",
		sessionID, agentTenantID, len([]rune(req.Query)))

	tenantInfo := s.resolveAgentTenantInfo(ctx, agentTenantID)
	req.CustomAgent.EnsureDefaults()

	agentConfig, err := s.buildAgentConfig(ctx, req, tenantInfo, agentTenantID)
	if err != nil {
		return ctx, nil, err
	}
	if delegated, err := delegatedConfigFromBinding(ctx); err != nil {
		return ctx, nil, err
	} else if delegated != nil {
		agentConfig.AssistantAccess = delegated.level(tenantInfo)
		if agentConfig.AssistantAccess == types.AssistantAccessReadOnly {
			agentConfig.WritableKBIDs = nil
		}
	}
	ctx = types.WithAccessLevel(ctx, agentConfig.AssistantAccess)
	if _, scoped := types.GrantScopeFromContext(ctx); !scoped {
		grantSessionID := req.GrantSessionID
		if grantSessionID == "" {
			grantSessionID = sessionID
		}
		ctx = types.WithGrantScope(ctx, types.GrantScope{Scope: types.AgentGrantScopeSession, ScopeID: grantSessionID})
	}
	if req.CustomAgent != nil && req.CustomAgent.Config.VLMModelID != "" {
		agentConfig.VLMModelID = req.CustomAgent.Config.VLMModelID
	}

	effectiveModelID, modelCtx, summaryModel, supportsVision, err := s.resolveAgentChatModel(ctx, req, agentConfig)
	if err != nil {
		return ctx, nil, err
	}
	rerankModel := s.resolveAgentRerankModel(ctx, req)
	llmContext, err := s.loadAgentTurnHistory(ctx, sessionID, agentConfig)
	if err != nil {
		return ctx, nil, err
	}

	return ctx, &agentTurnSetup{
		sessionID:                sessionID,
		agentConfig:              agentConfig,
		effectiveModelID:         effectiveModelID,
		modelCtx:                 modelCtx,
		summaryModel:             summaryModel,
		agentModelSupportsVision: supportsVision,
		rerankModel:              rerankModel,
		llmContext:               llmContext,
	}, nil
}

func (s *sessionService) resolveAgentTenantInfo(ctx context.Context, agentTenantID uint64) *types.Tenant {
	var tenantInfo *types.Tenant
	if v := ctx.Value(types.TenantInfoContextKey); v != nil {
		tenantInfo, _ = v.(*types.Tenant)
	}
	if tenantInfo == nil || tenantInfo.ID != agentTenantID {
		if s.tenantService != nil {
			if agentTenant, err := s.tenantService.GetTenantByID(ctx, agentTenantID); err == nil && agentTenant != nil {
				tenantInfo = agentTenant
				logger.Infof(ctx, "Using agent tenant info for retrieval scope, tenant ID: %d", agentTenantID)
			}
		}
	}
	if tenantInfo == nil {
		logger.Warnf(ctx, "Tenant info not available for agent tenant %d, proceeding with defaults", agentTenantID)
		tenantInfo = &types.Tenant{ID: agentTenantID}
	}
	return tenantInfo
}

func (s *sessionService) resolveAgentChatModel(
	ctx context.Context,
	req *types.QARequest,
	agentConfig *types.AgentConfig,
) (string, context.Context, chat.Chat, bool, error) {
	effectiveModelID, modelTenantID, err := s.resolveChatModelID(
		ctx, req, agentConfig.KnowledgeBases, agentConfig.KnowledgeIDs,
	)
	if err != nil {
		return "", nil, nil, false, err
	}
	if effectiveModelID == "" {
		logger.Warnf(ctx, "No summary model configured for custom agent %s", req.CustomAgent.ID)
		return "", nil, nil, false, errors.New("summary model (model_id) is not configured in custom agent settings")
	}

	modelCtx := types.WithExecutionTenant(ctx, modelTenantID)
	summaryModel, err := s.modelService.GetChatModel(modelCtx, effectiveModelID)
	if err != nil {
		logger.Warnf(ctx, "Failed to get chat model: %v", err)
		return "", nil, nil, false, fmt.Errorf("failed to get chat model: %w", err)
	}

	var supportsVision bool
	modelContextWindow := 0
	if effectiveModelID != "" {
		if modelInfo, err := s.modelService.GetModelByID(modelCtx, effectiveModelID); err == nil && modelInfo != nil {
			supportsVision = modelInfo.Parameters.SupportsVision
			modelContextWindow = modelInfo.Parameters.ContextWindow
			agentConfig.ModelMaxOutputTokens = modelMaxOutputTokens(modelInfo)
		}
	}
	agentConfig.ChatModelSupportsVision = supportsVision
	agentConfig.MaxContextTokens = types.AgentMaxContextTokens(
		agentConfig.MaxContextTokens, modelContextWindow,
	)
	logger.Infof(ctx, "Agent context window: %d tokens (model %s declares %d)",
		agentConfig.MaxContextTokens, effectiveModelID, modelContextWindow)
	return effectiveModelID, modelCtx, summaryModel, supportsVision, nil
}

func (s *sessionService) resolveAgentRerankModel(ctx context.Context, req *types.QARequest) rerank.Reranker {
	if !agentRequiresRerankModel(req.CustomAgent) {
		logger.Infof(ctx, "knowledge_search is unavailable for the effective agent scope, skipping rerank model initialization")
		return nil
	}
	rerankModelID := req.CustomAgent.Config.RerankModelID
	if rerankModelID == "" && types.UsesWorkspaceModelFallback(req.CustomAgent) {
		models, listErr := s.modelService.ListModels(ctx)
		if listErr != nil {
			logger.Warnf(ctx, "Failed to list models for internal agent %s rerank fallback: %v",
				req.CustomAgent.ID, listErr)
		}
		rerankModelID = pickWorkspaceRerankModelID(models)
		if rerankModelID == "" {
			logger.Infof(ctx, "No rerank model in the workspace, internal agent %s searches without reranking",
				req.CustomAgent.ID)
		}
	} else if rerankModelID == "" {
		logger.Warnf(ctx, "No rerank model configured for custom agent %s; knowledge_search will keep retrieval order",
			req.CustomAgent.ID)
	}
	if rerankModelID == "" {
		return nil
	}
	model, getErr := s.modelService.GetRerankModel(ctx, rerankModelID)
	if getErr != nil {
		logger.Warnf(ctx, "Failed to get rerank model %s, searching without reranking: %v", rerankModelID, getErr)
		return nil
	}
	return model
}

func (s *sessionService) loadAgentTurnHistory(
	ctx context.Context,
	sessionID string,
	agentConfig *types.AgentConfig,
) ([]chat.Message, error) {
	if !agentConfig.MultiTurnEnabled {
		logger.Infof(ctx, "Multi-turn disabled for this agent, running without history")
		return []chat.Message{}, nil
	}
	budget := agent.HistoryTokenBudget(agentConfig)
	var (
		llmContext []chat.Message
		err        error
	)
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		llmContext, agentConfig.ContextTokenScale, err = LoadAgentHistory(
			ctx, s.messageRepo, sessionID, budget, agentConfig.RetainRetrievalHistory,
		)
		if err == nil {
			break
		}
		logger.Warnf(ctx, "Failed to load agent history (attempt %d/2): %v", attempt+1, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load conversation history; please retry: %w", err)
	}
	logger.Infof(ctx, "Loaded %d history messages from DB (budget=%d tokens, token scale=%.2f)",
		len(llmContext), budget, agentConfig.ContextTokenScale)
	return llmContext, nil
}

func (s *sessionService) startAgentFAQPreflight(
	qaCtx context.Context,
	cancelPrep context.CancelFunc,
	req *types.QARequest,
	setup *agentTurnSetup,
	baseAgentConfig *types.AgentConfig,
	eventBus *event.EventBus,
) (chat.Chat, *agentFAQPreflight) {
	faqEligible := faqDirectEligible(req, setup.agentConfig, eventBus) && setup.agentConfig.SearchTargets.MayContainFAQ()
	steerFAQEligible := false
	if req.SteerSink != nil && setup.agentConfig.SearchTargets.MayContainFAQ() {
		followup := *req
		followup.Resume = nil
		steerFAQEligible = faqDirectEligible(&followup, setup.agentConfig, eventBus)
	}
	var faqModel chat.Chat
	if faqEligible || steerFAQEligible {
		faqModel = s.resolveFAQPreflightModel(qaCtx, setup.modelCtx, req, setup.summaryModel)
	}
	preflight := &agentFAQPreflight{cancel: func() {}}
	if !faqEligible || faqModel == nil {
		return faqModel, preflight
	}
	faqCfg := cloneAgentConfigForRun(baseAgentConfig)
	faqCfg.FAQPreflightUsage, faqCfg.FAQPreflightChecked, faqCfg.FAQPreflightQuestions, faqCfg.FAQPreflightRejections = nil, false, nil, nil
	faqCtx, cancelFAQ := context.WithCancel(qaCtx)
	faqDone := make(chan agentFAQOutcome, 1)
	go func() {
		var outcome agentFAQOutcome
		defer func() { faqDone <- outcome }()
		defer tools.RecoverGoroutine(faqCtx, "Agent FAQ preflight", func() { outcome = agentFAQOutcome{} })
		outcome.evaluation, outcome.err = s.preflightAgentFAQ(
			faqCtx, req, faqCfg, eventBus, faqModel, setup.rerankModel, setup.llmContext,
		)
		if outcome.evaluation != nil && outcome.evaluation.reply().Answer != "" {
			cancelPrep()
		}
	}()
	preflight.cfg = faqCfg
	preflight.done = faqDone
	preflight.cancel = cancelFAQ
	preflight.pending = true
	return faqModel, preflight
}

func (s *sessionService) prepareAgentTurnEngine(
	pCtx context.Context,
	baseQACtx context.Context,
	req *types.QARequest,
	setup *agentTurnSetup,
	baseAgentConfig *types.AgentConfig,
	stager sessionAttachmentStager,
	eventBus *event.EventBus,
	faqModel chat.Chat,
	allowDeferStaging bool,
) (*agentTurnPrep, error) {
	sessionID := setup.sessionID
	p := &agentTurnPrep{
		config: cloneAgentConfigForRun(baseAgentConfig),
		qaCtx:  baseQACtx,
	}

	stagedAttachments, err := s.stageAgentTurnAttachments(pCtx, req, sessionID, stager, p, allowDeferStaging)
	if err != nil {
		return p, err
	}

	pin, err := sandboxConfigForExistingSandbox(pCtx, s.sandboxPinner, sessionID)
	if err != nil {
		return p, fmt.Errorf("read sandbox scope for child tasks: %w", err)
	}
	childSrc := childSource{
		Agent: req.CustomAgent, ModelID: setup.effectiveModelID, SandboxConfigID: pin.ConfigID,
	}
	p.qaCtx = context.WithValue(baseQACtx, childSourceKey{}, childSrc)
	pCtx = context.WithValue(pCtx, childSourceKey{}, childSrc)

	logger.Info(pCtx, "Creating agent engine")
	p.engine, err = s.agentService.CreateAgentEngine(
		pCtx,
		p.config,
		setup.summaryModel,
		setup.rerankModel,
		eventBus,
		sessionID,
		req.AssistantMessageID,
	)
	if err != nil {
		logger.Errorf(pCtx, "Failed to create agent engine: %v", err)
		return p, err
	}
	if err := s.configureAgentTurnEngine(pCtx, req, setup, p, eventBus, faqModel); err != nil {
		return p, err
	}
	if req.Resume == nil {
		composeAgentTurnQuery(pCtx, req, setup.agentModelSupportsVision, p, stagedAttachments)
	}
	return p, nil
}

func (s *sessionService) stageAgentTurnAttachments(
	pCtx context.Context,
	req *types.QARequest,
	sessionID string,
	stager sessionAttachmentStager,
	p *agentTurnPrep,
	allowDeferStaging bool,
) ([]stagedSessionAttachment, error) {
	inputStore, storeErr := stager.sessionSandboxInputStore(pCtx, sessionID, p.config.SandboxConfigID)
	if storeErr != nil {
		return nil, fmt.Errorf("resolve sandbox file store for session %s: %w", sessionID, storeErr)
	}
	mgr, _, layoutErr := resolveSandboxForExecution(
		pCtx, s.sandboxResolver, s.sandboxMgr, s.sandboxPinner,
		req.Session.TenantID, sessionID, p.config.SandboxConfigID, s.sandboxPolicy,
		withLiteHostSandbox(s.hostSandbox), withLiteDesktop(s.hostDesktop),
	)
	p.layout = sessionWorkspaceLayout(
		pCtx, sessionID, mgr, layoutErr, s.hostSandbox, p.config.SandboxConfigID,
	)
	if inputStore == nil || strings.TrimSpace(p.layout.InputDir) == "" {
		return nil, nil
	}
	sessionAttachments, loadErr := s.messageRepo.GetSessionAttachments(pCtx, sessionID)
	if loadErr != nil {
		return nil, fmt.Errorf("load session attachments for sandbox staging: %w", loadErr)
	}
	if allowDeferStaging && len(sessionAttachments) == 0 {
		p.deferEmptyStaging = true
		return nil, nil
	}
	stagedAttachments, err := stager.stageSessionAttachments(
		pCtx, sessionID, p.config.SandboxConfigID, req.Session.TenantID, sessionAttachments, p.layout,
	)
	if err != nil {
		return nil, fmt.Errorf("restore session attachments into sandbox: %w", err)
	}
	for _, staged := range stagedAttachments {
		if staged.AttachmentPath == "" {
			continue
		}
		if p.config.AttachmentSandboxCopies == nil {
			p.config.AttachmentSandboxCopies = make(map[string]string, len(stagedAttachments))
		}
		p.config.AttachmentSandboxCopies[staged.Path] = staged.AttachmentPath
	}
	return stagedAttachments, nil
}

func (s *sessionService) configureAgentTurnEngine(
	pCtx context.Context,
	req *types.QARequest,
	setup *agentTurnSetup,
	p *agentTurnPrep,
	eventBus *event.EventBus,
	faqModel chat.Chat,
) error {
	if receiver, ok := p.engine.(interface{ SetRetrievalUserMessage(string, string) }); ok {
		receiver.SetRetrievalUserMessage(req.UserMessageID, req.Query)
	}
	if faqModel != nil {
		if receiver, ok := p.engine.(interface{ SetFastChatModel(chat.Chat) }); ok {
			receiver.SetFastChatModel(faqModel)
		}
	}
	if err := s.bindAgentRetrievalState(pCtx, req, p.config, p.engine); err != nil {
		return err
	}

	memoryCtx := types.ApplyAgentMemoryPreference(pCtx, p.config.MemoryEnabled)
	if s.memoryService != nil {
		p.recall = s.memoryService.Recall(memoryCtx, req.Query)
	}

	if req.SteerSink != nil {
		p.engine.SetSteerSink(req.SteerSink)
		if receiver, ok := p.engine.(interface{ SetSteerFAQ(agent.SteerFAQFunc) }); ok {
			receiver.SetSteerFAQ(s.steerFAQ(req, p.config, eventBus, faqModel, setup.rerankModel, setup.llmContext))
		}
	}

	p.engine.SetRunMode(req.Mode)
	if req.RunNote != "" {
		if noted, ok := p.engine.(interface{ SetRunNote(string) }); ok {
			noted.SetRunNote(req.RunNote)
		}
	}
	collaborating := false
	if bound, ok := p.engine.(interface{ HasCollaboration() bool }); ok {
		collaborating = bound.HasCollaboration()
	}
	promptSuspend := s.cfg != nil && s.cfg.Agent.PromptSuspend()
	if req.Suspendable && (promptSuspend || collaborating) {
		if request, err := json.Marshal(resumableAgentRequest(p.qaCtx, req)); err != nil {
			logger.Warnf(p.qaCtx, "Run cannot be suspended, encoding its request failed: %v", err)
		} else {
			p.engine.EnableSuspension(request)
			if b := collaboration.FromContext(p.qaCtx); !promptSuspend && (b == nil || b.Child == nil) {
				if restricted, ok := p.engine.(interface{ RestrictSuspensionToCollaboration() }); ok {
					restricted.RestrictSuspensionToCollaboration()
				}
			}
		}
	}

	if p.config.MultiTurnEnabled {
		p.engine.SetContextCheckpointSink(messageCheckpointSink{repo: s.messageRepo, sessionID: setup.sessionID})
	}
	return nil
}

func composeAgentTurnQuery(
	pCtx context.Context,
	req *types.QARequest,
	supportsVision bool,
	p *agentTurnPrep,
	stagedAttachments []stagedSessionAttachment,
) {
	p.agentQuery, p.agentImageURLs = composeAgentImageQuery(
		pCtx, req.Query, req.ImageDescription, supportsVision, req.ImageURLs,
		agentImageDescriber(p.engine),
	)
	if supportsVision && len(p.agentImageURLs) > 0 {
		logger.Infof(pCtx, "Agent model supports vision, passing %d image(s) directly", len(p.agentImageURLs))
	} else if len(req.ImageURLs) > 0 && strings.Contains(p.agentQuery, "[用户上传图片内容]") {
		logger.Infof(pCtx, "Agent model does not support vision, appending image description")
	} else if len(req.ImageURLs) > 0 {
		logger.Warnf(pCtx, "Agent model cannot view %d image(s) and no description was produced", len(req.ImageURLs))
	}
	if req.QuotedContext != "" {
		p.agentQuery += "\n\n" + req.QuotedContext
	}
	if len(req.Attachments) > 0 {
		p.agentQuery += req.Attachments.BuildAgentPrompt()
		logger.Infof(pCtx, "Appended %d attachment(s) to agent query", len(req.Attachments))
	}
	if manifest := buildSandboxAttachmentsPrompt(stagedAttachments, p.layout); manifest != "" {
		p.agentQuery += manifest
		logger.Infof(pCtx, "Appended %d staged sandbox attachment path(s) to agent query", len(stagedAttachments))
	}
}

func (s *sessionService) executePreparedAgentTurn(
	qaCtx context.Context,
	req *types.QARequest,
	setup *agentTurnSetup,
	prep *agentTurnPrep,
	stager sessionAttachmentStager,
	eventBus *event.EventBus,
	onStart func(),
) error {
	sessionID := setup.sessionID
	qaCtx = prep.qaCtx
	if len(prep.config.FAQPreflightQuestions) > 0 {
		if receiver, ok := prep.engine.(interface{ SetFAQPreflightQuestions([]string) }); ok {
			receiver.SetFAQPreflightQuestions(prep.config.FAQPreflightQuestions)
		}
	}
	if prep.deferEmptyStaging {
		if _, err := stager.stageSessionAttachments(
			qaCtx, sessionID, prep.config.SandboxConfigID, req.Session.TenantID, nil, prep.layout,
		); err != nil {
			return fmt.Errorf("restore session attachments into sandbox: %w", err)
		}
	}
	if prep.recall.Prompt != "" {
		prep.engine.SetMemoryPrompt(prep.recall.Prompt)
		used := types.UsedMemoriesFromItems(prep.recall.Items)
		if err := eventBus.Emit(qaCtx, event.Event{
			Type:      event.EventMemoryRecalled,
			SessionID: sessionID,
			Data:      event.MemoryRecalledData{Memories: used},
		}); err != nil {
			logger.Warnf(qaCtx, "Failed to emit memory recalled event: %v", err)
		}
		logger.Infof(qaCtx, "Injected %d long-term memories into agent context", len(used))
	}

	logger.Info(qaCtx, "Executing agent with streaming")
	if err := qaCtx.Err(); err != nil {
		return err
	}
	onStart()
	var err error
	if req.Resume != nil {
		_, err = prep.engine.Resume(qaCtx, sessionID, req.AssistantMessageID, setup.llmContext,
			req.Resume.Snapshot, req.Resume.Prompt)
	} else {
		_, err = prep.engine.Execute(qaCtx, sessionID, req.AssistantMessageID, prep.agentQuery, setup.llmContext, prep.agentImageURLs)
	}
	if err != nil {
		logger.Errorf(qaCtx, "Agent execution failed: %v", err)
	}
	return nil
}
