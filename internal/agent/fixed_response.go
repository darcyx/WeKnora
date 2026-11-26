package agent

import (
	"strings"
	"unicode"
)

const (
	fixedSentinelPrefix  = "[[FIXED:"
	fixedSentinelSuffix  = "]]"
	maxFenceInfoRunes    = 24
	maxLeadingWhitespace = 64
)

// parseOpeningFenceHeader parses a single markdown opening fence line such as
// "```markdown", "```", or "~~~". Shared between streaming sentinel gating and
// final response unwrapping so both paths accept identical prefixes.
func parseOpeningFenceHeader(line string) (fenceChar byte, fenceLen int, ok bool) {
	header := strings.TrimRight(line, "\r")
	if len(header) < 3 {
		return 0, 0, false
	}
	ch := header[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(header) && header[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	info := strings.TrimSpace(header[n:])
	if len([]rune(info)) > maxFenceInfoRunes || strings.ContainsAny(info, "`~ \t") {
		return 0, 0, false
	}
	return ch, n, true
}

// unwrapOuterMarkdownFence unwraps a single full-body fenced block such as
// ```markdown\n...\n``` or ~~~\n...\n~~~ when there is no earlier closing fence.
func unwrapOuterMarkdownFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return trimmed
	}
	nl := strings.IndexByte(trimmed, '\n')
	if nl < 0 {
		return trimmed
	}
	fenceChar, fenceLen, ok := parseOpeningFenceHeader(trimmed[:nl])
	if !ok {
		return trimmed
	}
	rest := trimmed[nl+1:]
	lines := strings.Split(rest, "\n")
	if len(lines) == 0 {
		return trimmed
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if !isClosingFenceLine(last, fenceChar, fenceLen) {
		return trimmed
	}
	for _, mid := range lines[:len(lines)-1] {
		if isClosingFenceLine(strings.TrimSpace(mid), fenceChar, fenceLen) {
			return trimmed
		}
	}
	return strings.TrimSpace(strings.Join(lines[:len(lines)-1], "\n"))
}

func isClosingFenceLine(line string, fenceChar byte, minLen int) bool {
	if len(line) < minLen {
		return false
	}
	for i := 0; i < len(line); i++ {
		if line[i] != fenceChar {
			return false
		}
	}
	return true
}

// expandFixedResponse deterministically expands an exact [[FIXED:<key>]]
// sentinel to its configured verbatim text, stripping any outer markdown fence,
// trailing reference block, or follow-up buttons. Unregistered keys are left
// untouched so ordinary text is never corrupted.
func expandFixedResponse(content string, fixed map[string]string) (string, bool) {
	if len(fixed) == 0 {
		return content, false
	}
	candidate := strings.TrimSpace(content)
	if candidate == "" {
		return content, false
	}
	// Strip outer wrappers iteratively so buttons/sources inside or outside a fence are handled.
	for range 2 {
		candidate = unwrapOuterMarkdownFence(candidate)
		candidate = strings.TrimSpace(stripFollowUpButtons(candidate))
		candidate = strings.TrimSpace(stripTrailingReferenceSection(candidate))
	}
	if !strings.HasPrefix(candidate, fixedSentinelPrefix) || !strings.HasSuffix(candidate, fixedSentinelSuffix) {
		return content, false
	}
	key := strings.TrimSpace(candidate[len(fixedSentinelPrefix) : len(candidate)-len(fixedSentinelSuffix)])
	if key == "" || strings.ContainsAny(key, "[]\n\r") {
		return content, false
	}
	if val, ok := fixed[key]; ok {
		if trimmed := strings.TrimSpace(val); trimmed != "" {
			return trimmed, true
		}
	}
	return content, false
}

type sentinelPrefixMatch int

const (
	sentinelPrefixNo sentinelPrefixMatch = iota
	sentinelPrefixPossible
	sentinelPrefixMatched
)

// classifySentinelPrefix checks whether s starts with [[FIXED: (optionally
// preceded by Unicode whitespace and a single opening markdown fence line such
// as ```markdown\n) or is still a valid prefix thereof.
func classifySentinelPrefix(s string) sentinelPrefixMatch {
	rest := strings.TrimLeftFunc(s, unicode.IsSpace)
	if rest == "" {
		return sentinelPrefixPossible
	}

	// Optional single opening code fence line (```markdown\n, ```\n, ~~~\n).
	if rest[0] == '`' || rest[0] == '~' {
		ch := rest[0]
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			header := strings.TrimRight(rest, "\r")
			fenceLen := 0
			for fenceLen < len(header) && header[fenceLen] == ch {
				fenceLen++
			}
			info := strings.TrimSpace(header[fenceLen:])
			if fenceLen < 3 {
				if info == "" {
					return sentinelPrefixPossible
				}
				return sentinelPrefixNo
			}
			if len([]rune(info)) > maxFenceInfoRunes || strings.ContainsAny(info, "`~ \t") {
				return sentinelPrefixNo
			}
			return sentinelPrefixPossible
		}
		if _, _, ok := parseOpeningFenceHeader(rest[:nl]); !ok {
			return sentinelPrefixNo
		}
		after := rest[nl+1:]
		rest = strings.TrimLeftFunc(after, unicode.IsSpace)
		if rest == "" {
			return sentinelPrefixPossible
		}
	}

	if strings.HasPrefix(rest, fixedSentinelPrefix) {
		return sentinelPrefixMatched
	}
	if strings.HasPrefix(fixedSentinelPrefix, rest) {
		return sentinelPrefixPossible
	}
	return sentinelPrefixNo
}

// fixedSentinelStreamGate buffers only the minimal leading prefix of a stream
// while it can still match [[FIXED:<key>]] (or a fenced sentinel). Ordinary
// answers flush immediately on the first non-matching chunk; sentinel streams
// stay buffered until publicAnswer expands them so raw sentinels never flash to
// SSE clients.
type fixedSentinelStreamGate struct {
	enabled bool
	decided bool
	holding bool
	buf     strings.Builder
}

func newFixedSentinelStreamGate(fixed map[string]string) *fixedSentinelStreamGate {
	return &fixedSentinelStreamGate{enabled: len(fixed) > 0}
}

func (g *fixedSentinelStreamGate) Reset() {
	if g == nil {
		return
	}
	g.decided = false
	g.holding = false
	g.buf.Reset()
}

func (g *fixedSentinelStreamGate) Held() bool {
	if g == nil || !g.enabled {
		return false
	}
	return g.holding || (!g.decided && g.buf.Len() > 0)
}

func (g *fixedSentinelStreamGate) Feed(chunk string) string {
	if g == nil || !g.enabled || chunk == "" {
		return chunk
	}
	if g.decided {
		if g.holding {
			g.buf.WriteString(chunk)
			return ""
		}
		return chunk
	}
	g.buf.WriteString(chunk)
	switch classifySentinelPrefix(g.buf.String()) {
	case sentinelPrefixMatched:
		g.decided = true
		g.holding = true
		return ""
	case sentinelPrefixPossible:
		// Cap unbounded leading whitespace without abandoning sentinel detection
		// before the first non-whitespace rune arrives.
		if strings.TrimLeftFunc(g.buf.String(), unicode.IsSpace) == "" && g.buf.Len() > maxLeadingWhitespace {
			g.buf.Reset()
		}
		return ""
	default:
		g.decided = true
		g.holding = false
		flushed := g.buf.String()
		g.buf.Reset()
		return flushed
	}
}

// referenceSectionStreamGate streams ordinary answer text live while holding
// any non-fenced "## 参考来源" / "### 参考来源" section (and anything following
// it) until publicAnswer verifies links against the structured URL whitelist.
// This prevents unverified URLs from flashing on SSE or being saved on cancel.
type referenceSectionStreamGate struct {
	holding   bool
	inFence   bool
	midLine   bool
	fenceChar byte
	fenceLen  int
	lineBuf   strings.Builder
}

func newReferenceSectionStreamGate() *referenceSectionStreamGate {
	return &referenceSectionStreamGate{}
}

func (g *referenceSectionStreamGate) Reset() {
	if g == nil {
		return
	}
	g.holding = false
	g.inFence = false
	g.midLine = false
	g.fenceChar = 0
	g.fenceLen = 0
	g.lineBuf.Reset()
}

func (g *referenceSectionStreamGate) Held() bool {
	if g == nil {
		return false
	}
	return g.holding || g.lineBuf.Len() > 0
}

// couldBeReferenceHeadingPrefix reports whether a partial line (no newline yet)
// could still complete into a "## 参考来源" or "### 参考来源" heading or a fence
// opener/closer line.
func couldBeReferenceHeadingPrefix(partial string, inFence bool) bool {
	trimmed := strings.TrimLeft(partial, " \t")
	if trimmed == "" {
		return true
	}
	if trimmed[0] == '`' || trimmed[0] == '~' {
		return len(trimmed) <= 64
	}
	if inFence {
		return false
	}
	if trimmed[0] != '#' {
		return false
	}
	// Accept prefixes of "## 参考来源" or "### 参考来源" (with flexible horizontal space).
	hashes := 0
	for hashes < len(trimmed) && trimmed[hashes] == '#' {
		hashes++
	}
	if hashes > 3 {
		return false
	}
	rest := trimmed[hashes:]
	if rest == "" {
		return true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return false
	}
	title := strings.TrimLeft(rest, " \t")
	if title == "" {
		return true
	}
	const target = "参考来源"
	if strings.HasPrefix(target, title) {
		return true
	}
	if strings.HasPrefix(title, target) {
		suffix := strings.TrimLeft(title[len(target):], " \t：:\r")
		return suffix == ""
	}
	return false
}

func (g *referenceSectionStreamGate) Feed(chunk string) string {
	if g == nil || chunk == "" {
		return ""
	}
	if g.holding {
		return ""
	}
	var out strings.Builder
	remaining := chunk
	for len(remaining) > 0 {
		if g.midLine {
			nl := strings.IndexByte(remaining, '\n')
			if nl < 0 {
				out.WriteString(remaining)
				break
			}
			out.WriteString(remaining[:nl+1])
			remaining = remaining[nl+1:]
			g.midLine = false
			continue
		}
		nl := strings.IndexByte(remaining, '\n')
		if nl >= 0 {
			g.lineBuf.WriteString(remaining[:nl+1])
			line := g.lineBuf.String()
			g.lineBuf.Reset()
			lineNoNL := strings.TrimRight(line, "\r\n")
			if isFenceToggleLine(lineNoNL, &g.inFence, &g.fenceChar, &g.fenceLen) {
				out.WriteString(line)
			} else if !g.inFence && referenceHeading.MatchString(lineNoNL) {
				g.holding = true
				break
			} else {
				out.WriteString(line)
			}
			remaining = remaining[nl+1:]
			continue
		}
		g.lineBuf.WriteString(remaining)
		remaining = ""
		if !couldBeReferenceHeadingPrefix(g.lineBuf.String(), g.inFence) {
			out.WriteString(g.lineBuf.String())
			g.lineBuf.Reset()
			g.midLine = true
		}
	}
	return out.String()
}

// Flush releases any buffered non-reference partial line at normal stream end
// (never flushing if holding a reference section).
func (g *referenceSectionStreamGate) Flush() string {
	if g == nil || g.holding || g.lineBuf.Len() == 0 {
		return ""
	}
	line := g.lineBuf.String()
	lineNoNL := strings.TrimRight(line, "\r\n")
	if !g.inFence && referenceHeading.MatchString(lineNoNL) {
		g.holding = true
		g.lineBuf.Reset()
		return ""
	}
	g.lineBuf.Reset()
	return line
}
