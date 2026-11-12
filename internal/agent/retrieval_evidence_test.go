package agent

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetrievalEvidencePresentation(t *testing.T) {
	for _, tc := range []struct {
		name, source, quote string
		match               bool
	}{
		{"xml arrows", `<content>微信公众平台 -&gt; 设置 -&gt; 用户隐私保护指引</content>`, "微信公众平台 -> 设置 -> 用户隐私保护指引", true},
		{"bold", `<content>**这是前提**——若后台未配置，无法拉起。</content>`, "这是前提——若后台未配置，无法拉起。", true},
		{"inline code", "<content>传入参数 `privacyAuthorizeMode` 的值。</content>", "传入参数 privacyAuthorizeMode 的值。", true},
		{"middle emphasis", `<content>SDK 会在小游戏**初始化完成后，自动调用**微信官方弹窗。</content>`, "SDK 会在小游戏初始化完成后，自动调用微信官方弹窗。", true},
		{"nested emphasis", "***必须***配置", "必须配置", true},
		{"changed version", "版本 **1.8.0**", "版本 1.9.0", false},
		{"changed negation", "iOS **不支持**沙盒支付", "iOS 支持沙盒支付", false},
		{"changed platform", "**iOS** 支付失败", "Android 支付失败", false},
		{"changed operator", "费用 `< 5`", "费用 > 5", false},
		{"code literals", "代码 `a**b**c`", "代码 abc", false},
		{"escaped markup", `保留 \*\*原文\*\*`, "保留 原文", false},
		{"cross document", `<content>必须</content><content>配置</content>`, "必须配置", false},
		{"blank", "text", " ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &retrievalAttempt{Completed: true, Outcome: retrievalSuccess, Content: tc.source}
			require.Equal(t, tc.match, validateEvidenceQuotes(a, []string{tc.quote}, true) == nil)
		})
	}
}

func TestRetrievalEvidenceReviewsKeepRawSourceAndRejectFalseProgress(t *testing.T) {
	c := newRetrievalControl("微信小游戏的协议如何配置？", "scope")
	id, err := c.register(retrievalFactProposal{GoalQuote: "微信小游戏的协议如何配置？", Fact: "协议配置"})
	require.NoError(t, err)
	const source = "<content>微信公众平台 -&gt; 设置。传入参数 `privacyAuthorizeMode`。</content>"
	quotes := []string{"微信公众平台 -> 设置。", "传入参数 privacyAuthorizeMode。"}
	require.NoError(t, c.acquire(id, "search", retrievalInitial, "", "search_knowledge", `{}`))
	require.NoError(t, c.observe("search", source, retrievalSuccess))
	require.NoError(t, c.applyReview(retrievalReview{GapID: id, CallID: "search", Status: retrievalProgress, Quotes: quotes}))
	require.NoError(t, c.acquire(id, "read", retrievalRead, "微信公众平台 -> 设置。", "read_document", `{"id":"doc"}`))
	require.NoError(t, c.observe("read", source, retrievalSuccess))
	require.ErrorContains(t, c.applyReview(retrievalReview{GapID: id, CallID: "read", Status: retrievalProgress, Quotes: []string{"传入参数 `privacyAuthorizeMode`。"}}), "newly cited evidence")
	require.NoError(t, c.applyReview(retrievalReview{GapID: id, CallID: "read", Status: retrievalSatisfied, Quotes: quotes}))
	require.Equal(t, source, c.snapshot().Attempts[1].Content)
	restored, err := restoreRetrievalControl(c.snapshot())
	require.NoError(t, err)
	require.True(t, restored.evidenceAvailable(restored.gap(id)))
}

func TestRetrievalEvidenceErrorIdentifiesQuote(t *testing.T) {
	a := &retrievalAttempt{Completed: true, Outcome: retrievalSuccess, Content: "版本 **1.8.0**"}
	err := validateEvidenceQuotes(a, []string{"版本 1.8.0", "版本 1.9.0"}, true)
	require.ErrorContains(t, err, "quotes[1]")
}

func TestRetrievalEvidenceRecordedWechatSession(t *testing.T) {
	data, err := os.ReadFile("testdata/retrieval_wechat_privacy_quotes.json")
	require.NoError(t, err)
	var fixture struct {
		Source  string            `json:"source"`
		Reviews []retrievalReview `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	c := newRetrievalControl("微信小游戏的协议如何配置？", "scope")
	id, err := c.register(retrievalFactProposal{GoalQuote: "微信小游戏的协议如何配置？", Fact: "协议配置"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(id, "search", retrievalInitial, "", "search_knowledge", `{}`))
	require.NoError(t, c.observe("search", fixture.Source, retrievalSuccess))
	for _, review := range fixture.Reviews {
		review.GapID, review.CallID = id, "search"
		require.NoError(t, c.applyReview(review), "recorded %s with %d quotes", review.Status, len(review.Quotes))
	}
	require.True(t, c.evidenceAvailable(c.gap(id)))
	require.Equal(t, fixture.Source, c.snapshot().Attempts[0].Content)
}

func TestRetrievalEvidenceMultipleSourcesKeepProvenance(t *testing.T) {
	c := newRetrievalControl("账号标识与互通", "scope")
	id, err := c.register(retrievalFactProposal{GoalQuote: "账号标识与互通", Fact: "账号规则"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(id, "one", retrievalInitial, "", "search_knowledge", `{}`))
	require.NoError(t, c.observe("one", "完整 userId 全渠道唯一", retrievalSuccess))
	require.NoError(t, c.review(id, "one", retrievalProgress, []string{"完整 userId 全渠道唯一"}))
	require.NoError(t, c.acquire(id, "two", retrievalRead, "账号", "read_document", `{"id":"two"}`))
	require.NoError(t, c.observe("two", "同主体下可按配置互通", retrievalSuccess))
	sources := []retrievalEvidence{{CallID: "one", Quotes: []string{"完整 userId 全渠道唯一"}}, {CallID: "two", Quotes: []string{"同主体下可按配置互通"}}}
	before := c.snapshot()
	require.Error(t, c.applyReview(retrievalReview{GapID: id, CallID: "one", Status: retrievalSatisfied, Quotes: sources[1].Quotes}))
	require.Equal(t, before, c.snapshot())
	require.NoError(t, c.applyReview(retrievalReview{GapID: id, Status: retrievalSatisfied, Evidence: sources}))
	require.True(t, c.evidenceAvailable(c.gap(id)))
	state := c.snapshot()
	sources[0].Quotes[0] = "invented"
	require.Equal(t, "完整 userId 全渠道唯一", state.Gaps[0].EvidenceSources[0].Quotes[0])
	restored, err := restoreRetrievalControl(state)
	require.NoError(t, err)
	require.True(t, restored.evidenceAvailable(restored.gap(id)))
	state.Gaps[0].EvidenceSources[1].CallID = "one"
	_, err = restoreRetrievalControl(state)
	require.Error(t, err)
}
