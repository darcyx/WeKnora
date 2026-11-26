package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublicAnswerDegradesOnGuardError(t *testing.T) {
	e := registeredMissingFields(t, "渠道")
	e.EnableRetrievalGuard()
	e.appInfo = `{"channel":"微信小游戏"}`
	raw, _ := json.Marshal(retrievalAdmission{Decision: "continue", Reason: "invalid quote", UserAnswers: []retrievalConditionAnswer{{GapID: "gap-1", retrievalUserAnswer: retrievalUserAnswer{Field: "渠道", InputID: e.projectInputID(), Quote: "抖音小游戏"}}}})
	e.chatModel = &boundaryChat{mockChat: &mockChat{}, reply: string(raw)}
	answer, err := e.publicAnswer(t.Context(), "打开支付配置并开启沙盒开关。", nil)
	require.NoError(t, err)
	require.Equal(t, e.enforceMissingConditions(t.Context(), "打开支付配置并开启沙盒开关。"), answer)
}

func TestDropContradictedButtons(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"drops only subject+object match",
			"抖音小游戏不支持沙盒支付。\n<button-send>抖音小游戏沙盒支付怎么配置</button-send>\n<button-send>微信小游戏沙盒支付怎么配置</button-send>\n<button-send>抖音小游戏怎么配置</button-send>",
			"抖音小游戏不支持沙盒支付。\n<button-send>微信小游戏沙盒支付怎么配置</button-send>\n<button-send>抖音小游戏怎么配置</button-send>"},
		{"deictic subject uses object",
			"该SDK不支持微信登录。\n<button-send>该SDK如何接入支付</button-send>\n<button-send>微信登录怎么接</button-send>",
			"该SDK不支持微信登录。\n<button-send>该SDK如何接入支付</button-send>\n"},
		{"table cell", "| 微信登录 | 不支持 |\n<button-send>微信登录怎么接</button-send>", "| 微信登录 | 不支持 |\n"},
		{"list item", "- 微信登录：不支持\n<button-send>微信登录怎么接</button-send>", "- 微信登录：不支持\n"},
		{"double negation kept", "鸿蒙并非不支持推送。\n<button-send>鸿蒙推送配置</button-send>", "鸿蒙并非不支持推送。\n<button-send>鸿蒙推送配置</button-send>"},
		{"adverb before 不支持", "快手渠道目前暂不支持退款。\n<button-send>快手渠道退款流程</button-send>", "快手渠道目前暂不支持退款。\n"},
		{"no unsupported keeps all", "微信支持沙盒。\n<button-send>微信怎么配置</button-send>", "微信支持沙盒。\n<button-send>微信怎么配置</button-send>"},
		{"empty subject ignored", "不支持。\n<button-send>怎么办</button-send>", "不支持。\n<button-send>怎么办</button-send>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { require.Equal(t, c.want, dropContradictedButtons(c.in)) })
	}
}
