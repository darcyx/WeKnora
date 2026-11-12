package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/agent/approval"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
)

type fakeAsker struct {
	answer  approval.Answer
	asked   []approval.AskRequest
	resumed []*types.AgentPrompt
}

func (f *fakeAsker) AskUser(_ context.Context, req approval.AskRequest) (approval.Answer, error) {
	f.asked = append(f.asked, req)
	return f.answer, nil
}

func (f *fakeAsker) ResumeAnswer(_ context.Context, p *types.AgentPrompt, _ *event.EventBus) approval.Answer {
	f.resumed = append(f.resumed, p)
	return f.answer
}

func askCtx(meta *ToolExecContext) context.Context {
	return WithToolExecContext(context.Background(), meta)
}

const formArgs = `{"question":"deploy where?","fields":[
	{"name":"env","type":"select","options":["dev","prod"],"required":true},
	{"name":"replicas","type":"number"}]}`

func TestAskUserTool_NotInteractiveAsksInReply(t *testing.T) {
	asker := &fakeAsker{}
	res, err := NewAskUserTool(asker).Execute(
		askCtx(&ToolExecContext{EventBus: event.NewEventBus()}), json.RawMessage(`{"question":"q"}`))
	require.NoError(t, err)
	assert.True(t, res.Success)
	assert.Equal(t, "q", res.Output)
	assert.NotNil(t, res.UserQuestion)
	assert.Empty(t, asker.asked)
}

func TestAskUserTool_RejectsInvalidQuestion(t *testing.T) {
	res, err := NewAskUserTool(&fakeAsker{}).Execute(
		askCtx(&ToolExecContext{Mode: types.RunModeAttended, EventBus: event.NewEventBus()}),
		json.RawMessage(`{"question":"q","options":["a"],"fields":[{"name":"n","type":"text"}]}`))
	require.NoError(t, err)
	assert.False(t, res.Success)
	assert.Contains(t, res.Error, "Invalid question")
}

func TestAskUserToolRendersOptionsAndFormsWithoutWaiting(t *testing.T) {
	for _, tc := range []struct{ args, text string }{
		{`{"question":"选择平台","options":["Android","iOS"]}`, "选择平台\n- Android\n- iOS"},
		{formArgs, "deploy where?\n- env: dev / prod\n- replicas"},
		{`{"question":"补充材料","fields":[{"name":"order","label":"订单号","type":"text","required":true}]}`, "补充材料\n- 订单号"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			asker := &fakeAsker{}
			result, err := NewAskUserTool(asker).Execute(t.Context(), json.RawMessage(tc.args))
			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, tc.text, result.Output)
			require.NotNil(t, result.UserQuestion)
			require.Empty(t, asker.asked)
		})
	}
}

func TestAskUserTool_ResumedUsesStoredAnswer(t *testing.T) {
	asker := &fakeAsker{answer: approval.Answer{PromptID: "p1", Answered: true, Response: types.AskUserPromptResponse{
		Text: "blue",
	}}}
	answered := &types.AgentPrompt{ID: "p1", Kind: types.AgentPromptKindAskUser}
	// A resumed run is served even if the client could not show a new prompt.
	res, err := NewAskUserTool(asker).Execute(
		askCtx(&ToolExecContext{Answered: answered, EventBus: event.NewEventBus()}),
		json.RawMessage(`{"question":"q"}`))
	require.NoError(t, err)
	assert.Equal(t, "The user answered.\nText: blue", res.Output)
	assert.Empty(t, asker.asked)
	assert.Equal(t, []*types.AgentPrompt{answered}, asker.resumed)
}

func TestAskUserTool_NoAnswer(t *testing.T) {
	asker := &fakeAsker{answer: approval.Answer{PromptID: "p1", Reason: "the user declined to answer"}}
	res, err := NewAskUserTool(asker).Execute(
		askCtx(&ToolExecContext{Answered: &types.AgentPrompt{Kind: types.AgentPromptKindAskUser}, EventBus: event.NewEventBus()}),
		json.RawMessage(`{"question":"q"}`))
	require.NoError(t, err)
	assert.True(t, res.Success)
	assert.Contains(t, res.Output, "did not answer (the user declined to answer)")
}

func TestAskUserDoesNotCreatePromptForSharedAgent(t *testing.T) {
	asker := &fakeAsker{}
	ctx := context.WithValue(t.Context(), types.TenantIDContextKey, uint64(1))
	ctx = types.WithExecutionTenant(ctx, 2)
	ctx = WithToolExecContext(ctx, &ToolExecContext{Mode: types.RunModeAttended, EventBus: event.NewEventBus()})
	_, err := NewAskUserTool(asker).Execute(ctx, json.RawMessage(`{"question":"which?"}`))
	require.NoError(t, err)
	require.Empty(t, asker.asked)
}

func TestAskUserRegistryValidatesSerializedOptions(t *testing.T) {
	malformed := `["first", "depends on "rules""]`
	cases := []struct {
		name    string
		options any
		valid   bool
		count   int
	}{
		{"real array", []string{"first", `depends on "rules"`}, true, 2},
		{"valid serialized array", `["first","second"]`, true, 2},
		{"single text", "first", true, 1},
		{"ordinary bracket label", []string{"[staging]"}, true, 1},
		{"malformed serialized array", malformed, false, 0},
		{"truncated serialized array", `["first",`, false, 0},
		{"wrapped malformed array", []string{malformed}, false, 0},
		{"wrapped valid array", []string{`["first","second"]`}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asker := &fakeAsker{}
			registry := NewToolRegistry()
			registry.RegisterTool(NewAskUserTool(asker))
			ctx := askCtx(&ToolExecContext{Mode: types.RunModeAttended, EventBus: event.NewEventBus()})
			args, err := json.Marshal(map[string]any{"question": "which?", "options": tc.options})
			require.NoError(t, err)
			result, err := registry.ExecuteTool(ctx, ToolAskUser, args)
			require.NoError(t, err)
			require.Equal(t, tc.valid, result.Success, result.Error)
			if tc.valid {
				require.Empty(t, asker.asked)
				require.NotNil(t, result.UserQuestion)
				require.Len(t, result.UserQuestion.Options, tc.count)
			} else {
				require.Empty(t, asker.asked, "invalid input must never create a prompt")
				require.Contains(t, result.Error, "options")
			}
		})
	}
}

func TestAskUserRejectsSerializedFormChoices(t *testing.T) {
	asker := &fakeAsker{}
	result, err := NewAskUserTool(asker).Execute(
		askCtx(&ToolExecContext{Mode: types.RunModeAttended, EventBus: event.NewEventBus()}),
		json.RawMessage(`{"question":"which?","fields":[
			{"name":"env","type":"select","options":["[\"dev\",\"prod\"]"]}
		]}`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "one choice per string")
	require.Empty(t, asker.asked)
}

func TestAskUserToolNeverWaitsForNewInput(t *testing.T) {
	for _, mode := range []types.RunMode{types.RunModeAttended, types.RunModeDeferred, types.RunModeUnattended} {
		t.Run(string(mode), func(t *testing.T) {
			asker := &fakeAsker{answer: approval.Answer{Suspended: &types.PromptSuspension{PromptID: "should-not-exist"}}}
			snapshots := 0
			meta := &ToolExecContext{Mode: mode, EventBus: event.NewEventBus(), Snapshot: func() ([]byte, int, error) { snapshots++; return nil, 0, nil }}
			result, err := NewAskUserTool(asker).Execute(askCtx(meta), json.RawMessage(`{"question":" 请提供订单号。 "}`))
			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, "请提供订单号。", result.Output)
			require.Nil(t, result.Suspend)
			require.Equal(t, "user_question", result.Data["display_type"], "must not reconstruct an interactive history card")
			require.Empty(t, asker.asked)
			require.Zero(t, snapshots)
		})
	}
}

func TestAskUserLegacyResumeWithoutHandlerFailsExplicitly(t *testing.T) {
	result, err := NewAskUserTool(nil).Execute(askCtx(&ToolExecContext{Answered: &types.AgentPrompt{Kind: types.AgentPromptKindAskUser}}), json.RawMessage(`{"question":"q"}`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "no answer handler")
}

func TestAskUserLegacyResumePreservesAnswerOwnershipSuspension(t *testing.T) {
	suspended := &types.PromptSuspension{PromptID: "p1", Kind: types.AgentPromptKindAskUser, Detached: true}
	asker := &fakeAsker{answer: approval.Answer{Suspended: suspended}}
	result, err := NewAskUserTool(asker).Execute(askCtx(&ToolExecContext{Answered: &types.AgentPrompt{Kind: types.AgentPromptKindAskUser}}), json.RawMessage(`{"question":"q"}`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Same(t, suspended, result.Suspend)
	require.Nil(t, result.UserQuestion)
}
