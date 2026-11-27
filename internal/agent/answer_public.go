package agent

import (
	"context"
	"html"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/modelcontext"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// holdRetrievalAnswer returns false under deterministic RuntimeRetrieval so
// first-round streaming never stalls waiting for a synchronous LLM judge.
func (e *AgentEngine) holdRetrievalAnswer() bool {
	return false
}

// publicAnswer applies deterministic post-processing (fixed response expansion,
// reference URL whitelist sanitization, and extension hooks).
// Cancellation and persistence failures remain fatal.
func (e *AgentEngine) publicAnswer(ctx context.Context, content string, _ []chat.Message) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if e.retrieval != nil {
		e.retrieval.mu.Lock()
		failed := e.retrieval.persistenceFailed
		e.retrieval.mu.Unlock()
		if failed {
			return "", errRetrievalStateSave
		}
	}
	answer := tools.StripThinkBlocks(content)
	if expanded, ok := expandFixedResponseWithStripper(answer, e.config.FixedResponses, e.extHooks().StripFollowUpButtons); ok {
		return expanded, nil
	}
	answer = e.sanitizeReferenceSection(answer)
	return e.extHooks().PostProcessAnswer(e, answer), nil
}

func (e *AgentEngine) isRuntimeFallbackAnswer(content string) bool {
	return content == stalledAnswerFallback || content == truncatedAnswerFallback ||
		content != "" && content == e.configuredFallback()
}

var (
	referenceHeading    = regexp.MustCompile(`^[ \t]*#{2,3}[ \t]+参考来源[ \t：:\r]*$`)
	markdownHeadingLine = regexp.MustCompile(`^[ \t]*#{1,6}[ \t]+`)
	htmlAnchorTag       = regexp.MustCompile(`(?i)<a\b[^>]*>`)
	htmlAttrPair        = regexp.MustCompile(`(?i)(?:^|[\s/])([a-z][a-z0-9:_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+))`)
	bareHTTPURL         = regexp.MustCompile(`(?i)https?://[^\s<>"'` + "`" + `\\，。；！？、（）【】]+`)
	verifiedCitationTag = regexp.MustCompile(`<(?:kb|web)\b[^>]*/?>`)
	referenceListPrefix = regexp.MustCompile(`^[ \t]*(?:[-*+•]|\d+[.)、])[ \t]+`)
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
	return e != nil && e.retrieval != nil && e.retrieval.isAwaitingUserInput()
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
