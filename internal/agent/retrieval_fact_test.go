package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetrievalIndependentFactsMayShareUserQuote(t *testing.T) {
	c := newRetrievalControl("比较两个渠道的沙盒支持情况和配置方法", "scope")
	support, err := c.register(retrievalFactProposal{GoalQuote: "沙盒支持情况和配置方法", Fact: "沙盒支持情况"})
	require.NoError(t, err)
	configuration, err := c.register(retrievalFactProposal{
		GoalQuote: "沙盒支持情况和配置方法", Fact: "沙盒配置方法", DependsOn: []string{support},
		DistinctFrom: []retrievalDistinction{{GapID: support, Reason: "支持与否不能替代具体配置步骤"}},
	})
	require.NoError(t, err)
	require.NotEqual(t, support, configuration)
	require.Error(t, c.acquire(configuration, "premature", retrievalInitial, "", "search", `{}`))
	require.NoError(t, c.acquire(support, "support", retrievalInitial, "", "search", `{}`))
	require.NoError(t, c.observe("support", "两个渠道均支持沙盒", retrievalSuccess))
	require.NoError(t, c.review(support, "support", retrievalSatisfied, []string{"两个渠道均支持沙盒"}))
	require.NoError(t, c.acquire(configuration, "configuration", retrievalInitial, "", "search", `{}`))
	for _, attempt := range c.snapshot().Attempts {
		require.NotEqual(t, "premature", attempt.ID, "dependency rejection must not dispatch or consume an allowance")
	}
}

func TestRetrievalFactIdentityIncludesGroundedConditions(t *testing.T) {
	c := newRetrievalControl("比较 iOS 和 Google 的沙盒配置", "scope")
	ios, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration steps", Conditions: "iOS", ConditionQuote: "iOS"})
	require.NoError(t, err)
	google, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration steps", Conditions: "Google", ConditionQuote: "Google",
		DistinctFrom: []retrievalDistinction{{GapID: ios, Reason: "用户要求比较两个平台的配置步骤"}}})
	require.NoError(t, err)
	require.NotEqual(t, ios, google, "the same fact label can have different explicitly supported applicability conditions")
	again, err := c.register(retrievalFactProposal{GoalQuote: "iOS", Fact: " CONFIGURATION  steps ", Conditions: "ios", ConditionQuote: "iOS"})
	require.NoError(t, err)
	require.Equal(t, ios, again)
	_, err = c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration steps", Conditions: "other", ConditionQuote: "not supplied"})
	require.Error(t, err)
	_, err = c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration steps", Conditions: "other"})
	require.Error(t, err)
	require.Len(t, c.snapshot().Gaps, 2)
	s := c.snapshot()
	s.Gaps[1].ConditionQuote = "invented"
	_, err = restoreRetrievalControl(s)
	require.Error(t, err)
}

func TestRetrievalPrerequisiteCanUseEarlierAvailableEvidence(t *testing.T) {
	c := newRetrievalControl("沙盒配置", "scope")
	parent, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(parent, "first", retrievalInitial, "", "search", `{}`))
	require.NoError(t, c.observe("first", "需要测试账号；下一节列配置步骤", retrievalSuccess))
	require.NoError(t, c.review(parent, "first", retrievalProgress, []string{"需要测试账号"}))
	require.NoError(t, c.acquire(parent, "read", retrievalRead, "配置步骤", "read_document", `{}`))
	require.NoError(t, c.observe("read", "配置步骤详见控制台说明", retrievalSuccess))
	child, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "账号前提",
		DistinctFrom: []retrievalDistinction{{GapID: parent, Reason: "首份资料仍适用，账号是配置步骤之外的必要前提"}},
		Source:       &retrievalFactSource{CallID: "first", Quote: "需要测试账号"}})
	require.NoError(t, err, "a later read does not invalidate earlier applicable evidence")
	require.Equal(t, []string{child}, c.snapshot().Gaps[0].DependsOn)
}

func TestRetrievalDiscoveredPrerequisitePreservesParentBudget(t *testing.T) {
	c := newRetrievalControl("沙盒怎么配置", "scope")
	parent, err := c.register(retrievalFactProposal{GoalQuote: "沙盒怎么配置", Fact: "配置步骤"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(parent, "first", retrievalInitial, "", "search", `{}`))
	require.NoError(t, c.observe("first", "未找到配置步骤，测试账号是下一条线索", retrievalSuccess))
	require.NoError(t, c.review(parent, "first", retrievalNoProgress, nil))
	require.NoError(t, c.acquire(parent, "supplement", retrievalSupplement, "测试账号", "search", `{"q":"account"}`))
	require.NoError(t, c.observe("supplement", "配置前必须核实测试账号的地区限制", retrievalSuccess))
	require.NoError(t, c.review(parent, "supplement", retrievalProgress, []string{"测试账号的地区限制"}))
	proposal := retrievalFactProposal{
		GoalQuote: "沙盒怎么配置", Fact: "测试账号的地区限制",
		DistinctFrom: []retrievalDistinction{{GapID: parent, Reason: "资料指出账号地区是配置前置条件，配置步骤不能替代地区限制"}},
		Source:       &retrievalFactSource{CallID: "supplement", Quote: "配置前必须核实测试账号的地区限制"},
	}
	child, err := c.register(proposal)
	require.NoError(t, err, "a documented prerequisite need not appear verbatim in the user's task")
	require.Equal(t, []string{child}, c.snapshot().Gaps[0].DependsOn)
	require.Error(t, c.acquire(parent, "blocked", retrievalRead, "地区限制", "read_document", `{}`))
	require.NoError(t, c.acquire(child, "child", retrievalInitial, "", "search", `{}`))
	require.NoError(t, c.observe("child", "地区限制已满足", retrievalSuccess))
	require.NoError(t, c.review(child, "child", retrievalSatisfied, []string{"地区限制已满足"}))
	encoded, err := json.Marshal(c.snapshot())
	require.NoError(t, err)
	var state retrievalState
	require.NoError(t, json.Unmarshal(encoded, &state))
	restored, err := restoreRetrievalControl(state)
	require.NoError(t, err)
	restored.restoreObservation("supplement", "配置前必须核实测试账号的地区限制")
	require.Error(t, restored.acquire(parent, "invisible", retrievalRead, "地区限制", "read_document", `{}`), "a historical satisfied label is not visible evidence")
	redacted, err := restoreRetrievalControl(state)
	require.NoError(t, err)
	redacted.restoreObservation("supplement", "配置前必须核实测试账号的地区限制")
	redacted.restoreObservation("child", "Historical result removed; search again if necessary.")
	require.Error(t, redacted.acquire(parent, "redacted", retrievalRead, "地区限制", "read_document", `{}`), "a nonempty redaction marker cannot satisfy a prerequisite")
	restored.restoreObservation("child", "地区限制已满足")
	require.NoError(t, restored.acquire(parent, "read", retrievalRead, "地区限制", "read_document", `{}`))
	require.True(t, restored.snapshot().Gaps[0].SupplementUsed, "a derived fact never refunds its parent's allowance")
	require.NoError(t, restored.observe("read", "仍无配置步骤", retrievalSuccess))
	require.NoError(t, restored.review(parent, "read", retrievalNoProgress, nil))
	require.Equal(t, retrievalExhausted, restored.snapshot().Gaps[0].Status)
}

func TestRetrievalPrerequisiteRejectsFabricatedOrClosedOrigins(t *testing.T) {
	for _, reason := range []string{"unknown call", "invented quote", "failed call", "closed parent", "cycle", "unknown distinction"} {
		t.Run(reason, func(t *testing.T) {
			c := newRetrievalControl("沙盒配置", "scope")
			parent, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration"})
			require.NoError(t, err)
			require.NoError(t, c.acquire(parent, "first", retrievalInitial, "", "search", `{}`))
			outcome := retrievalSuccess
			if reason == "failed call" {
				outcome = retrievalTransient
			}
			require.NoError(t, c.observe("first", "需要测试账号", outcome))
			proposal := retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "账号前提",
				DistinctFrom: []retrievalDistinction{{GapID: parent, Reason: "配置前需要核对账号前提"}},
				Source:       &retrievalFactSource{CallID: "first", Quote: "需要测试账号"}}
			switch reason {
			case "unknown call":
				proposal.Source.CallID = "invented"
			case "invented quote":
				proposal.Source.Quote = "另一个前提"
			case "closed parent":
				require.NoError(t, c.review(parent, "first", retrievalExhausted, nil))
			case "cycle":
				proposal.DependsOn = []string{parent}
			case "unknown distinction":
				proposal.DistinctFrom[0].GapID = "gap-99"
			}
			before := c.snapshot()
			_, err = c.register(proposal)
			require.Error(t, err)
			require.Equal(t, before, c.snapshot(), "invalid declarations must not alter identities, dependencies or allowances")
		})
	}
}

func TestRetrievalSnapshotValidatesAndCopiesFactLinks(t *testing.T) {
	c := newRetrievalControl("沙盒配置", "scope")
	parent, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(parent, "first", retrievalInitial, "", "search", `{}`))
	require.NoError(t, c.observe("first", "需要测试账号", retrievalSuccess))
	child, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "账号前提",
		DistinctFrom: []retrievalDistinction{{GapID: parent, Reason: "账号是配置前提"}},
		Source:       &retrievalFactSource{CallID: "first", Quote: "需要测试账号"}})
	require.NoError(t, err)
	before := c.snapshot()
	for name, corrupt := range map[string]func(*retrievalState){
		"cycle":               func(s *retrievalState) { s.Gaps[1].DependsOn = []string{parent} },
		"unknown dependency":  func(s *retrievalState) { s.Gaps[0].DependsOn[0] = "missing" },
		"missing link":        func(s *retrievalState) { s.Gaps[0].DependsOn = nil },
		"invented origin":     func(s *retrievalState) { s.Gaps[1].Origins[0].CallID = "missing" },
		"invented quote":      func(s *retrievalState) { s.Gaps[1].Origins[0].Quote = "never returned" },
		"unknown distinction": func(s *retrievalState) { s.Gaps[1].DistinctFrom[0].GapID = child },
		"lost origin":         func(s *retrievalState) { s.Gaps[1].Origins = nil },
	} {
		t.Run(name, func(t *testing.T) {
			s := c.snapshot()
			corrupt(&s)
			_, err := restoreRetrievalControl(s)
			require.Error(t, err)
			require.Equal(t, before, c.snapshot(), "snapshots must not alias the live graph")
		})
	}
}

func TestRetrievalDerivedFactCannotContinueAfterParentStops(t *testing.T) {
	for _, status := range []retrievalStatus{retrievalNeedsUser, retrievalExhausted} {
		t.Run(string(status), func(t *testing.T) {
			c := newRetrievalControl("沙盒配置", "scope")
			parent, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration"})
			require.NoError(t, err)
			require.NoError(t, c.acquire(parent, "first", retrievalInitial, "", "search", `{}`))
			require.NoError(t, c.observe("first", "需要测试账号", retrievalSuccess))
			child, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "账号前提",
				DistinctFrom: []retrievalDistinction{{GapID: parent, Reason: "账号是配置前提"}},
				Source:       &retrievalFactSource{CallID: "first", Quote: "需要测试账号"}})
			require.NoError(t, err)
			require.NoError(t, c.review(parent, "first", status, nil))
			require.Error(t, c.acquire(child, "child", retrievalInitial, "", "search", `{}`))
			require.Len(t, c.snapshot().Attempts, 1)
		})
	}
}

func TestRetrievalDerivedChainStopsWithItsOriginalGoal(t *testing.T) {
	c := newRetrievalControl("沙盒配置", "scope")
	root, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration"})
	require.NoError(t, err)
	require.NoError(t, c.acquire(root, "root", retrievalInitial, "", "search", `{}`))
	require.NoError(t, c.observe("root", "需要测试账号", retrievalSuccess))
	child, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "账号前提",
		DistinctFrom: []retrievalDistinction{{GapID: root, Reason: "账号是配置前提"}},
		Source:       &retrievalFactSource{CallID: "root", Quote: "需要测试账号"}})
	require.NoError(t, err)
	require.NoError(t, c.acquire(child, "child", retrievalInitial, "", "search", `{}`))
	require.NoError(t, c.observe("child", "账号依赖地区条件", retrievalSuccess))
	grandchild, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "地区条件",
		DistinctFrom: []retrievalDistinction{{GapID: root, Reason: "地区条件不是配置步骤"}, {GapID: child, Reason: "资料要求先确定地区条件"}},
		Source:       &retrievalFactSource{CallID: "child", Quote: "账号依赖地区条件"}})
	require.NoError(t, err)
	require.NoError(t, c.review(root, "root", retrievalNeedsUser, nil))
	require.Error(t, c.acquire(grandchild, "grandchild", retrievalInitial, "", "search", `{}`))
	require.Len(t, c.snapshot().Attempts, 2)
}

// A prerequisite quote is matched like review evidence, both when it is registered
// and when the ledger is restored: a stricter restore would reject a saved ledger.
func TestRetrievalPrerequisiteQuoteMatchesLikeEvidence(t *testing.T) {
	for _, tc := range []struct{ name, content, quote string }{
		{"verbatim", "需要测试账号；下一节列配置步骤", "需要测试账号"},
		{"xml arrows", `<content>微信公众平台 -&gt; 设置 -&gt; 测试账号</content>`, "微信公众平台 -> 设置 -> 测试账号"},
		{"middle emphasis", `<content>配置前**必须先申请**测试账号。</content>`, "配置前必须先申请测试账号。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRetrievalControl("沙盒配置", "scope")
			parent, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration"})
			require.NoError(t, err)
			require.NoError(t, c.acquire(parent, "first", retrievalInitial, "", "search", `{}`))
			require.NoError(t, c.observe("first", tc.content, retrievalSuccess))
			require.NoError(t, c.review(parent, "first", retrievalProgress, []string{tc.quote}), "review accepts the quote")
			_, err = c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "账号前提",
				DistinctFrom: []retrievalDistinction{{GapID: parent, Reason: "配置前需要核对账号前提"}},
				Source:       &retrievalFactSource{CallID: "first", Quote: tc.quote}})
			require.NoError(t, err, "a quote review accepts is also a valid prerequisite source")
			_, err = restoreRetrievalControl(c.snapshot())
			require.NoError(t, err, "restore accepts what registration accepted")
		})
	}
}

// A condition grounded in the cited prerequisite quote is matched like evidence too:
// the quote may be copied raw while the condition is worded as it reads.
func TestRetrievalConditionQuoteMatchesLikeEvidence(t *testing.T) {
	for _, tc := range []struct{ name, sourceQuote, condition string }{
		{"verbatim", "企业账号需要测试账号", "企业账号"},
		{"xml arrows", "微信公众平台 -&gt; 设置 -&gt; 测试账号", "微信公众平台 -> 设置"},
		{"middle emphasis", "**仅限**企业账号申请测试账号", "仅限企业账号"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRetrievalControl("沙盒配置", "scope")
			parent, err := c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "configuration"})
			require.NoError(t, err)
			require.NoError(t, c.acquire(parent, "first", retrievalInitial, "", "search", `{}`))
			require.NoError(t, c.observe("first", tc.sourceQuote, retrievalSuccess))
			_, err = c.register(retrievalFactProposal{GoalQuote: "沙盒配置", Fact: "账号前提", Conditions: tc.condition, ConditionQuote: tc.condition,
				DistinctFrom: []retrievalDistinction{{GapID: parent, Reason: "配置前需要核对账号前提"}},
				Source:       &retrievalFactSource{CallID: "first", Quote: tc.sourceQuote}})
			require.NoError(t, err, "a condition worded like the cited quote is grounded")
			_, err = restoreRetrievalControl(c.snapshot())
			require.NoError(t, err, "restore accepts what registration accepted")
		})
	}
}
