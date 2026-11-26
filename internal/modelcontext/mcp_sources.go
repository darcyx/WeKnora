package modelcontext

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// MCP schemas and payloads remain opaque. This sidecar only indexes literal
// HTTP(S) links observed in a successful result; it never interprets external
// IDs as knowledge chunks or synthesizes URLs from article IDs.
const maxMCPSourceCandidates = 50

var (
	mcpJSONStringRE = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	httpURLRE       = regexp.MustCompile("(?i)https?://[^\\s<>\"'`\\\\，。；！？、（）【】]+")
)

func trimURLDelimiters(match string) string {
	link := strings.TrimRight(match, ".,;!?")
	// Retain balanced URL parentheses such as /wiki/Function_(mathematics).
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}, {"{", "}"}} {
		for strings.HasSuffix(link, pair[1]) && strings.Count(link, pair[1]) > strings.Count(link, pair[0]) {
			link = strings.TrimSuffix(link, pair[1])
		}
	}
	return link
}

func extractMCPHTTPURLs(output string) []string {
	if strings.TrimSpace(output) == "" {
		return nil
	}
	// Decode JSON string escaping only in a scratch copy used to find links.
	// Both the external body and the durable ToolResult stay unchanged.
	scan := mcpJSONStringRE.ReplaceAllStringFunc(output, func(token string) string {
		var value string
		if json.Unmarshal([]byte(token), &value) == nil {
			return `"` + value + `"`
		}
		return token
	})
	seen := make(map[string]bool)
	var urls []string
	for _, match := range httpURLRE.FindAllString(scan, -1) {
		rawURL := trimURLDelimiters(html.UnescapeString(match))
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil ||
			(parsed.Scheme != "https" && parsed.Scheme != "http") {
			continue
		}
		key := NormalizeReferenceURL(rawURL)
		if seen[key] {
			continue
		}
		seen[key] = true
		urls = append(urls, rawURL)
		if len(seen) >= maxMCPSourceCandidates {
			break
		}
	}
	return urls
}

// mcpResultURLs applies one shared limit to the output and structured content,
// preserving fragments for reference URL validation.
func mcpResultURLs(result *types.ToolResult) []string {
	if result == nil {
		return nil
	}
	urls := extractMCPHTTPURLs(result.Output)
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		seen[NormalizeReferenceURL(u)] = true
	}
	if len(urls) < maxMCPSourceCandidates && result.Data != nil {
		if sc := result.Data["structured_content"]; sc != nil {
			if rawJSON, err := json.Marshal(sc); err == nil {
				for _, u := range extractMCPHTTPURLs(string(rawJSON)) {
					key := NormalizeReferenceURL(u)
					if seen[key] {
						continue
					}
					seen[key] = true
					urls = append(urls, u)
					if len(urls) >= maxMCPSourceCandidates {
						break
					}
				}
			}
		}
	}
	return urls
}

func (r *Registry) mcpSourceCandidates(result *types.ToolResult) string {
	urls := mcpResultURLs(result)
	// Always register published MCP URLs into allowedRefURLs independently of
	// citationsEnabled so reference-section validation matches model-visible MCP links.
	for _, rawURL := range urls {
		r.sources.RegisterReferenceURL(rawURL)
	}
	if !r.sources.citationsEnabled || len(urls) == 0 {
		return ""
	}
	var rows strings.Builder
	seenHandles := make(map[string]bool)
	for _, rawURL := range urls {
		handle := r.sources.RegisterWeb(rawURL, "")
		if seenHandles[handle] {
			continue
		}
		seenHandles[handle] = true
		fmt.Fprintf(&rows, "<source id=\"%s\" url=\"%s\"/>\n", handle, escapeAttr(rawURL))
	}
	return "\n\n<external_source_candidates>\n" +
		"System-indexed links from this MCP result. Cite the matching url://N " +
		"only when the result supports the claim; " +
		"a link alone does not mean the linked page was read. Do not use KB " +
		"chunk handles for this external content.\n" +
		rows.String() + "</external_source_candidates>"
}
