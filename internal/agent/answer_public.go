package agent

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// publicAnswer degrades optional guard failures to the cleaned original answer.
// Cancellation and persistence failures remain fatal.
func (e *AgentEngine) publicAnswer(ctx context.Context, content string, messages []chat.Message) (string, error) {
	answer, err := e.publicAnswerChecked(ctx, content, messages)
	if err != nil && ctx.Err() == nil && !errors.Is(err, errRetrievalStateSave) {
		logger.Warnf(ctx, "[Agent][PublicAnswer] degraded to original answer: %v", err)
		clean := tools.StripThinkBlocks(content)
		if strings.TrimSpace(clean) == "" {
			clean = stalledAnswerFallback
		}
		answer, err = e.enforceMissingConditions(ctx, clean), nil
	}
	if err != nil {
		return answer, err
	}
	return dropContradictedButtons(answer), nil
}

func (e *AgentEngine) publicAnswerChecked(ctx context.Context, content string, messages []chat.Message) (answer string, err error) {
	defer func() {
		if err == nil {
			if ctx.Err() != nil {
				answer, err = "", ctx.Err()
				return
			}
			answer = e.enforceMissingConditions(ctx, answer)
			if ctx.Err() != nil {
				answer, err = "", ctx.Err()
			}
		}
	}()
	e.retrievalGateMu.Lock()
	err = e.resolveProjectConditions(ctx)
	e.retrievalGateMu.Unlock()
	if err != nil {
		return "", err
	}
	// A turn that registered no retrieval fact still gets one admission check:
	// business rules may require user fields before any answer is usable, and
	// that decision must not wait for a search the model never ran. Only a
	// confirmed needs_user acts on it. Any other verdict — including an
	// unconfirmed stop — leaves the answer untouched: replacing a usable answer
	// with the stalled fallback turned a guard opinion into a lost turn.
	if e.retrievalGuard && e.retrieval != nil && len(e.retrieval.snapshot().Gaps) == 0 {
		verdict, err := e.retrievalAdmission(ctx, "fact", map[string]any{"current_input": e.retrievalUserMessage.text, "goal": e.retrieval.snapshot().Goal})
		switch {
		case errors.Is(err, errRetrievalGuardUnavailable):
			logger.Warnf(ctx, "Retrieval boundary check unavailable for a direct answer; publishing it unchanged: %v", err)
		case err != nil:
			return "", err
		case verdict.Decision == "needs_user":
			if _, err := clarificationFields(verdict.MissingUserFields); err != nil {
				return "", err
			}
			goal := e.retrieval.snapshot().Goal
			if err := e.retrieval.applyReview(retrievalReview{NewFact: &retrievalFactProposal{GoalQuote: goal, Fact: goal}, Status: retrievalNeedsUser, MissingUserFields: verdict.MissingUserFields, independentRequests: verdict.IndependentRequests}); err != nil {
				return "", err
			}
		case verdict.Decision != "allow":
			logger.Infof(ctx, "Direct answer kept: the boundary check returned %q without confirming required user fields", verdict.Decision)
		}
	}
	clean := tools.StripThinkBlocks(content)
	if strings.TrimSpace(clean) != "" {
		return clean, nil
	}
	if e.retrieval != nil {
		var fields []string
		for _, g := range e.retrieval.snapshot().Gaps {
			if g.Status == retrievalNeedsUser {
				fields = append(fields, g.MissingUserFields...)
			}
		}
		if len(fields) > 0 {
			return missingConditionsQuestion(fields), nil
		}
	}
	// Nothing publishable arrived. The loop's empty-content guard retries an
	// empty round, so no repair call is spent here. The repair used to exist to
	// rescue a draft made entirely of process narration — a heuristic that
	// deleted whole sentences from otherwise good answers, and is gone.
	return "", nil
}

// Hold a clarification-only draft while another requested fact can still be handled.
func (e *AgentEngine) clarificationLeavesIndependentWork(answer string) bool {
	if !e.retrievalGuard || e.retrieval == nil {
		return false
	}
	c := e.retrieval
	c.mu.Lock()
	defer c.mu.Unlock()
	fields := c.missingUserFields()
	if len(fields) == 0 {
		return false
	}
	for _, g := range c.state.Gaps {
		if g.Derived || !retrievalScopeCovers(c.state.Scope, g.Scope) || c.checkDependencies(&g) != nil {
			continue
		}
		if g.Status == retrievalPending || (answer == missingConditionsQuestion(fields) && c.evidenceAvailable(&g)) {
			return true
		}
	}
	return false
}

func missingConditionsQuestion(fields []string) string {
	return "方便提供一下" + strings.Join(fields, "、") + "吗？"
}

func isRuntimeFallbackAnswer(content string) bool {
	return content == stalledAnswerFallback || content == truncatedAnswerFallback
}

// Every publication path, including repaired drafts, inherits the live blockers.
func (e *AgentEngine) enforceMissingConditions(ctx context.Context, content string) string {
	if e.retrieval == nil {
		return content
	}
	c := &retrievalControl{state: e.retrieval.snapshot()}
	fields := c.missingUserFields()
	var supported []retrievalGap
	for _, g := range c.state.Gaps {
		if !retrievalScopeCovers(c.state.Scope, g.Scope) || !c.factNeeded(&g, make(map[string]bool)) {
			continue
		}
		if !g.Derived && c.evidenceAvailable(&g) && c.checkDependencies(&g) == nil {
			supported = append(supported, g)
		}
	}
	if len(fields) == 0 {
		return content
	}
	question := missingConditionsQuestion(fields)
	if content == question {
		return question
	}
	if e.retrievalGuard && strings.TrimSpace(content) != "" {
		verdict, err := e.retrievalAdmission(ctx, "blocked_answer", map[string]any{"candidate": content, "supported_independent_facts": supported, "missing_user_fields": fields})
		if err == nil && verdict.Decision == "allow" {
			if verdict.ClarificationComplete {
				return content
			}
			if len(supported) > 0 {
				return content + "\n\n" + question
			}
		}
	}
	return question
}

var (
	buttonSend = regexp.MustCompile(`(?m)^[ \t]*<button-send>(.*?)</button-send>[ \t]*\n?`)
	// "X 不支持 Y", "X：不支持", "| X | 不支持 |" — X and Y stop at clause/cell boundaries.
	unsupportedClaim = regexp.MustCompile(`([^，,。；;！!？?\n|：:]*?)[\s|：:]*(?:暂|目前|均|都|并|尚|还|也)*不支持([^，,。；;！!？?\n|]*)`)
)

// claimPart trims list/table/quote markers; deictic subjects ("该SDK") and
// one-rune leftovers carry no matchable name.
func claimPart(s string, subject bool) string {
	s = strings.Trim(s, " \t-*+#>")
	if subject {
		for _, p := range []string{"该", "本", "此", "这", "当前"} {
			if strings.HasPrefix(s, p) {
				return ""
			}
		}
	}
	if len([]rune(s)) < 2 {
		return ""
	}
	return s
}

// dropContradictedButtons removes follow-up buttons that name what the body
// itself declared unsupported: both subject and object when the claim has both
// ("抖音小游戏不支持沙盒支付" drops "抖音小游戏沙盒支付怎么配置", keeps
// "微信小游戏沙盒支付怎么配置"). The body text is never modified.
// ponytail: substring match on clause parts; long objects ("不支持在iOS上使用X")
// slip through — upgrade to structured support decisions if evals show misses.
func dropContradictedButtons(answer string) string {
	body := buttonSend.ReplaceAllString(answer, "")
	var claims [][2]string
	for _, m := range unsupportedClaim.FindAllStringSubmatch(body, -1) {
		if strings.HasSuffix(m[1], "非") || strings.HasSuffix(m[1], "否") { // 并非不支持 / 是否不支持
			continue
		}
		if c := [2]string{claimPart(m[1], true), claimPart(m[2], false)}; c[0] != "" || c[1] != "" {
			claims = append(claims, c)
		}
	}
	if len(claims) == 0 {
		return answer
	}
	return buttonSend.ReplaceAllStringFunc(answer, func(line string) string {
		text := buttonSend.FindStringSubmatch(line)[1]
		for _, c := range claims {
			if (c[0] == "" || strings.Contains(text, c[0])) && (c[1] == "" || strings.Contains(text, c[1])) {
				return ""
			}
		}
		return line
	})
}
