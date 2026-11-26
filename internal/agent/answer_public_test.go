package agent

import (
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
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

func TestSanitizeFollowUpButtonsStripsOnFallbackRefusalAndMissingConditions(t *testing.T) {
	t.Run("configured fixed fallback strips buttons and heading", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		e.config.FallbackStrategy = string(types.FallbackStrategyFixed)
		e.config.FallbackResponse = "**现状**：当前知识库没有查询到该问题对应的解答。"
		in := "**现状**：当前知识库没有查询到该问题对应的解答。\n\n## 相关问题\n<button-send>如何接入推送</button-send>\n"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, "**现状**：当前知识库没有查询到该问题对应的解答。", got)
	})

	t.Run("out of scope refusal strips buttons without fixed fallback config", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		in := "我是助手，无法回答与SDK无关问题，欢迎向我咨询相关业务问题。\n\n**相关问题：**\n- <button-send>怎么配置支付</button-send>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, "我是助手，无法回答与SDK无关问题，欢迎向我咨询相关业务问题。", got)
	})

	t.Run("no answer body strips buttons and inline button", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		in := "**现状**：当前知识库没有查询到该问题对应的解答。\n\n**建议**：建议将该问题反馈给相关同事。<button-send>查看其他功能</button-send>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, "**现状**：当前知识库没有查询到该问题对应的解答。\n\n**建议**：建议将该问题反馈给相关同事。", got)
	})

	t.Run("missing user conditions strips buttons on clarification", func(t *testing.T) {
		e := registeredMissingFields(t, "平台")
		in := "方便提供一下平台吗？\n\n## 相关问题\n<button-send>Android 怎么接</button-send>"
		got := e.sanitizeFollowUpButtons(in)
		require.Equal(t, "方便提供一下平台吗？", got)
	})
}

func TestSanitizeFollowUpButtonsNormalizesDedupsDropsContradictedAndCaps(t *testing.T) {
	e := newTestEngine(t, &mockChat{})

	t.Run("strips heading and list numbers, drops contradicted before capping to 3", func(t *testing.T) {
		in := "## 沙盒支付说明\n抖音小游戏不支持沙盒支付。\n\n## 相关问题\n" +
			"1. <button-send>抖音小游戏沙盒支付怎么配置</button-send>\n" +
			"2. <button-send> 微信小游戏沙盒支付怎么配置 </button-send>\n" +
			"- <button-send>微信小游戏沙盒支付怎么配置</button-send>\n" +
			"3. <button-send>抖音小游戏怎么配置</button-send>\n" +
			"4. <button-send>微信小游戏怎么提审</button-send>\n" +
			"5. <button-send>服务端回调怎么验签</button-send>\n"
		want := "## 沙盒支付说明\n抖音小游戏不支持沙盒支付。\n\n" +
			"<button-send>微信小游戏沙盒支付怎么配置</button-send>\n" +
			"<button-send>抖音小游戏怎么配置</button-send>\n" +
			"<button-send>微信小游戏怎么提审</button-send>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("keeps ordinary heading when no buttons present", func(t *testing.T) {
		in := "## 相关问题\n请参考官方文档排查。"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, in, got)
	})

	t.Run("drops all buttons and trims trailing space when every button is contradicted", func(t *testing.T) {
		in := "快手渠道暂不支持退款。\n\n**猜你想问：**\n<button-send>快手渠道退款流程</button-send>\n"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, "快手渠道暂不支持退款。", got)
	})
}

func TestFollowUpButtonsPreserveFencedExamples(t *testing.T) {
	e := newTestEngine(t, &mockChat{})
	code := "```html\n<button-send>one</button-send> <button-send>two</button-send>\n<button-send>three</button-send>\n```"
	t.Run("normal answer and cap", func(t *testing.T) {
		in := "示例\n\n" + code + "\n\n<button-send>real</button-send>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, in, got)
	})
	t.Run("fallback removes only real buttons", func(t *testing.T) {
		in := "当前知识库没有找到相关解答。\n\n" + code + "\n\n<button-send>real</button-send>"
		require.Equal(t, "当前知识库没有找到相关解答。\n\n"+code, e.sanitizeFollowUpButtons(in))
	})
	t.Run("code claims do not affect real buttons", func(t *testing.T) {
		in := "```text\n抖音小游戏不支持沙盒支付。\n```\n<button-send>抖音小游戏沙盒支付怎么配置</button-send>"
		require.Equal(t, in, e.sanitizeFollowUpButtons(in))
	})
	t.Run("code refusal does not suppress real buttons", func(t *testing.T) {
		in := "示例\n```text\n当前知识库没有找到相关解答。\n```\n<button-send>real</button-send>"
		require.Equal(t, in, e.sanitizeFollowUpButtons(in))
	})
	t.Run("real claim does not remove code buttons", func(t *testing.T) {
		in := "抖音小游戏不支持沙盒支付。\n```html\n<button-send>抖音小游戏沙盒支付怎么配置</button-send>\n```"
		require.Equal(t, in, e.sanitizeFollowUpButtons(in))
	})
}
