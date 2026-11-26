package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func collectFinalAnswerEvents(bus *event.EventBus) *[]event.AgentFinalAnswerData {
	var mu sync.Mutex
	var events []event.AgentFinalAnswerData
	bus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		if data, ok := evt.Data.(event.AgentFinalAnswerData); ok {
			mu.Lock()
			events = append(events, data)
			mu.Unlock()
		}
		return nil
	})
	return &events
}

func TestP04BugFixesRefusalWithSourcesAndMultiButtonSingleLine(t *testing.T) {
	t.Run("refusal with trailing reference section still strips buttons", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		e.modelContext.RegisterReferenceURL("https://docs.dobest.cn/docs-pack/a/")
		in := "**现状**：当前知识库没有查询到该问题对应的解答。\n\n**建议**：建议将该问题反馈给@SDK技术部同事。\n\n## 参考来源\n- <a href=\"https://docs.dobest.cn/docs-pack/a/\" target=\"_blank\">文档A</a>\n\n<button-send>进一步了解</button-send>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.NotContains(t, got, "<button-send>")
	})

	t.Run("fenced fixed refusal strips buttons", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		e.config.FallbackStrategy = string(types.FallbackStrategyFixed)
		e.config.FallbackResponse = "**现状**：当前知识库没有查询到该问题对应的解答。"
		in := "```markdown\n**现状**：当前知识库没有查询到该问题对应的解答。\n```\n\n<button-send>继续提问</button-send>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.NotContains(t, got, "<button-send>")
	})

	t.Run("multi-button single line drops empty buttons, splits, drops contradicted, and caps at 3", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		in := "## 说明\n抖音小游戏不支持沙盒支付。\n\n<button-send></button-send> <button-send>   </button-send> <button-send>抖音小游戏沙盒支付怎么配置</button-send> <button-send>问题1</button-send> <button-send>问题2</button-send> <button-send>问题3</button-send> <button-send>问题4</button-send>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.NotContains(t, got, "<button-send></button-send>")
		require.NotContains(t, got, "抖音小游戏沙盒支付怎么配置")
		require.NotContains(t, got, "问题4")
		require.Contains(t, got, "<button-send>问题1</button-send>\n<button-send>问题2</button-send>\n<button-send>问题3</button-send>")
	})
}

func TestFixedResponsesSentinelExpansionAndStreamingGate(t *testing.T) {
	fixed := map[string]string{
		"OOS":       "我是 SDK 部门 AI 小助手，无法回答与SDK无关问题，欢迎向我咨询SDK相关业务问题。",
		"NO_ANSWER": "**现状**：当前知识库没有查询到该问题对应的解答。\n\n**建议**：建议将该问题反馈给@SDK技术部同事。",
	}

	t.Run("expandFixedResponse strips fences, sources, and buttons", func(t *testing.T) {
		raw := "```markdown\n[[FIXED:OOS]]\n```\n\n## 参考来源\n- <a href=\"https://fake.example\">x</a>\n\n<button-send>继续问</button-send>"
		expanded, ok := expandFixedResponse(raw, fixed)
		require.True(t, ok)
		require.Equal(t, fixed["OOS"], expanded)
	})

	t.Run("expandFixedResponse leaves unregistered sentinel untouched", func(t *testing.T) {
		expanded, ok := expandFixedResponse("[[FIXED:UNKNOWN]]", fixed)
		require.False(t, ok)
		require.Equal(t, "[[FIXED:UNKNOWN]]", expanded)
	})

	t.Run("think.go split-chunk sentinel with full-width leading space never flashes raw token", func(t *testing.T) {
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "\u3000```markdown\n[[FIX"},
			{ResponseType: types.ResponseTypeAnswer, Content: "ED:OOS]]\n```", FinishReason: "stop", Done: true},
		}}}}
		e := newTestEngine(t, model)
		e.config.FixedResponses = fixed
		events := collectFinalAnswerEvents(e.eventBus)

		resp, err := e.streamThinkingToEventBus(t.Context(), []chat.Message{{Role: "user", Content: "今天天气怎么样"}}, nil, 0, "sess-1")
		require.NoError(t, err)
		require.Equal(t, fixed["OOS"], resp.Content)
		require.NotEmpty(t, *events)
		for _, evt := range *events {
			require.NotContains(t, evt.Content, "[[FIXED:")
		}
		var combined strings.Builder
		for _, evt := range *events {
			combined.WriteString(evt.Content)
		}
		require.Equal(t, fixed["OOS"], combined.String())
	})

	t.Run("think.go holds reference section during streaming and excludes unverified links on cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "## 核心结论\n按文档配置。\n\n## 参考"},
			{ResponseType: types.ResponseTypeAnswer, Content: "来源\n- <a href=\"https://fake.example/unverified\">假链接</a>", FinishReason: "stop", Done: true},
		}}}}
		e := newTestEngine(t, model)
		events := collectFinalAnswerEvents(e.eventBus)

		// Normal completion: fake link never flashes on SSE and is stripped from final answer.
		resp, err := e.streamThinkingToEventBus(t.Context(), []chat.Message{{Role: "user", Content: "配置"}}, nil, 0, "sess-ref-1")
		require.NoError(t, err)
		require.NotContains(t, resp.Content, "fake.example")
		for _, evt := range *events {
			require.NotContains(t, evt.Content, "fake.example")
		}

		// Cancel right after body streams: held unverified reference section is never saved.
		cancelModel := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "## 核心结论\n已完成。\n\n## 参考来源\n- <a href=\"https://fake.example/cancel\">假链接</a>"},
		}}}}
		eCancel := newTestEngine(t, cancelModel)
		cancelEvents := collectFinalAnswerEvents(eCancel.eventBus)
		eCancel.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, _ event.Event) error {
			cancel()
			return nil
		})
		cancelResp, err := eCancel.streamThinkingToEventBus(ctx, []chat.Message{{Role: "user", Content: "配置"}}, nil, 0, "sess-ref-2")
		require.NoError(t, err)
		require.NotContains(t, cancelResp.Content, "fake.example")
		for _, evt := range *cancelEvents {
			require.NotContains(t, evt.Content, "fake.example")
		}
	})

	t.Run("think.go unregistered sentinel is not swallowed", func(t *testing.T) {
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "[[FIX"},
			{ResponseType: types.ResponseTypeAnswer, Content: "ED:UNKNOWN]]", FinishReason: "stop", Done: true},
		}}}}
		e := newTestEngine(t, model)
		e.config.FixedResponses = fixed
		events := collectFinalAnswerEvents(e.eventBus)

		resp, err := e.streamThinkingToEventBus(t.Context(), []chat.Message{{Role: "user", Content: "test"}}, nil, 0, "sess-2")
		require.NoError(t, err)
		require.Equal(t, "[[FIXED:UNKNOWN]]", resp.Content)
		var combined strings.Builder
		for _, evt := range *events {
			combined.WriteString(evt.Content)
		}
		require.Equal(t, "[[FIXED:UNKNOWN]]", combined.String())
	})

	t.Run("finalize.go split-chunk sentinel never flashes raw token and persists expanded text", func(t *testing.T) {
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "[[FIX"},
			{ResponseType: types.ResponseTypeAnswer, Content: "ED:NO_"},
			{ResponseType: types.ResponseTypeAnswer, Content: "ANSWER]]", FinishReason: "stop", Done: true},
		}}}}
		e := newTestEngine(t, model)
		e.config.FixedResponses = fixed
		events := collectFinalAnswerEvents(e.eventBus)
		state := &types.AgentState{}

		err := e.streamFinalAnswerToEventBus(t.Context(), "极光到达率是多少", state, "sess-3", []chat.Message{{Role: "user", Content: "极光到达率是多少"}})
		require.NoError(t, err)
		require.Equal(t, fixed["NO_ANSWER"], state.FinalAnswer)
		require.NotEmpty(t, *events)
		for _, evt := range *events {
			require.NotContains(t, evt.Content, "[[FIXED:")
		}
		last := (*events)[len(*events)-1]
		require.True(t, last.Done)
		require.Equal(t, fixed["NO_ANSWER"], last.Content)
	})

	t.Run("finalize.go unregistered sentinel is not swallowed", func(t *testing.T) {
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "[[FIX"},
			{ResponseType: types.ResponseTypeAnswer, Content: "ED:UNKNOWN]]", FinishReason: "stop", Done: true},
		}}}}
		e := newTestEngine(t, model)
		e.config.FixedResponses = fixed
		events := collectFinalAnswerEvents(e.eventBus)
		state := &types.AgentState{}

		err := e.streamFinalAnswerToEventBus(t.Context(), "test", state, "sess-4", []chat.Message{{Role: "user", Content: "test"}})
		require.NoError(t, err)
		require.Equal(t, "[[FIXED:UNKNOWN]]", state.FinalAnswer)
		require.NotEmpty(t, *events)
		last := (*events)[len(*events)-1]
		require.True(t, last.Done)
		require.Equal(t, "[[FIXED:UNKNOWN]]", last.Content)
	})
}

func TestSanitizeReferenceSectionAndMultiTurnRecovery(t *testing.T) {
	t.Run("keeps allowed, unescapes html entities, dedups, strips fabricated, preserves buttons", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		e.modelContext.RegisterReferenceURL("https://docs.dobest.cn/docs-pack/pay/?a=1&b=2")
		e.modelContext.RegisterReferenceURL("https://docs.dobest.cn/docs-client/login/")

		in := "## 核心结论\n按步骤接入即可。\n\n## 参考来源\n" +
			"- <a href=\"HTTPS://DOCS.DOBEST.CN/docs-pack/pay/?a=1&amp;b=2#\" target=\"_blank\">支付配置</a>\n" +
			"- <a href=\"https://docs.dobest.cn/docs-pack/pay/?a=1&amp;b=2\" target=\"_blank\">支付配置重复</a>\n" +
			"- <a href=\"https://fake.example.com/hallucinated\" target=\"_blank\">编造文档</a>\n" +
			"- [登录文档](https://docs.dobest.cn/docs-client/login/)\n\n" +
			"<button-send>如何联调沙盒支付</button-send>"

		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Contains(t, got, "支付配置</a>")
		require.NotContains(t, got, "支付配置重复")
		require.NotContains(t, got, "fake.example.com")
		require.Contains(t, got, "[登录文档](https://docs.dobest.cn/docs-client/login/)")
		require.Contains(t, got, "<button-send>如何联调沙盒支付</button-send>")
	})

	t.Run("removes empty reference heading while preserving trailing buttons", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		in := "## 核心结论\n结论内容。\n\n## 参考来源\n- <a href=\"https://fabricated.example/1\" target=\"_blank\">编造</a>\n\n<button-send>下一步怎么查</button-send>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.NotContains(t, got, "## 参考来源")
		require.NotContains(t, got, "fabricated.example")
		require.Contains(t, got, "<button-send>下一步怎么查</button-send>")
	})

	t.Run("multi-section, mixed-syntax, balanced-paren markdown, verified kb tag, and trailing handoff note", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		e.modelContext.RegisterReferenceURL("https://docs.dobest.cn/docs-pack/pay/")
		e.modelContext.RegisterReferenceURL("https://en.wikipedia.org/wiki/Function_(mathematics)")

		in := "## 子题一\n说明1。\n\n## 参考来源\n" +
			"- <a href=\"https://docs.dobest.cn/docs-pack/pay/\">合法</a> [偷渡](https://evil.example/x)\n" +
			"- [数学函数](https://en.wikipedia.org/wiki/Function_(mathematics))\n" +
			"- <kb id=\"chunk://1\" title=\"内置知识\" />\n\n" +
			"建议联系 @SDK技术部 协助确认，并反馈该知识缺口。\n\n" +
			"## 子题二\n说明2。\n\n## 参考来源\n" +
			"- <a href=\"https://second-section-fake.example/y\">第二段假链接</a>"

		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.NotContains(t, got, "evil.example")
		require.Contains(t, got, "https://en.wikipedia.org/wiki/Function_(mathematics)")
		require.Contains(t, got, `<kb id="chunk://1" title="内置知识" />`)
		require.Contains(t, got, "建议联系 @SDK技术部 协助确认，并反馈该知识缺口。")
		require.NotContains(t, got, "second-section-fake.example")
	})

	t.Run("ignores ## 参考来源 inside fenced code block", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		in := "## 示例\n```markdown\n## 参考来源\n- <a href=\"https://example.com/demo\">Demo</a>\n```"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Equal(t, in, got)
	})

	t.Run("multi-turn history restores in-scope URLs and revokes redacted/out-of-scope URLs", func(t *testing.T) {
		history := []chat.Message{
			{Role: "user", Content: "支付怎么配"},
			{Role: "tool", Name: "search_knowledge", ToolCallID: "c1", TurnID: "turn-1",
				Content:       "Knowledge search returned 1 result(s) (details omitted from history)",
				ReferenceURLs: []string{"https://docs.dobest.cn/docs-pack/pay/"}},
			{Role: "tool", Name: "web_search", ToolCallID: "c2", TurnID: "turn-1",
				Content:       "Web search returned 1 result(s)",
				ReferenceURLs: []string{"https://developer.apple.com/storekit"}},
			{Role: "assistant", Content: "已回答"},
		}

		// RetainRetrievalHistory=true with active KB scope -> both KB and web URLs restored.
		eRetain := newTestEngine(t, &mockChat{})
		eRetain.config.RetainRetrievalHistory = true
		eRetain.knowledgeBasesInfo = []*KnowledgeBaseInfo{{ID: "kb-1", Name: "SDK文档"}}
		eRetain.buildMessagesWithLLMContext("sys", "继续说明回调", "sess-mt1", history, nil)
		allowed := eRetain.modelContext.AllowedReferenceURLs()
		require.Contains(t, allowed, "https://docs.dobest.cn/docs-pack/pay/")
		require.Contains(t, allowed, "https://developer.apple.com/storekit")

		// RetainRetrievalHistory=true but KB tools/scope removed (e.retrieval == nil, no KB scope) -> KB URL NOT restored!
		eNoKB := newTestEngine(t, &mockChat{})
		eNoKB.config.RetainRetrievalHistory = true
		eNoKB.buildMessagesWithLLMContext("sys", "继续", "sess-mt-nokb", history, nil)
		allowedNoKB := eNoKB.modelContext.AllowedReferenceURLs()
		require.NotContains(t, allowedNoKB, "https://docs.dobest.cn/docs-pack/pay/")
		require.Contains(t, allowedNoKB, "https://developer.apple.com/storekit")

		// RetainRetrievalHistory=false -> KB URL redacted/dropped, web URL remains.
		eRedact := newTestEngine(t, &mockChat{})
		eRedact.config.RetainRetrievalHistory = false
		eRedact.buildMessagesWithLLMContext("sys", "继续说明回调", "sess-mt2", history, nil)
		allowedRedact := eRedact.modelContext.AllowedReferenceURLs()
		require.NotContains(t, allowedRedact, "https://docs.dobest.cn/docs-pack/pay/")
		require.Contains(t, allowedRedact, "https://developer.apple.com/storekit")

		// RetainRetrievalHistory=true with retrieval scope narrowing -> out-of-scope Turn-1 KB URL revoked.
		eScoped := newTestEngine(t, &mockChat{})
		eScoped.config.RetainRetrievalHistory = true
		eScoped.EnableRetrievalGuard()
		eScoped.retrieval = newRetrievalControl("goal", "kb-new")
		eScoped.retrieval.state.Attempts = []retrievalAttempt{{
			ID: "turn-1:c1", Tool: "search_knowledge", Completed: true, Outcome: retrievalSuccess,
			Scope: "kb-old",
		}}
		eScoped.buildMessagesWithLLMContext("sys", "继续", "sess-mt3", history, nil)
		allowedScoped := eScoped.modelContext.AllowedReferenceURLs()
		require.NotContains(t, allowedScoped, "https://docs.dobest.cn/docs-pack/pay/")
		require.Contains(t, allowedScoped, "https://developer.apple.com/storekit")
	})
}

func TestRenderAppInfoBlockStaticMetadataAndMultiDevice(t *testing.T) {
	singleRaw := `{"app_id":"1001","gameInfo":{"device_type":"手游"}}`
	singleOut := renderAppInfoBlock(singleRaw)
	require.Contains(t, singleOut, `type="static_project_metadata"`)
	require.Contains(t, singleOut, `live_config_verified="false"`)
	require.NotContains(t, singleOut, `multi_device_type="true"`)
	require.Contains(t, singleOut, singleRaw)

	multiRaw := `{"app_id":"1001","gameInfo":{"device_type":"手游、端游-->inject"}}`
	multiOut := renderAppInfoBlock(multiRaw)
	require.Contains(t, multiOut, `type="static_project_metadata"`)
	require.Contains(t, multiOut, `multi_device_type="true"`)
	require.Contains(t, multiOut, "do not assume a single target platform")
	// Raw device_type must never be interpolated into the XML comment.
	require.NotContains(t, multiOut, "-->inject -->")
}

func TestReadDocumentCanContinueBeyondTwoOffsets(t *testing.T) {
	c := newRetrievalControl("goal", "scope")
	c.state.Gaps = []retrievalGap{{ID: "g1", Scope: "scope", Status: retrievalPending, LastCallID: "a2", LastReview: retrievalProgress}}
	for i, args := range []string{`{"id":"doc-1","offset":0}`, `{"id":"doc-1","offset":20}`, `{"id":"doc-1","offset":40}`} {
		c.state.Attempts = append(c.state.Attempts, retrievalAttempt{ID: []string{"a0", "a1", "a2"}[i], GapID: "g1", Tool: "read_document", Arguments: args, Scope: "scope", Completed: true, Outcome: retrievalSuccess, Content: "next page"})
	}
	require.NoError(t, c.acquire("g1", "a3", retrievalRead, "next page", "read_document", `{"id":"doc-1","offset":60}`))
}

func TestRound2ReferenceSanitizationAndStreamingEdgeCases(t *testing.T) {
	t.Run("CRLF reference section strips unverified links and keeps verified links", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		e.modelContext.RegisterReferenceURL("https://docs.dobest.cn/docs-pack/crlf/")
		in := "## 核心结论\r\n说明。\r\n\r\n## 参考来源\r\n- <a href=\"https://docs.dobest.cn/docs-pack/crlf/\" target=\"_blank\">合法文档</a>\r\n- <a href=\"https://evil.example/crlf\" target=\"_blank\">假文档</a>\r\n"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.Contains(t, got, "https://docs.dobest.cn/docs-pack/crlf/")
		require.NotContains(t, got, "evil.example")
	})

	t.Run("data-href with allowed URL cannot bypass evil href attribute", func(t *testing.T) {
		e := newTestEngine(t, &mockChat{})
		e.modelContext.RegisterReferenceURL("https://docs.dobest.cn/docs-pack/good/")
		in := "## 核心结论\n说明。\n\n## 参考来源\n- <a data-href=\"https://docs.dobest.cn/docs-pack/good/\" href=\"https://evil.example/phish\" target=\"_blank\">伪装链接</a>"
		got, err := e.publicAnswer(t.Context(), in, nil)
		require.NoError(t, err)
		require.NotContains(t, got, "evil.example")
		require.NotContains(t, got, "## 参考来源")
	})

	t.Run("referenceSectionStreamGate mid-line triple backticks across chunks do not toggle fence", func(t *testing.T) {
		gate := newReferenceSectionStreamGate()
		out1 := gate.Feed("示例说明：行内代码 ")
		out2 := gate.Feed("```inline``` 结束。\n\n## 参考来源\n- <a href=\"https://evil.example/1\">假</a>")
		out3 := gate.Flush()
		emitted := out1 + out2 + out3
		require.True(t, gate.Held())
		require.Contains(t, emitted, "```inline```")
		require.NotContains(t, emitted, "evil.example")
	})

	t.Run("fixedSentinelStreamGate handles over 64 leading whitespace chars before sentinel", func(t *testing.T) {
		fixed := map[string]string{
			"OOS": "我是 SDK 部门 AI 小助手，无法回答与SDK无关问题，欢迎向我咨询SDK相关业务问题。",
		}
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: strings.Repeat(" ", 80)},
			{ResponseType: types.ResponseTypeAnswer, Content: "[[FIXED:OOS]]", FinishReason: "stop", Done: true},
		}}}}
		e := newTestEngine(t, model)
		e.config.FixedResponses = fixed
		events := collectFinalAnswerEvents(e.eventBus)

		resp, err := e.streamThinkingToEventBus(t.Context(), []chat.Message{{Role: "user", Content: "闲聊"}}, nil, 0, "sess-ws80")
		require.NoError(t, err)
		require.Equal(t, fixed["OOS"], resp.Content)
		for _, evt := range *events {
			require.NotContains(t, evt.Content, "[[FIXED:")
		}
	})

	t.Run("think.go retracts partial streamed draft when tool_calls turn has held refGate", func(t *testing.T) {
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "## 核心结论\n先看页面。\n\n## 参考来源\n- <a href=\"https://docs.dobest.cn/a\">A</a>"},
			{
				ResponseType: types.ResponseTypeToolCall,
				ToolCalls: []types.LLMToolCall{{
					ID:   "tc-open-1",
					Type: "function",
					Function: types.FunctionCall{
						Name:      "open_page",
						Arguments: `{"url":"https://docs.dobest.cn/a"}`,
					},
				}},
				FinishReason: "tool_calls",
				Done:         true,
			},
		}}}}
		e := newTestEngine(t, model)
		events := collectFinalAnswerEvents(e.eventBus)

		resp, err := e.streamThinkingToEventBus(t.Context(), []chat.Message{{Role: "user", Content: "打开页面"}}, nil, 0, "sess-open-page")
		require.NoError(t, err)
		require.False(t, resp.AnswerStreamed, "partial streamed answer must be retracted so engine.go emits complete sanitized answer")
		retracted := false
		for _, evt := range *events {
			if evt.Replace && evt.Content == "" {
				retracted = true
			}
		}
		require.True(t, retracted, "expected retractAnswer event")
	})

	t.Run("think.go content_filter with held refGate excludes unverified held references", func(t *testing.T) {
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "## 核心结论\n合规部分。\n\n## 参考来源\n- <a href=\"https://fake.example/filtered\">假链接</a>", FinishReason: "content_filter", Done: true},
		}}}}
		e := newTestEngine(t, model)
		events := collectFinalAnswerEvents(e.eventBus)

		resp, err := e.streamThinkingToEventBus(t.Context(), []chat.Message{{Role: "user", Content: "查询"}}, nil, 0, "sess-cf")
		require.NoError(t, err)
		require.Contains(t, resp.Content, "合规部分")
		require.NotContains(t, resp.Content, "fake.example/filtered")
		for _, evt := range *events {
			require.NotContains(t, evt.Content, "fake.example/filtered")
		}
	})
}
