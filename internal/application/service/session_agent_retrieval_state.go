package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

type agentRetrievalStateStore interface {
	GetPreviousRetrievalState(context.Context, string, string) (*types.Message, error)
	UpdateMessageRetrievalState(context.Context, string, string, json.RawMessage) error
}

type agentRetrievalRecord struct {
	Policy string          `json:"policy"`
	State  json.RawMessage `json:"state"`
}

func (s *sessionService) bindAgentRetrievalState(ctx context.Context, req *types.QARequest, cfg *types.AgentConfig, engine any) error {
	bound, ok := engine.(interface {
		EnableRetrievalGuard()
		SetRetrievalPersistence(json.RawMessage, func(context.Context, json.RawMessage) error)
	})
	if !ok || len(cfg.SearchTargets) == 0 {
		return nil
	}
	bound.EnableRetrievalGuard()
	if ordinary, ok := engine.(interface{ EnableRuntimeRetrieval() }); ok {
		ordinary.EnableRuntimeRetrieval()
	}
	followUps := req.CustomAgent != nil && req.CustomAgent.Config.QuestionSuggestions != nil && req.CustomAgent.Config.QuestionSuggestions.FollowUps.Enabled
	if !cfg.MultiTurnEnabled && !followUps {
		return nil
	}
	store, ok := s.messageRepo.(agentRetrievalStateStore)
	if !ok {
		return fmt.Errorf("message repository cannot retain retrieval state")
	}
	policyJSON, err := json.Marshal(struct {
		Prompt, WebPrompt, OfflinePrompt, Agent string
	}{cfg.SystemPrompt, cfg.SystemPromptWebEnabled, cfg.SystemPromptWebDisabled, req.CustomAgent.ID})
	if err != nil {
		return err
	}
	policy := fmt.Sprintf("%x", sha256.Sum256(policyJSON))
	var previous json.RawMessage
	if cfg.MultiTurnEnabled {
		var message *types.Message
		var err error
		if req.Resume == nil {
			message, err = store.GetPreviousRetrievalState(ctx, req.Session.ID, req.AssistantMessageID)
		} else {
			message, err = s.messageRepo.GetMessage(ctx, req.Session.ID, req.AssistantMessageID)
		}
		if err != nil {
			return fmt.Errorf("load previous retrieval state: %w", err)
		}
		if message != nil && len(message.ExecutionContext.RetrievalState) > 0 {
			var record agentRetrievalRecord
			if err := json.Unmarshal(message.ExecutionContext.RetrievalState, &record); err != nil {
				return fmt.Errorf("decode previous retrieval state: %w", err)
			}
			if record.Policy == policy {
				previous = record.State
			}
		}
	}
	bound.SetRetrievalPersistence(previous, func(ctx context.Context, state json.RawMessage) error {
		raw, err := json.Marshal(agentRetrievalRecord{Policy: policy, State: state})
		if err != nil {
			return err
		}
		return store.UpdateMessageRetrievalState(ctx, req.Session.ID, req.AssistantMessageID, raw)
	})
	return nil
}
