package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type retrievalStateRepo struct {
	interfaces.MessageRepository
	previous *types.Message
	reads    int
	saved    json.RawMessage
}

func (r *retrievalStateRepo) GetPreviousRetrievalState(context.Context, string, string) (*types.Message, error) {
	r.reads++
	return r.previous, nil
}

func TestSingleTurnSuggestionsRetainCurrentMissingStateWithoutContinuation(t *testing.T) {
	repo := &retrievalStateRepo{previous: &types.Message{ExecutionContext: types.MessageExecutionContext{RetrievalState: json.RawMessage(`invalid previous state`)}}}
	service := &sessionService{messageRepo: repo}
	agent := &types.CustomAgent{ID: "agent"}
	agent.Config.QuestionSuggestions = &types.QuestionSuggestionConfig{FollowUps: types.FollowUpSuggestionConfig{Enabled: true}}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, AssistantMessageID: "assistant", CustomAgent: agent}
	cfg := &types.AgentConfig{MultiTurnEnabled: false, SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb"}}}
	engine := &retrievalBoundEngine{}
	require.NoError(t, service.bindAgentRetrievalState(t.Context(), req, cfg, engine))
	require.True(t, engine.enabled)
	require.True(t, engine.runtimeRetrieval)
	require.Empty(t, engine.previous)
	require.Zero(t, repo.reads, "single-turn requests never inherit an earlier question's control state")
	require.NotNil(t, engine.save)
	require.NoError(t, engine.save(t.Context(), json.RawMessage(`{"scope":"current","gaps":[{"scope":"current","status":"needs_user","missing_user_fields":["订单号"]}]}`)))
	message := &types.Message{ExecutionContext: types.MessageExecutionContext{RetrievalState: repo.saved}}
	require.NotNil(t, missingInputFollowUps(message), "post-answer suggestions can see this turn's blockers")
}
func (r *retrievalStateRepo) UpdateMessageRetrievalState(_ context.Context, _, _ string, raw json.RawMessage) error {
	r.saved = append(json.RawMessage(nil), raw...)
	return nil
}

type retrievalBoundEngine struct {
	enabled          bool
	runtimeRetrieval bool
	previous         json.RawMessage
	save             func(context.Context, json.RawMessage) error
}

func (e *retrievalBoundEngine) EnableRetrievalGuard()   { e.enabled = true }
func (e *retrievalBoundEngine) EnableRuntimeRetrieval() { e.runtimeRetrieval = true }
func (e *retrievalBoundEngine) SetRetrievalPersistence(raw json.RawMessage, save func(context.Context, json.RawMessage) error) {
	e.previous = raw
	e.save = save
}

func TestAgentRetrievalStateBindingReusesOnlySameBusinessPolicy(t *testing.T) {
	repo := &retrievalStateRepo{}
	service := &sessionService{messageRepo: repo}
	req := &types.QARequest{Session: &types.Session{ID: "session"}, AssistantMessageID: "assistant", CustomAgent: &types.CustomAgent{ID: "agent"}}
	cfg := &types.AgentConfig{MultiTurnEnabled: true, SystemPrompt: "business rules", SearchTargets: types.SearchTargets{{KnowledgeBaseID: "kb"}}}
	first := &retrievalBoundEngine{}
	require.NoError(t, service.bindAgentRetrievalState(t.Context(), req, cfg, first))
	require.True(t, first.enabled)
	require.True(t, first.runtimeRetrieval)
	require.NoError(t, first.save(t.Context(), json.RawMessage(`{"version":3}`)))
	repo.previous = &types.Message{ExecutionContext: types.MessageExecutionContext{RetrievalState: repo.saved}}
	next := &retrievalBoundEngine{}
	require.NoError(t, service.bindAgentRetrievalState(t.Context(), req, cfg, next))
	require.JSONEq(t, `{"version":3}`, string(next.previous))
	cfg.SystemPrompt = "updated business rules"
	changed := &retrievalBoundEngine{}
	require.NoError(t, service.bindAgentRetrievalState(t.Context(), req, cfg, changed))
	require.Empty(t, changed.previous)
}
