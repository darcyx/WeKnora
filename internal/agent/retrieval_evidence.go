package agent

import (
	"encoding/xml"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Compare presentation variants without changing the retained source or its wording.
func evidenceContains(source, quote string) bool {
	if strings.TrimSpace(quote) == "" {
		return false
	}
	if strings.Contains(source, quote) {
		return true
	}
	decoded, wanted := html.UnescapeString(source), html.UnescapeString(quote)
	if strings.Contains(decoded, wanted) {
		return true
	}
	wanted = evidenceInlineText(wanted)
	if strings.TrimSpace(wanted) == "" {
		return false
	}
	// XML envelopes must remain boundaries: never join text from different chunks.
	decoder := xml.NewDecoder(strings.NewReader(source))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		if data, ok := token.(xml.CharData); ok && strings.Contains(evidenceInlineText(string(data)), wanted) {
			return true
		}
	}
	return strings.Contains(evidenceInlineText(decoded), wanted)
}

// Remove only parser-recognized emphasis/code delimiters; keep numbers, operators,
// links, strikethrough, whitespace and literal markup inside code unchanged.
func evidenceInlineText(value string) string {
	source := []byte(value)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	drop := make([]bool, len(source))
	var bounds func(ast.Node) (int, int)
	bounds = func(node ast.Node) (int, int) {
		if leaf, ok := node.(*ast.Text); ok {
			return leaf.Segment.Start, leaf.Segment.Stop
		}
		start, end := len(source), 0
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			left, right := bounds(child)
			start, end = min(start, left), max(end, right)
		}
		if start >= end {
			return start, end
		}
		width := 0
		switch inline := node.(type) {
		case *ast.Emphasis:
			width = inline.Level
		case *ast.CodeSpan:
			for start-width > 0 && source[start-width-1] == '`' {
				width++
			}
		}
		if width > 0 && start >= width && end+width <= len(source) {
			left, right := source[start-width:start], source[end:end+width]
			if string(left) == string(right) && strings.Trim(string(left), "*_`") == "" {
				for i := 0; i < width; i++ {
					drop[start-width+i], drop[end+i] = true, true
				}
				return start - width, end + width
			}
		}
		return start, end
	}
	bounds(root)
	var out strings.Builder
	for i, char := range source {
		if !drop[i] {
			out.WriteByte(char)
		}
	}
	return out.String()
}
