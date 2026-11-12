package agent

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetrievalGapIdentityAndIndependentGoals(t *testing.T) {
	control := newRetrievalControl("比较登录方式；解释支付配置", "bound-scope")
	login, err := control.register(retrievalFactProposal{GoalQuote: "比较登录方式", Fact: "登录方式的区别"})
	require.NoError(t, err)
	_, err = control.register(retrievalFactProposal{GoalQuote: "比较登录方式", Fact: "换个说法查询登录差异"})
	require.Error(t, err, "renaming needs a specific independently required fact, not another allowance")
	reused, err := control.register(retrievalFactProposal{GoalQuote: "登录方式", Fact: "登录方式的区别"})
	require.NoError(t, err)
	require.Equal(t, login, reused)
	payment, err := control.register(retrievalFactProposal{GoalQuote: "解释支付配置", Fact: "支付配置",
		DistinctFrom: []retrievalDistinction{{GapID: login, Reason: "用户另问支付配置，与登录方式比较分别回答"}}})
	require.NoError(t, err)
	require.NotEqual(t, login, payment)
	_, err = control.register(retrievalFactProposal{GoalQuote: "不存在的用户目标", Fact: "background"})
	require.Error(t, err)
	_, err = control.register(retrievalFactProposal{GoalQuote: "登录方式", Fact: "rename using a smaller quote"})
	require.Error(t, err, "another quote does not distinguish an independently needed fact")
}

func TestRetrievalSupplementAndRecoverySharePersistentBudgets(t *testing.T) {
	for _, recoveryFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "supplement then recovery", true: "recovery then supplement"}[recoveryFirst], func(t *testing.T) {
			control := newRetrievalControl("TapTap 沙盒支付配置", "scope")
			gap, err := control.register(retrievalFactProposal{GoalQuote: "TapTap 沙盒支付配置", Fact: "configuration"})
			require.NoError(t, err)
			require.NoError(t, control.acquire(gap, "initial", retrievalInitial, "", "search", `{"query":"沙盒设置"}`))
			if recoveryFirst {
				require.NoError(t, control.observe("initial", "", retrievalTransient))
				require.NoError(t, control.acquire(gap, "recovery", retrievalRecovery, "", "search", `{"query":"沙盒设置"}`))
				require.NoError(t, control.observe("recovery", "未找到配置", retrievalSuccess))
				require.NoError(t, control.review(gap, "recovery", retrievalNoProgress, nil))
			} else {
				require.NoError(t, control.observe("initial", "未找到配置", retrievalSuccess))
				require.NoError(t, control.review(gap, "initial", retrievalNoProgress, nil))
			}
			require.NoError(t, control.acquire(gap, "supplement", retrievalSupplement, "TapTap", "search", `{"query":"TapTap 测试支付"}`))
			require.NoError(t, control.observe("supplement", "", retrievalTransient))
			if !recoveryFirst {
				require.NoError(t, control.acquire(gap, "recovery", retrievalRecovery, "", "search", `{"query":"TapTap 测试支付"}`))
				require.NoError(t, control.observe("recovery", "仍未找到配置", retrievalSuccess))
				require.NoError(t, control.review(gap, "recovery", retrievalNoProgress, nil))
			}
			encoded, err := json.Marshal(control.snapshot())
			require.NoError(t, err)
			var snapshot retrievalState
			require.NoError(t, json.Unmarshal(encoded, &snapshot))
			restored, err := restoreRetrievalControl(snapshot)
			require.NoError(t, err)
			for _, action := range []retrievalAction{retrievalInitial, retrievalSupplement, retrievalRecovery, retrievalRead} {
				require.Error(t, restored.acquire(gap, "again", action, "TapTap", "other-reader", `{}`))
			}
			stored := restored.snapshot().Gaps[0]
			require.True(t, stored.SupplementUsed)
			require.True(t, stored.RecoveryUsed)
		})
	}
}

func TestRetrievalProgressRequiresObservedNewEvidence(t *testing.T) {
	control := newRetrievalControl("如何配置沙盒", "scope")
	gap, err := control.register(retrievalFactProposal{GoalQuote: "如何配置沙盒", Fact: "configuration"})
	require.NoError(t, err)
	require.NoError(t, control.acquire(gap, "first", retrievalInitial, "", "search", `{}`))
	require.NoError(t, control.observe("first", "沙盒配置见测试账号小节", retrievalSuccess))
	require.Error(t, control.review(gap, "invented-call", retrievalProgress, []string{"测试账号"}))
	require.Error(t, control.review(gap, "first", retrievalProgress, []string{"不存在的引用"}))
	require.NoError(t, control.review(gap, "first", retrievalProgress, []string{"测试账号"}))
	require.NoError(t, control.acquire(gap, "read", retrievalRead, "测试账号", "read_document", `{"id":"doc"}`))
	require.NoError(t, control.observe("read", "沙盒配置见测试账号小节", retrievalSuccess))
	require.Error(t, control.review(gap, "read", retrievalProgress, []string{"测试账号"}), "same cited content cannot claim new progress")
	require.Error(t, control.review(gap, "read", retrievalProgress, []string{"沙盒配置"}), "a different quote from previously observed content is not new evidence")
	require.NoError(t, control.review(gap, "read", retrievalNoProgress, nil))
	require.Error(t, control.acquire(gap, "again", retrievalRead, "测试账号", "read_document", `{}`), "a no-progress result cannot be mislabeled as a necessary read")
}

func TestRetrievalTerminalStatesBlockOnlyTheirOwnGap(t *testing.T) {
	for _, status := range []retrievalStatus{retrievalSatisfied, retrievalNeedsUser, retrievalExhausted} {
		t.Run(string(status), func(t *testing.T) {
			control := newRetrievalControl("登录说明；支付说明", "scope")
			first, err := control.register(retrievalFactProposal{GoalQuote: "登录说明", Fact: "login"})
			require.NoError(t, err)
			second, err := control.register(retrievalFactProposal{GoalQuote: "支付说明", Fact: "payment",
				DistinctFrom: []retrievalDistinction{{GapID: first, Reason: "payment is a separate requested topic"}}})
			require.NoError(t, err)
			require.NoError(t, control.acquire(first, "call", retrievalInitial, "", "search", `{}`))
			require.NoError(t, control.observe("call", "登录要求账号", retrievalSuccess))
			require.NoError(t, control.review(first, "call", status, []string{"登录要求账号"}))
			require.Error(t, control.acquire(first, "blocked", retrievalSupplement, "登录", "search", `{}`))
			require.NoError(t, control.acquire(second, "independent", retrievalInitial, "", "search", `{}`))
		})
	}
}

func TestRetrievalConcurrentCallsDoNotOverspend(t *testing.T) {
	control := newRetrievalControl("支付未到账", "scope")
	gap, err := control.register(retrievalFactProposal{GoalQuote: "支付未到账", Fact: "payment"})
	require.NoError(t, err)
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- control.acquire(gap, id, retrievalInitial, "", "search", `{}`)
		}()
	}
	wait.Wait()
	close(results)
	allowed := 0
	for err := range results {
		if err == nil {
			allowed++
		}
	}
	require.Equal(t, 1, allowed)
}

func TestRetrievalRestoreRejectsLostBudgetOrIdentity(t *testing.T) {
	control := newRetrievalControl("支付配置", "scope")
	gap, err := control.register(retrievalFactProposal{GoalQuote: "支付配置", Fact: "payment"})
	require.NoError(t, err)
	require.NoError(t, control.acquire(gap, "first", retrievalInitial, "", "search", `{}`))
	require.NoError(t, control.observe("first", "未找到", retrievalSuccess))
	require.NoError(t, control.review(gap, "first", retrievalNoProgress, nil))
	require.NoError(t, control.acquire(gap, "supplement", retrievalSupplement, "支付", "search", `{}`))
	for name, corrupt := range map[string]func(*retrievalState){
		"lost budget":       func(s *retrievalState) { s.Gaps[0].SupplementUsed = false },
		"unknown last call": func(s *retrievalState) { s.Gaps[0].LastCallID = "missing" },
		"duplicate gap":     func(s *retrievalState) { s.Gaps = append(s.Gaps, s.Gaps[0]) },
		"invalid state":     func(s *retrievalState) { s.Gaps[0].Status = "anything" },
		"invalid identity":  func(s *retrievalState) { s.Gaps[0].ID = "gap-2" },
	} {
		t.Run(name, func(t *testing.T) {
			s := control.snapshot()
			corrupt(&s)
			_, err := restoreRetrievalControl(s)
			require.Error(t, err)
		})
	}
	restored, err := restoreRetrievalControl(control.snapshot())
	require.NoError(t, err)
	require.Equal(t, retrievalUnavailable, restored.snapshot().Gaps[0].Status)
	require.True(t, restored.snapshot().Gaps[0].SupplementUsed)
	require.Error(t, restored.acquire(gap, "new-id", retrievalInitial, "", "search", `{}`), "an unknown dispatched result cannot become a fresh search")
}

func TestRetrievalErrorAndDecisionBudgetsAreSeparate(t *testing.T) {
	control := newRetrievalControl("code 123", "scope")
	gap, err := control.register(retrievalFactProposal{GoalQuote: "code 123", Fact: "error meaning"})
	require.NoError(t, err)
	require.NoError(t, control.acquire(gap, "first", retrievalInitial, "", "search", `{"bad":"123"}`))
	require.NoError(t, control.observe("first", "query required", retrievalInput))
	require.Error(t, control.acquire(gap, "bad-retry", retrievalRecovery, "guessed", "search", `{"query":"456"}`))
	require.False(t, control.snapshot().Gaps[0].RecoveryUsed, "a rejected request did not execute")
	require.NoError(t, control.acquire(gap, "fixed", retrievalRecovery, "123", "search", `{"query":"123"}`))
	require.NoError(t, control.observe("fixed", "", retrievalSuccess))
	require.NoError(t, control.review(gap, "fixed", retrievalNoProgress, nil))
	require.Error(t, control.acquire(gap, "empty-is-not-error", retrievalRecovery, "123", "search", `{}`))
	require.True(t, control.takeDecisionCorrection())
	require.False(t, control.takeDecisionCorrection())
	restored, err := restoreRetrievalControl(control.snapshot())
	require.NoError(t, err)
	require.False(t, restored.takeDecisionCorrection())
	require.NoError(t, restored.acquire(gap, "supplement", retrievalSupplement, "123", "search", `{}`))
	require.NoError(t, restored.observe("supplement", "permission denied", retrievalUnknown))
	require.Equal(t, retrievalUnavailable, restored.snapshot().Gaps[0].Status)
}
