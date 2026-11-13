package agent

import (
	"regexp"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// Wiki tools render Markdown inside fixed, line-delimited tags. Match that
// envelope without parsing or re-serializing the Markdown as XML.
var (
	wikiPageBlock  = regexp.MustCompile("(?ms)^<wiki_page>\n.*?^</wiki_page>$")
	wikiNavigation = regexp.MustCompile("(?ms)^<relationships>\n.*?^</relationships>\n")
)

// compactWikiHistory projects tool messages for this model call. Keep the last
// tool batch intact so the model can choose pages and see freshly fetched data.
func compactWikiHistory(messages []chat.Message) []chat.Message {
	lastBatch := len(messages)
	for lastBatch > 0 && messages[lastBatch-1].Role == "tool" {
		lastBatch--
	}
	var projected []chat.Message
	var seen map[string]bool
scan:
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		switch {
		case m.Role == "user":
			// History before this user request may already have been summarized.
			break scan
		case m.Role != "tool":
			continue
		case m.Name == agenttools.ToolWikiReadPage:
			if seen == nil {
				seen = make(map[string]bool)
			}
			m.Content = wikiPageBlock.ReplaceAllStringFunc(m.Content, func(page string) string {
				// Relationships can change between reads. Keep sources and summary
				// in the identity so equal text from another KB is never merged.
				evidence := wikiNavigation.ReplaceAllString(page, "")
				repeat := seen[evidence]
				seen[evidence] = true
				if i >= lastBatch {
					return page
				}
				header, _, ok := strings.Cut(evidence, "</metadata>\n")
				if !ok {
					return page
				}
				if repeat {
					return header + "</metadata>\n[Same page and evidence appear in a later tool result.]\n</wiki_page>"
				}
				if strings.Contains(header, "\n<type>index</type>\n") {
					return header + "</metadata>\n[Directory previously shown. Use wiki_search or read the index again to browse.]\n</wiki_page>"
				}
				return evidence
			})
		default:
			continue
		}
		if m.Content != messages[i].Content && len(m.Content) < len(messages[i].Content) {
			if projected == nil {
				projected = append([]chat.Message(nil), messages...)
			}
			projected[i] = m
		}
	}
	if projected != nil {
		return projected
	}
	return messages
}
