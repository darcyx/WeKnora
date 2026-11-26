package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// RequestedTargets is not proof of completed coverage. Actual failed sub-scopes
// remain attached to their typed causes, including partial candidate results.
type agentFAQObservation struct {
	Stage            string
	Query            string
	RequestedTargets types.SearchTargets
	CompletedTargets types.SearchTargets
	SkippedTargets   []tools.FAQSkippedTarget
	Matches          []tools.FAQMatch
	Candidates       []*types.SearchResult
	Failures         []types.RetrievalFailure
	CauseStage       string
	Err              error
}

// Evaluation never publishes; the caller decides when its result can be emitted.
type agentFAQEvaluation struct {
	Query        string
	Observations []agentFAQObservation
	Match        *tools.FAQMatch
	Answer       string
	Usage        *types.TokenUsage
	ModelID      string
}

func (e *agentFAQEvaluation) captureUsage(cfg *types.AgentConfig, model chat.Chat) {
	if cfg != nil && cfg.FAQPreflightUsage != nil {
		usage := *cfg.FAQPreflightUsage
		e.Usage = &usage
		if model != nil {
			e.ModelID = model.GetModelID()
		}
	}
}

func (e *agentFAQEvaluation) observe(stage, query string, targets types.SearchTargets, matches []tools.FAQMatch, err error) {
	observation := agentFAQObservation{Stage: stage, Query: query, RequestedTargets: targets.Clone(),
		Matches: slices.Clone(matches), Err: err}
	for _, match := range matches {
		observation.Candidates = append(observation.Candidates, match.Result)
	}
	var incomplete *tools.FAQSearchError
	if errors.As(err, &incomplete) {
		observation.CauseStage = incomplete.CauseStage
		observation.Failures = slices.Clone(incomplete.Failures)
		for i := range observation.Failures {
			failure := &observation.Failures[i]
			failure.KnowledgeBaseIDs = slices.Clone(failure.KnowledgeBaseIDs)
			failure.Targets = failure.Targets.Clone()
		}
		observation.Candidates = slices.Clone(incomplete.Candidates)
	}
	e.Observations = append(e.Observations, observation)
}

func (e *agentFAQEvaluation) observeSearch(stage, query string, result tools.FAQSearchResult, err error) {
	e.observe(stage, query, result.RequestedTargets, result.Matches, err)
	observation := &e.Observations[len(e.Observations)-1]
	observation.CompletedTargets = result.CompletedTargets.Clone()
	observation.SkippedTargets = slices.Clone(result.SkippedTargets)
	for i := range observation.SkippedTargets {
		skip := &observation.SkippedTargets[i]
		skip.Target = types.SearchTargets{skip.Target}.Clone()[0]
	}
	observation.Candidates = slices.Clone(result.Candidates)
}

func (e *agentFAQEvaluation) reply() agent.SteerFAQReply {
	reply := agent.SteerFAQReply{Usage: e.Usage}
	if e.Match != nil {
		answer := e.Answer
		if e.Match.Result != nil && e.Match.Metadata != nil && strings.TrimSpace(answer) != "" && !tools.IsProtocolArtifact(answer) {
			if _, public := tools.SplitThinkContent(answer); public == answer {
				reply.Answer = answer
			}
		}
	}
	return reply
}

func emitAgentFAQEvaluation(ctx context.Context, req *types.QARequest, bus *event.EventBus, evaluation *agentFAQEvaluation) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if evaluation == nil || bus == nil || !faqTurnUnsteered(ctx, req) {
		return false, nil
	}
	reply := evaluation.reply()
	if reply.Answer == "" {
		return false, nil
	}
	id := req.AssistantMessageID + "-faq-answer"
	if match := evaluation.Match; match != nil {
		logger.Infof(ctx, "FAQ direct answer: session=%s message=%s kb=%s chunk=%s", req.Session.ID, req.AssistantMessageID, match.Result.KnowledgeBaseID, match.Result.ID)
	}
	if err := bus.Emit(ctx, event.Event{ID: id, Type: event.EventAgentFinalAnswer,
		SessionID: req.Session.ID, Data: event.AgentFinalAnswerData{Content: reply.Answer, Done: true}}); err != nil {
		return false, err
	}
	data := event.AgentCompleteData{SessionID: req.Session.ID, MessageID: req.AssistantMessageID, FinalAnswer: reply.Answer,
		Usage: reply.Usage, ModelID: evaluation.ModelID}
	if err := bus.Emit(ctx, event.Event{ID: req.AssistantMessageID + "-faq-complete", Type: event.EventAgentComplete,
		SessionID: req.Session.ID, Data: data}); err != nil {
		return false, err
	}
	return true, nil
}
