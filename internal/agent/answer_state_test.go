package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func registeredMissingFields(t *testing.T, fields ...string) *AgentEngine {
	t.Helper()
	e, _ := newRetrievalProbeEngine(t)
	id, err := e.retrieval.register(retrievalFactProposal{GoalQuote: "TapTap sandbox configuration", Fact: "sandbox configuration"})
	require.NoError(t, err)
	require.NoError(t, e.retrieval.applyReview(retrievalReview{GapID: id, Status: retrievalNeedsUser, MissingUserFields: fields}))
	return e
}

func TestRegisteredMissingFieldsBlockOrdinaryAndFinalAnswer(t *testing.T) {
	for _, path := range []string{"ordinary", "final"} {
		t.Run(path, func(t *testing.T) {
			e := registeredMissingFields(t, "渠道")
			e.chatModel = &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "打开支付配置并开启沙盒开关。", Done: true, FinishReason: "stop"}}}}}
			var public string
			e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
				data := ev.Data.(event.AgentFinalAnswerData)
				if data.Replace {
					public = ""
				}
				public += data.Content
				return nil
			})
			if path == "ordinary" {
				response, err := e.streamThinkingToEventBus(t.Context(), nil, nil, 0, "session")
				require.NoError(t, err)
				require.Equal(t, "方便提供一下渠道吗？", response.Content)
			} else {
				state := &types.AgentState{}
				require.NoError(t, e.streamFinalAnswerToEventBus(t.Context(), "sandbox", state, "session", nil))
				require.Equal(t, "方便提供一下渠道吗？", state.FinalAnswer)
			}
			require.Equal(t, "方便提供一下渠道吗？", public)
		})
	}
}

func TestRegisteredMissingFieldsPreserveOnlyVerifiedNaturalQuestions(t *testing.T) {
	for _, tc := range []struct {
		name, content, reply string
		preserve             bool
	}{
		{"complete", "这笔支付已经扣款了吗？请发一下 SDK 或渠道订单号，任意一种就可以。", `{"decision":"allow","clarification_complete":true,"reason":"asks exactly the necessary fields without promising a result"}`, true},
		{"complete request", "请告知是否已扣款，并提供 SDK 或渠道订单号，任意一种即可。", `{"decision":"allow","clarification_complete":true,"reason":"requests both required fields without extra claims"}`, true},
		{"incomplete", "能发一下订单号吗？", `{"decision":"allow","clarification_complete":false,"reason":"deduction status is still unanswered"}`, false},
		{"dependent steps", "先开启沙盒开关，再发一下订单号好吗？", `{"decision":"stop","reason":"contains steps dependent on missing conditions"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := registeredMissingFields(t, "扣款情况", "订单号")
			e.EnableRetrievalGuard()
			e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: tc.reply}
			answer, err := e.publicAnswer(t.Context(), tc.content, nil)
			require.NoError(t, err)
			if tc.preserve {
				require.Equal(t, tc.content, answer)
			} else {
				require.Equal(t, "方便提供一下扣款情况、订单号吗？", answer)
			}
			require.Equal(t, retrievalNeedsUser, e.retrieval.snapshot().Gaps[0].Status)
		})
	}
}

type projectContextChat struct {
	*mockChat
	reply    string
	replies  []string
	messages []chat.Message
	calls    [][]chat.Message
}

func (m *projectContextChat) Chat(_ context.Context, messages []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	m.messages = messages
	m.calls = append(m.calls, messages)
	if len(m.replies) > 0 {
		reply := m.replies[0]
		m.replies = m.replies[1:]
		return &types.ChatResponse{Content: reply}, nil
	}
	return &types.ChatResponse{Content: m.reply}, nil
}

func TestFirstMissingFieldsPreserveOnlyVerifiedNaturalQuestions(t *testing.T) {
	for _, tc := range []struct {
		name, content, reply string
		preserve             bool
	}{
		{"complete", "这笔支付已经扣款了吗？发一下 SDK 或渠道订单号就可以。", `{"decision":"allow","clarification_complete":true,"reason":"asks both required fields"}`, true},
		{"incomplete", "能发一下订单号吗？", `{"decision":"allow","clarification_complete":false,"reason":"deduction status is still missing"}`, false},
		{"dependent steps", "先开启沙盒，再发一下订单号好吗？", `{"decision":"stop","reason":"contains dependent steps"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newRetrievalProbeEngine(t)
			e.beginTurn("支付未到账", nil, &types.AgentState{})
			e.EnableRetrievalGuard()
			model := &projectContextChat{mockChat: &mockChat{}, replies: []string{
				`{"decision":"needs_user","missing_user_fields":["扣款情况","订单号"],"reason":"required by the business workflow"}`, tc.reply,
			}}
			e.chatModel = model
			answer, err := e.publicAnswer(t.Context(), tc.content, nil)
			require.NoError(t, err)
			if tc.preserve {
				require.Equal(t, tc.content, answer)
			} else {
				require.Equal(t, "方便提供一下扣款情况、订单号吗？", answer)
			}
			require.Len(t, model.calls, 2)
			require.Equal(t, retrievalNeedsUser, e.retrieval.snapshot().Gaps[0].Status)
		})
	}
}

func TestProjectContextReachesGuardAndResolvesOnlySuppliedField(t *testing.T) {
	e := registeredMissingFields(t, "渠道", "订单号")
	e.EnableRetrievalGuard()
	e.appInfo = `{"channel":"微信小游戏"}`
	e.SetRetrievalUserMessage("user", "请继续处理")
	model := &projectContextChat{mockChat: &mockChat{}}
	response, _ := json.Marshal(retrievalAdmission{Decision: "continue", Reason: "channel is provided by project context", UserAnswers: []retrievalConditionAnswer{{GapID: "gap-1", retrievalUserAnswer: retrievalUserAnswer{Field: "渠道", InputID: e.projectInputID(), Quote: "微信小游戏"}}}})
	model.reply = string(response)
	e.chatModel = model
	answer, err := e.publicAnswer(t.Context(), "打开支付配置并开启沙盒开关。", nil)
	require.NoError(t, err)
	require.Equal(t, "方便提供一下订单号吗？", answer)
	var payload map[string]any
	projectMessages := model.calls[0]
	require.NoError(t, json.Unmarshal([]byte(projectMessages[len(projectMessages)-1].Content[len("project_conditions\n"):]), &payload))
	require.Equal(t, e.appInfo, payload["app_info"])
	gap := e.retrieval.snapshot().Gaps[0]
	require.Equal(t, []string{"订单号"}, gap.MissingUserFields)
	require.Equal(t, e.projectInputID(), gap.UserAnswers[0].InputID)
	require.Equal(t, "请继续处理", e.retrievalUserMessage.text)
}

func TestProjectContextRejectsInventedValue(t *testing.T) {
	e := registeredMissingFields(t, "渠道")
	e.EnableRetrievalGuard()
	e.appInfo = `{"channel":"微信小游戏"}`
	raw, _ := json.Marshal(retrievalAdmission{Decision: "continue", Reason: "test invalid quote", UserAnswers: []retrievalConditionAnswer{{GapID: "gap-1", retrievalUserAnswer: retrievalUserAnswer{Field: "渠道", InputID: e.projectInputID(), Quote: "抖音小游戏"}}}})
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: string(raw)}
	require.Error(t, e.resolveProjectConditions(t.Context()))
	require.Equal(t, []string{"渠道"}, e.retrieval.snapshot().Gaps[0].MissingUserFields)
	require.Empty(t, e.retrieval.snapshot().Gaps[0].UserAnswers)
}

func TestMissingFieldsPreserveSupportedIndependentAnswer(t *testing.T) {
	e := registeredMissingFields(t, "渠道")
	c := e.retrieval
	id, err := c.register(retrievalFactProposal{GoalQuote: "TapTap sandbox configuration", Fact: "whitelist purpose", DistinctFrom: []retrievalDistinction{{GapID: "gap-1", Reason: "independent supported explanation"}}})
	require.NoError(t, err)
	require.NoError(t, c.acquire(id, "evidence", retrievalInitial, "", "search_knowledge", `{}`))
	require.NoError(t, c.observe("evidence", "白名单用于允许测试账号。", retrievalSuccess))
	require.NoError(t, c.applyReview(retrievalReview{GapID: id, CallID: "evidence", Status: retrievalSatisfied, Quotes: []string{"白名单用于允许测试账号。"}}))
	e.EnableRetrievalGuard()
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"only the independent whitelist explanation is answered"}`}
	answer, err := e.publicAnswer(t.Context(), "白名单用于允许测试账号。", nil)
	require.NoError(t, err)
	require.Equal(t, "白名单用于允许测试账号。\n\n方便提供一下渠道吗？", answer)
}

func TestOptionalProjectContextCanBeAbsent(t *testing.T) {
	e := registeredMissingFields(t, "渠道")
	e.EnableRetrievalGuard()
	model := &projectContextChat{mockChat: &mockChat{}, reply: `{"decision":"allow","reason":"no project metadata is required for this decision"}`}
	e.chatModel = model
	answer, err := e.publicAnswer(t.Context(), "打开支付配置并开启沙盒开关。", nil)
	require.NoError(t, err)
	require.Equal(t, "方便提供一下渠道吗？", answer)
	for _, messages := range model.calls {
		require.NotContains(t, messages[len(messages)-1].Content, "project_conditions\n", "missing optional context must not start a project-context call")
	}
	_, err = e.retrievalAdmission(t.Context(), "fact", map[string]any{"goal": "sandbox"})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(model.messages[len(model.messages)-1].Content[len("fact\n"):]), &payload))
	require.NotContains(t, payload, "app_info")
	require.NotContains(t, payload, "app_info_input_id")
}
