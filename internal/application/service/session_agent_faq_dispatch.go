package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
)

func (s *sessionService) agentFAQResolver(req *types.QARequest, cfg *types.AgentConfig, model chat.Chat, reranker rerank.Reranker, history []chat.Message) agent.FAQResolver {
	return func(ctx context.Context, targets types.SearchTargets) (*agent.FAQResolution, error) {
		if err := faqEvaluationInterrupted(ctx, req); err != nil {
			return nil, err
		}
		kbs, err := s.knowledgeBaseService.GetKnowledgeBasesByIDsOnly(ctx, targets.GetAllKnowledgeBaseIDs())
		if err != nil {
			return nil, err
		}
		byID := make(map[string]*types.KnowledgeBase, len(kbs))
		for _, kb := range kbs {
			if kb != nil {
				byID[kb.ID] = kb
			}
		}
		var faqTargets, documents types.SearchTargets
		for _, target := range targets {
			if target == nil {
				continue
			}
			kb := byID[target.KnowledgeBaseID]
			if kb == nil {
				return nil, fmt.Errorf("knowledge base metadata is unavailable for FAQ dispatch")
			}
			if kb.Type == types.KnowledgeBaseTypeFAQ {
				faqTargets = append(faqTargets, target)
			} else {
				documents = append(documents, target)
			}
		}
		if len(faqTargets) == 0 {
			return nil, nil
		}
		local := *cfg
		local.SearchTargets, local.FAQPreflightUsage = faqTargets.Clone(), nil
		evaluation := s.evaluateAgentFAQ(ctx, req, &local, model, reranker, history)
		search := tools.NewSearchKnowledgeTool(s.knowledgeBaseService, s.knowledgeService, s.chunkService, faqTargets, nil, s.cfg)
		resolution, err := faqDispatchResolution(ctx, search, evaluation, documents)
		return resolution, errors.Join(err, faqEvaluationInterrupted(ctx, req))
	}
}

func faqDispatchResolution(ctx context.Context, search *tools.SearchKnowledgeTool, evaluation *agentFAQEvaluation, documents types.SearchTargets) (*agent.FAQResolution, error) {
	var candidates []*types.SearchResult
	seen := make(map[string]bool)
	completed := false
	var failures []types.RetrievalFailure
	var notes []string
	var observations []map[string]any
	for _, observation := range evaluation.Observations {
		completed = completed || len(observation.CompletedTargets) > 0
		for _, candidate := range observation.Candidates {
			if candidate != nil && !seen[candidate.KnowledgeBaseID+"/"+candidate.ID] {
				seen[candidate.KnowledgeBaseID+"/"+candidate.ID] = true
				candidates = append(candidates, candidate)
			}
		}
		for _, failure := range observation.Failures {
			failure.Targets = failure.Targets.Clone()
			failure.KnowledgeBaseIDs = slices.Clone(failure.KnowledgeBaseIDs)
			// Generic document recovery must not replay completed FAQ stages.
			if !strings.HasPrefix(failure.Stage, "faq_") {
				failure.Stage = observation.Stage + "/" + failure.Stage
			}
			failures = append(failures, failure)
		}
		record := map[string]any{"stage": observation.Stage, "query": observation.Query,
			"requested_targets": observation.RequestedTargets, "completed_targets": observation.CompletedTargets,
			"skipped_targets": observation.SkippedTargets, "cause_stage": observation.CauseStage}
		if observation.Err != nil {
			record["error"] = observation.Err.Error()
			notes = append(notes, observation.Stage+": "+observation.Err.Error())
		}
		observations = append(observations, record)
	}
	encoded, err := json.Marshal(observations)
	if err != nil {
		return nil, err
	}
	// Keep the verified candidate in the rendered evidence even when other
	// candidates must be omitted for the context budget.
	if evaluation.Match != nil && evaluation.Match.Result != nil {
		selected := evaluation.Match.Result
		candidates = slices.DeleteFunc(candidates, func(candidate *types.SearchResult) bool {
			return candidate.KnowledgeBaseID == selected.KnowledgeBaseID && candidate.ID == selected.ID
		})
		candidates = append([]*types.SearchResult{selected}, candidates...)
	}
	if len(documents) > 0 {
		ctx = tools.WithOutputBudget(ctx, max(1, tools.OutputBudget(ctx)/2))
	}
	result := search.FormatFAQResults(ctx, evaluation.Query, candidates)
	result.Success = completed || len(candidates) > 0
	result.RetrievalFailures = failures
	result.FAQ = &types.FAQToolResult{}
	if evaluation.Usage != nil {
		result.FAQ.Usage = *evaluation.Usage
	}
	if len(notes) > 0 {
		result.Data["stage_failures"] = notes
		result.Output += "\nFAQ stages not completed: " + strings.Join(notes, "; ")
		if !result.Success {
			result.Error = "FAQ evaluation did not complete; this is not evidence that the sources lack an answer."
		}
	}
	if evaluation.Match != nil && evaluation.Match.Metadata != nil && evaluation.Match.Result != nil {
		match := evaluation.Match
		var answers []string
		for _, answer := range match.Metadata.Answers {
			if strings.TrimSpace(answer) != "" {
				answers = append(answers, answer)
			}
		}
		if len(answers) > 0 {
			if match.Metadata.AnswerStrategy == types.AnswerStrategyRandom {
				answers = []string{answers[rand.IntN(len(answers))]}
			}
			result.FAQ.Reply = strings.Join(answers, "\n\n")
			ref := *match.Result
			ref.Content = result.FAQ.Reply
			result.FAQ.References = []*types.SearchResult{&ref}
		}
	} else if evaluation.Question != "" && evaluation.Clarification != nil {
		// Missing user scope cannot be filled by the document fallback. Legacy or
		// invalid field metadata returns to the existing Agent decision instead.
		documents = nil
		fields := make([]string, 0, len(evaluation.MissingUserFields))
		for _, field := range evaluation.MissingUserFields {
			field = strings.TrimSpace(field)
			if field == "" || slices.ContainsFunc(fields, func(previous string) bool { return strings.EqualFold(previous, field) }) {
				fields = nil
				break
			}
			fields = append(fields, field)
		}
		if len(fields) == 0 {
			const clarificationNote = "FAQ applicability requires user clarification; record the necessary missing fields before continuing dependent work."
			result.Data["stage_failures"] = append(notes, clarificationNote)
			result.Output += "\n" + clarificationNote
			return &agent.FAQResolution{Result: result, Observations: encoded}, nil
		}
		result.FAQ.Clarification, err = json.Marshal(evaluation.Clarification)
		if err != nil {
			return nil, err
		}
		result.FAQ.Reply = evaluation.Question
		result.FAQ.MissingUserFields = fields
	}
	return &agent.FAQResolution{Result: result, Observations: encoded, DocumentTargets: documents.Clone()}, nil
}
