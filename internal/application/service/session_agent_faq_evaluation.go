package service

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// Evaluation never publishes; the caller decides when its result can be emitted.
type agentFAQEvaluation struct {
	Query               string
	Match               *tools.FAQMatch
	Answer              string
	Usage               *types.TokenUsage
	ModelID             string
	Outcome             string
	BroadDocumentSearch bool
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
		if evaluation != nil && evaluation.Match != nil && req != nil && req.Session != nil {
			logger.Infof(ctx, "FAQ direct answer not published: session=%s user_message=%s message=%s outcome=%s published=false",
				req.Session.ID, req.UserMessageID, req.AssistantMessageID, evaluation.Outcome)
		}
		return false, nil
	}
	reply := evaluation.reply()
	if reply.Answer == "" {
		if evaluation.Match != nil && req != nil && req.Session != nil {
			logger.Infof(ctx, "FAQ direct answer filtered: session=%s user_message=%s message=%s outcome=%s published=false",
				req.Session.ID, req.UserMessageID, req.AssistantMessageID, evaluation.Outcome)
		}
		return false, nil
	}
	id := req.AssistantMessageID + "-faq-answer"
	if match := evaluation.Match; match != nil {
		logger.Infof(ctx, "FAQ direct answer: session=%s user_message=%s message=%s kb=%s chunk=%s outcome=%s published=true",
			req.Session.ID, req.UserMessageID, req.AssistantMessageID, match.Result.KnowledgeBaseID, match.Result.ID, evaluation.Outcome)
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
