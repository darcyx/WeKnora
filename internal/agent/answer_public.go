package agent

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

var processNarration = regexp.MustCompile(`(?i)^(?:(?:现在我|目前我|接下来我|我(?:现在|已经|需要|将要)|i (?:now have|have enough|need to|will now)).*(?:组织回复|组织回答|足够信息|回复模板|检索策略|推理过程|选择规则|选择模板|compose|prepare|reason|enough information|analy[sz])|用户(?:只说|只提供|问的是|的意图).*(?:关键条件|问题|支付|登录)|the user (?:only|asks|wants).*(?:answer|information|question))`)

// Only remove recognizable process sentences outside code and quoted passages.
func stripProcessNarration(content string) (string, bool) {
	var out []string
	fence := ""
	removed := false
	for _, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			marker := trim[:3]
			if fence == "" {
				fence = marker
			} else if fence == marker {
				fence = ""
			}
			out = append(out, line)
			continue
		}
		if fence != "" || strings.HasPrefix(trim, ">") || strings.HasPrefix(trim, "\"") || strings.HasPrefix(trim, "“") || strings.HasPrefix(trim, "`") {
			out = append(out, line)
			continue
		}
		var kept strings.Builder
		start := 0
		for i, r := range line {
			if strings.ContainsRune("。！？.!?", r) {
				end := i + len(string(r))
				sentence := line[start:end]
				if processNarration.MatchString(strings.TrimSpace(sentence)) {
					removed = true
				} else {
					kept.WriteString(sentence)
				}
				start = end
			}
		}
		last := line[start:]
		if processNarration.MatchString(strings.TrimSpace(last)) {
			removed = true
		} else {
			kept.WriteString(last)
		}
		if kept.Len() > 0 {
			out = append(out, kept.String())
		}
	}
	if !removed {
		return content, false
	}
	return strings.Join(out, "\n"), true
}

// publicAnswer never turns a usable draft into an error: guard failures degrade
// to the cleaned original answer instead of losing it.
func (e *AgentEngine) publicAnswer(ctx context.Context, content string, messages []chat.Message) (string, error) {
	answer, err := e.publicAnswerChecked(ctx, content, messages)
	if err != nil && ctx.Err() == nil {
		logger.Warnf(ctx, "[Agent][PublicAnswer] degraded to original answer: %v", err)
		clean, _ := stripProcessNarration(tools.StripThinkBlocks(content))
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
	if e.retrievalGuard && e.retrieval != nil && len(e.retrieval.snapshot().Gaps) == 0 {
		verdict, err := e.retrievalAdmission(ctx, "fact", map[string]any{"current_input": e.retrievalUserMessage.text, "goal": e.retrieval.snapshot().Goal})
		if err != nil {
			return "", err
		}
		if verdict.Decision == "needs_user" {
			if _, err := clarificationFields(verdict.MissingUserFields); err != nil {
				return "", err
			}
			goal := e.retrieval.snapshot().Goal
			if err := e.retrieval.applyReview(retrievalReview{NewFact: &retrievalFactProposal{GoalQuote: goal, Fact: goal}, Status: retrievalNeedsUser, MissingUserFields: verdict.MissingUserFields}); err != nil {
				return "", err
			}
		}
		if verdict.Decision != "allow" && verdict.Decision != "needs_user" {
			return stalledAnswerFallback, nil
		}
	}
	clean, removed := stripProcessNarration(tools.StripThinkBlocks(content))
	if !removed || strings.TrimSpace(clean) != "" {
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
	// Only a recognized analysis-only draft gets one tool-free repair.
	repair := append([]chat.Message(nil), messages...)
	repair = append(repair, chat.Message{Role: "system", Content: "The previous candidate contained only process narration. Provide the actual user-facing answer, necessary clarification, or precise limitation from available evidence. Do not narrate analysis, rule selection or answer preparation. Do not add unsupported facts."})
	ctx, cancel := context.WithTimeout(api.WithoutTransportRetries(ctx), 30*time.Second)
	defer cancel()
	response, err := e.chatModel.Chat(ctx, repair, &chat.ChatOptions{Isolated: true, MaxTokens: 1024, ToolChoice: "none", Thinking: new(false)})
	if response != nil && e.progress.state != nil {
		e.progress.state.AddUsage(response.Usage)
		e.progress.state.TurnUsage.Accumulate(response.Usage)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err == nil && response != nil && len(response.ToolCalls) == 0 {
		if text, _ := stripProcessNarration(tools.StripThinkBlocks(response.Content)); strings.TrimSpace(text) != "" {
			return text, nil
		}
	}
	return stalledAnswerFallback, nil
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
