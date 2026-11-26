package agent

import (
	"context"
	"errors"
	"html"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/modelcontext"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// Buffer drafts only when retrieval admission or a known missing field can change the answer.
func (e *AgentEngine) holdRetrievalAnswer() bool {
	if e.retrieval == nil {
		return false
	}
	c := e.retrieval
	c.mu.Lock()
	defer c.mu.Unlock()
	return e.retrievalGuard && len(c.state.Gaps) == 0 || len(c.missingUserFields()) > 0
}

// publicAnswer degrades optional guard failures to the cleaned original answer.
// Cancellation and persistence failures remain fatal.
func (e *AgentEngine) publicAnswer(ctx context.Context, content string, messages []chat.Message) (string, error) {
	answer, err := e.publicAnswerChecked(ctx, content, messages)
	if err != nil && ctx.Err() == nil && !errors.Is(err, errRetrievalStateSave) {
		logger.Warnf(ctx, "[Agent][PublicAnswer] degraded to original answer: %v", err)
		clean := tools.StripThinkBlocks(content)
		if strings.TrimSpace(clean) == "" {
			clean = e.fallbackAnswer()
		}
		answer, err = e.enforceMissingConditions(ctx, clean), nil
	}
	if err != nil {
		return answer, err
	}
	if expanded, ok := expandFixedResponse(answer, e.config.FixedResponses); ok {
		return expanded, nil
	}
	answer = e.sanitizeReferenceSection(answer)
	return e.sanitizeFollowUpButtons(answer), nil
}

func (e *AgentEngine) publicAnswerChecked(ctx context.Context, content string, messages []chat.Message) (answer string, err error) {
	if e.retrieval != nil {
		e.retrieval.mu.Lock()
		failed := e.retrieval.persistenceFailed
		e.retrieval.mu.Unlock()
		if failed {
			return "", errRetrievalStateSave
		}
	}
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

func (e *AgentEngine) isRuntimeFallbackAnswer(content string) bool {
	return content == stalledAnswerFallback || content == truncatedAnswerFallback ||
		content != "" && content == e.configuredFallback()
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

const maxFollowUpButtons = 3

var (
	buttonSend = regexp.MustCompile(`(?m)^[ \t]*<button-send>([^<>]+)</button-send>[ \t]*\n?`)
	// Match standalone button lines (single or multiple buttons on one line),
	// optionally prefixed with list numbers or bullets.
	buttonSendLine      = regexp.MustCompile(`^[ \t]*(?:(?:\d+[.)、]|[-*+•])[ \t]+)?<button-send>([^<>]+)</button-send>[ \t]*$`)
	buttonSendTag       = regexp.MustCompile(`<button-send>([^<>]*)</button-send>`)
	buttonSendLineMulti = regexp.MustCompile(`^[ \t]*(?:(?:\d+[.)、]|[-*+•])[ \t]+)?(?:<button-send>[^<>]*</button-send>[ \t,;，；、|/]*)+$`)
	inlineButtonSend    = regexp.MustCompile(`<button-send>.*?</button-send>`)
	followUpHeadingLine = regexp.MustCompile(`(?i)^[ \t]*(?:#{1,6}[ \t]+)?(?:\*{1,2})?[ \t]*(?:相关问题|猜你想问|你可能还想问|您可能还想问|推荐问题|延伸问题|继续提问|下一步(?:建议|可问|问题)?|related questions|follow-up questions|suggested questions)[ \t：:]*(?:\*{1,2})?[ \t：:]*$`)
	referenceHeading    = regexp.MustCompile(`^[ \t]*#{2,3}[ \t]+参考来源[ \t：:\r]*$`)
	markdownHeadingLine = regexp.MustCompile(`^[ \t]*#{1,6}[ \t]+`)
	htmlAnchorTag       = regexp.MustCompile(`(?i)<a\b[^>]*>`)
	htmlAttrPair        = regexp.MustCompile(`(?i)(?:^|[\s/])([a-z][a-z0-9:_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+))`)
	bareHTTPURL         = regexp.MustCompile(`(?i)https?://[^\s<>"'` + "`" + `\\，。；！？、（）【】]+`)
	verifiedCitationTag = regexp.MustCompile(`<(?:kb|web)\b[^>]*/?>`)
	referenceListPrefix = regexp.MustCompile(`^[ \t]*(?:[-*+•]|\d+[.)、])[ \t]+`)
	noAnswerOrRefusal   = regexp.MustCompile(`(?:无法回答与[^。！？\n]{0,24}无关|超出(?:服务|解答|支持)范围|(?:当前)?知识库(?:中)?(?:没有|未)(?:查询到|找到|检索到)[^。！？\n]{0,30}(?:解答|答案|对应|相关))`)
	// "X 不支持 Y", "X：不支持", "| X | 不支持 |" — X and Y stop at clause/cell boundaries.
	unsupportedClaim = regexp.MustCompile(`([^，,。；;！!？?\n|：:]*?)[\s|：:]*(?:暂|目前|均|都|并|尚|还|也)*不支持([^，,。；;！!？?\n|]*)`)
)

func (e *AgentEngine) isFixedResponseValue(trimmed string) bool {
	if e == nil || e.config == nil || len(e.config.FixedResponses) == 0 || trimmed == "" {
		return false
	}
	for _, val := range e.config.FixedResponses {
		if v := strings.TrimSpace(val); v != "" && trimmed == v {
			return true
		}
	}
	return false
}

func (e *AgentEngine) hasMissingUserFields() bool {
	if e == nil || e.retrieval == nil {
		return false
	}
	return len((&retrievalControl{state: e.retrieval.snapshot()}).missingUserFields()) > 0
}

func (e *AgentEngine) isNoAnswerOrRefusalBody(body string) bool {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || e.isRuntimeFallbackAnswer(trimmed) || e.isFixedResponseValue(trimmed) {
		return true
	}
	candidate := strings.TrimSpace(stripTrailingReferenceSection(unwrapOuterMarkdownFence(trimmed)))
	if candidate == "" || e.isRuntimeFallbackAnswer(candidate) || e.isFixedResponseValue(candidate) {
		return true
	}
	semanticBody := strings.TrimSpace(unfencedText(stripTrailingReferenceSection(trimmed)))
	if len([]rune(semanticBody)) <= 220 && !strings.HasPrefix(semanticBody, "## ") && !strings.Contains(semanticBody, "\n## ") &&
		noAnswerOrRefusal.MatchString(semanticBody) {
		return true
	}
	return false
}

// isFenceToggleLine returns whether line opens or closes a fenced code block,
// updating fenceChar and fenceLen accordingly.
func isFenceToggleLine(line string, inFence *bool, fenceChar *byte, fenceLen *int) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < 3 {
		return false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return false
	}
	if !*inFence {
		// Opening backtick fence must not contain backticks in the info string.
		if ch == '`' && strings.ContainsRune(trimmed[n:], '`') {
			return false
		}
		*inFence = true
		*fenceChar = ch
		*fenceLen = n
		return true
	}
	if ch == *fenceChar && n >= *fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
		*inFence = false
		*fenceChar = 0
		*fenceLen = 0
		return true
	}
	return false
}

// fencedLines marks fence delimiters and their contents so button handling
// leaves Markdown code examples untouched.
func fencedLines(lines []string) []bool {
	protected := make([]bool, len(lines))
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for i, line := range lines {
		wasInFence := inFence
		toggle := isFenceToggleLine(strings.TrimRight(line, "\r\n"), &inFence, &fenceChar, &fenceLen)
		protected[i] = wasInFence || toggle
	}
	return protected
}

func unfencedText(text string) string {
	lines := strings.Split(text, "\n")
	protected := fencedLines(lines)
	for i := range lines {
		if protected[i] {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

type lineSpan struct {
	start int
	end   int
}

func isReferenceEntryLine(line string) bool {
	clean := strings.TrimRight(line, "\r")
	if referenceListPrefix.MatchString(clean) {
		return true
	}
	if htmlAnchorTag.MatchString(clean) || bareHTTPURL.MatchString(clean) || verifiedCitationTag.MatchString(clean) {
		return true
	}
	return false
}

// findReferenceSections locates all "## 参考来源" or "### 参考来源" sections
// outside fenced code blocks. Each section stops before the next Markdown
// heading, follow-up heading, <button-send> line, or a blank line followed by
// a non-list/non-link prose paragraph (preserving trailing handoff notes).
func findReferenceSections(lines []string) []lineSpan {
	inFence := false
	var fenceChar byte
	fenceLen := 0
	var sections []lineSpan
	start := -1
	sawBlankAfterItem := false
	sawEntry := false

	closeSection := func(end int) {
		if start >= 0 {
			sections = append(sections, lineSpan{start: start, end: end})
			start = -1
			sawBlankAfterItem = false
			sawEntry = false
		}
	}

	for i, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		if isFenceToggleLine(line, &inFence, &fenceChar, &fenceLen) {
			if start >= 0 {
				closeSection(i)
			}
			continue
		}
		if inFence {
			continue
		}
		if start < 0 {
			if referenceHeading.MatchString(line) {
				start = i
				sawBlankAfterItem = false
				sawEntry = false
			}
			continue
		}
		if referenceHeading.MatchString(line) {
			closeSection(i)
			start = i
			continue
		}
		if markdownHeadingLine.MatchString(line) || followUpHeadingLine.MatchString(line) ||
			buttonSendLineMulti.MatchString(line) || strings.Contains(line, "<button-send>") {
			closeSection(i)
			continue
		}
		if strings.TrimSpace(line) == "" {
			if sawEntry {
				sawBlankAfterItem = true
			}
			continue
		}
		if sawBlankAfterItem && !isReferenceEntryLine(line) {
			closeSection(i)
			continue
		}
		sawEntry = true
		sawBlankAfterItem = false
	}
	closeSection(len(lines))
	return sections
}

// stripTrailingReferenceSection removes a trailing reference section when
// nothing other than blank lines follows it.
func stripTrailingReferenceSection(answer string) string {
	if !strings.Contains(answer, "参考来源") {
		return answer
	}
	lines := strings.Split(answer, "\n")
	sections := findReferenceSections(lines)
	if len(sections) == 0 {
		return answer
	}
	last := sections[len(sections)-1]
	for i := last.end; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return answer
		}
	}
	return strings.TrimRight(strings.Join(lines[:last.start], "\n"), " \t\r\n")
}

func trimBalancedURLDelimiters(raw string) string {
	link := strings.TrimRight(raw, ".,;!?")
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}, {"{", "}"}} {
		for strings.HasSuffix(link, pair[1]) && strings.Count(link, pair[1]) > strings.Count(link, pair[0]) {
			link = strings.TrimSuffix(link, pair[1])
		}
	}
	return link
}

// extractMarkdownLinkURLs extracts all markdown link URLs [label](url) on a
// line, supporting balanced parentheses inside URLs (e.g. Wikipedia paths), and
// returns both the extracted URLs and the byte spans covered so bare-URL
// scanning does not double-count them.
func extractMarkdownLinkURLs(line string) ([]string, []lineSpan) {
	var urls []string
	var spans []lineSpan
	for i := 0; i < len(line); i++ {
		if line[i] != '[' {
			continue
		}
		// Find matching '](' on this line.
		closeBracket := strings.IndexByte(line[i+1:], ']')
		if closeBracket < 0 {
			break
		}
		openParen := i + 1 + closeBracket + 1
		if openParen >= len(line) || line[openParen] != '(' {
			continue
		}
		pos := openParen + 1
		for pos < len(line) && (line[pos] == ' ' || line[pos] == '\t') {
			pos++
		}
		urlStart := pos
		depth := 0
		end := -1
		if pos < len(line) && line[pos] == '<' {
			// Angle-bracket destination: [text](<url>)
			urlStart = pos + 1
			closeAngle := strings.IndexByte(line[urlStart:], '>')
			if closeAngle >= 0 {
				raw := line[urlStart : urlStart+closeAngle]
				afterAngle := urlStart + closeAngle + 1
				closeParen := strings.IndexByte(line[afterAngle:], ')')
				if closeParen >= 0 {
					end = afterAngle + closeParen + 1
					urls = append(urls, html.UnescapeString(strings.TrimSpace(raw)))
					spans = append(spans, lineSpan{start: openParen, end: end})
					i = end - 1
				}
			}
			continue
		}
		for pos < len(line) {
			ch := line[pos]
			if ch == ' ' || ch == '\t' {
				break
			}
			if ch == '(' {
				depth++
			} else if ch == ')' {
				if depth == 0 {
					end = pos + 1
					break
				}
				depth--
			}
			pos++
		}
		if end < 0 {
			// Handle optional title after space: [text](url "title")
			urlEnd := pos
			closeParen := strings.IndexByte(line[pos:], ')')
			if closeParen >= 0 {
				end = pos + closeParen + 1
				raw := strings.TrimSpace(line[urlStart:urlEnd])
				if raw != "" {
					urls = append(urls, html.UnescapeString(raw))
					spans = append(spans, lineSpan{start: openParen, end: end})
					i = end - 1
				}
			}
			continue
		}
		raw := strings.TrimSpace(line[urlStart : end-1])
		if raw != "" {
			urls = append(urls, html.UnescapeString(raw))
			spans = append(spans, lineSpan{start: openParen, end: end})
			i = end - 1
		}
	}
	return urls, spans
}

func overlapsAnySpan(start, end int, spans []lineSpan) bool {
	for _, s := range spans {
		if start < s.end && end > s.start {
			return true
		}
	}
	return false
}

// extractHTMLAnchorHrefs parses exact "href" attributes (ignoring data-href or
// other suffix-matching attributes) from every <a ...> tag on a line, returning
// each href value and ONLY its attribute byte span.
func extractHTMLAnchorHrefs(line string) ([]string, []lineSpan) {
	var urls []string
	var spans []lineSpan
	for _, tagIdx := range htmlAnchorTag.FindAllStringIndex(line, -1) {
		tagBodyStart := tagIdx[0] + 2 // skip "<a"
		tagBodyEnd := tagIdx[1] - 1   // skip ">"
		if tagBodyEnd <= tagBodyStart {
			continue
		}
		tagBody := line[tagBodyStart:tagBodyEnd]
		for _, attrIdx := range htmlAttrPair.FindAllStringSubmatchIndex(tagBody, -1) {
			name := strings.ToLower(tagBody[attrIdx[2]:attrIdx[3]])
			if name != "href" {
				continue
			}
			var raw string
			switch {
			case attrIdx[4] >= 0 && attrIdx[5] >= 0:
				raw = tagBody[attrIdx[4]:attrIdx[5]]
			case attrIdx[6] >= 0 && attrIdx[7] >= 0:
				raw = tagBody[attrIdx[6]:attrIdx[7]]
			case attrIdx[8] >= 0 && attrIdx[9] >= 0:
				raw = tagBody[attrIdx[8]:attrIdx[9]]
			}
			if raw != "" {
				urls = append(urls, html.UnescapeString(raw))
				spans = append(spans, lineSpan{
					start: tagBodyStart + attrIdx[0],
					end:   tagBodyStart + attrIdx[1],
				})
			}
		}
	}
	return urls, spans
}

// extractReferenceLineURLs extracts EVERY link target on a reference line across
// HTML <a href>, Markdown [text](url) (with balanced parentheses), and bare
// HTTP(S) URLs so mixed-syntax lines cannot smuggle unverified links.
func extractReferenceLineURLs(line string) []string {
	var rawURLs []string
	var covered []lineSpan

	htmlURLs, htmlSpans := extractHTMLAnchorHrefs(line)
	rawURLs = append(rawURLs, htmlURLs...)
	covered = append(covered, htmlSpans...)

	mdURLs, mdSpans := extractMarkdownLinkURLs(line)
	rawURLs = append(rawURLs, mdURLs...)
	covered = append(covered, mdSpans...)

	for _, idx := range bareHTTPURL.FindAllStringIndex(line, -1) {
		if overlapsAnySpan(idx[0], idx[1], covered) {
			continue
		}
		raw := trimBalancedURLDelimiters(html.UnescapeString(line[idx[0]:idx[1]]))
		if raw != "" {
			rawURLs = append(rawURLs, raw)
		}
	}
	return rawURLs
}

// sanitizeReferenceSection validates links in every non-fenced ## 参考来源 /
// ### 参考来源 section against the structured URL whitelist in modelContext,
// preserves system-verified <kb/>/<web/> citations, drops fabricated or
// duplicate URLs, removes empty headings, and preserves intervening prose and
// trailing <button-send> lines.
func (e *AgentEngine) sanitizeReferenceSection(answer string) string {
	if e == nil || e.modelContext == nil || !strings.Contains(answer, "参考来源") {
		return answer
	}
	allowed := e.modelContext.AllowedReferenceURLs()
	lines := strings.Split(answer, "\n")
	sections := findReferenceSections(lines)
	if len(sections) == 0 {
		return answer
	}

	var parts []string
	cursor := 0
	for _, sec := range sections {
		if sec.start > cursor {
			seg := strings.Trim(strings.Join(lines[cursor:sec.start], "\n"), "\r\n")
			if strings.TrimSpace(seg) != "" {
				parts = append(parts, strings.TrimRight(seg, " \t\r\n"))
			}
		}

		seen := make(map[string]struct{})
		keptItems := make([]string, 0, sec.end-sec.start)
		for i := sec.start + 1; i < sec.end; i++ {
			line := lines[i]
			if strings.TrimSpace(line) == "" {
				continue
			}
			urls := extractReferenceLineURLs(line)
			if len(urls) == 0 {
				if verifiedCitationTag.MatchString(line) {
					keptItems = append(keptItems, line)
				}
				continue
			}
			allAllowed := true
			hasNew := false
			norms := make([]string, 0, len(urls))
			for _, raw := range urls {
				norm := modelcontext.NormalizeReferenceURL(raw)
				if norm == "" {
					allAllowed = false
					break
				}
				if _, ok := allowed[norm]; !ok {
					allAllowed = false
					break
				}
				norms = append(norms, norm)
				if _, dup := seen[norm]; !dup {
					hasNew = true
				}
			}
			if !allAllowed || !hasNew {
				continue
			}
			for _, norm := range norms {
				seen[norm] = struct{}{}
			}
			keptItems = append(keptItems, line)
		}

		if len(keptItems) > 0 {
			parts = append(parts, lines[sec.start]+"\n"+strings.Join(keptItems, "\n"))
		}
		cursor = sec.end
	}

	if cursor < len(lines) {
		after := strings.TrimLeft(strings.Join(lines[cursor:], "\n"), "\r\n")
		if strings.TrimSpace(after) != "" {
			parts = append(parts, strings.TrimRight(after, " \t\r\n"))
		}
	}
	return strings.TrimRight(strings.Join(parts, "\n\n"), " \t\r\n")
}

// removePrecedingFollowUpHeading clears a standalone "相关问题" heading directly
// above a removed or normalized button block (skipping blank lines).
func removePrecedingFollowUpHeading(lines []string, removed []bool, from int) {
	for i := from - 1; i >= 0; i-- {
		if removed[i] || strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if followUpHeadingLine.MatchString(lines[i]) {
			removed[i] = true
		}
		return
	}
}

// stripFollowUpButtons removes all <button-send> tags and any standalone
// follow-up heading immediately preceding them.
func stripFollowUpButtons(answer string) string {
	if !strings.Contains(answer, "<button-send>") {
		return answer
	}
	lines := strings.Split(answer, "\n")
	removed := make([]bool, len(lines))
	protected := fencedLines(lines)
	for i, line := range lines {
		if !protected[i] && (buttonSendLineMulti.MatchString(line) || buttonSendLine.MatchString(line)) {
			removed[i] = true
			removePrecedingFollowUpHeading(lines, removed, i)
		}
	}
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if removed[i] {
			continue
		}
		if !protected[i] && strings.Contains(line, "<button-send>") {
			cleaned := strings.TrimSpace(inlineButtonSend.ReplaceAllString(line, ""))
			if cleaned == "" {
				continue
			}
			line = cleaned
		}
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), " \t\r\n")
}

// sanitizeFollowUpButtons enforces deterministic <button-send> rules at answer
// publication: strip all buttons on fallback/refusal/clarification turns, and
// normalize, split multi-button lines, deduplicate, drop contradicted items,
// and cap to 3 on normal turns.
func (e *AgentEngine) sanitizeFollowUpButtons(answer string) string {
	if !strings.Contains(answer, "<button-send>") {
		return answer
	}
	body := stripFollowUpButtons(answer)
	if e.hasMissingUserFields() || e.isNoAnswerOrRefusalBody(body) {
		return body
	}

	rawLines := strings.Split(answer, "\n")
	lines := make([]string, 0, len(rawLines))
	hadDroppedEmptyButton := false
	rawProtected := fencedLines(rawLines)
	for i, line := range rawLines {
		if !rawProtected[i] && buttonSendLineMulti.MatchString(line) {
			matches := buttonSendTag.FindAllStringSubmatch(line, -1)
			emittedAny := false
			for _, m := range matches {
				text := strings.TrimSpace(m[1])
				if text == "" {
					hadDroppedEmptyButton = true
					continue
				}
				lines = append(lines, "<button-send>"+text+"</button-send>")
				emittedAny = true
			}
			if !emittedAny && hadDroppedEmptyButton {
				// Leave a marker blank removal for preceding follow-up heading cleanup below.
				for j := len(lines) - 1; j >= 0; j-- {
					if strings.TrimSpace(lines[j]) == "" {
						continue
					}
					if followUpHeadingLine.MatchString(lines[j]) {
						lines = append(lines[:j], lines[j+1:]...)
					}
					break
				}
			}
			continue
		}
		lines = append(lines, line)
	}

	removed := make([]bool, len(lines))
	seen := make(map[string]bool)
	protected := fencedLines(lines)
	for i, line := range lines {
		if protected[i] {
			continue
		}
		match := buttonSendLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		removePrecedingFollowUpHeading(lines, removed, i)
		text := strings.TrimSpace(match[1])
		if text == "" || seen[text] {
			removed[i] = true
			continue
		}
		seen[text] = true
		lines[i] = "<button-send>" + text + "</button-send>"
	}
	normalized := make([]string, 0, len(lines))
	for i, line := range lines {
		if !removed[i] {
			normalized = append(normalized, line)
		}
	}

	filtered := dropContradictedButtons(strings.Join(normalized, "\n"))

	filteredLines := strings.Split(filtered, "\n")
	capped := make([]string, 0, len(filteredLines))
	keptButtons := 0
	filteredProtected := fencedLines(filteredLines)
	for i, line := range filteredLines {
		if !filteredProtected[i] && buttonSendLine.MatchString(line) {
			if keptButtons >= maxFollowUpButtons {
				continue
			}
			keptButtons++
		}
		capped = append(capped, line)
	}
	return strings.TrimRight(strings.Join(capped, "\n"), " \t\r\n")
}

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
	body := buttonSend.ReplaceAllString(unfencedText(answer), "")
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
	lines := strings.SplitAfter(answer, "\n")
	protected := fencedLines(lines)
	for i, line := range lines {
		if protected[i] {
			continue
		}
		lines[i] = buttonSend.ReplaceAllStringFunc(line, func(line string) string {
			text := buttonSend.FindStringSubmatch(line)[1]
			for _, c := range claims {
				if (c[0] == "" || strings.Contains(text, c[0])) && (c[1] == "" || strings.Contains(text, c[1])) {
					return ""
				}
			}
			return line
		})
	}
	return strings.Join(lines, "")
}
