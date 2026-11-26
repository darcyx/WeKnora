package types

import (
	"encoding/json"
	"strings"
	"unicode"
)

// IsCredentialKey reports whether a JSON key name represents a credential or
// session secret that must never be exposed to models, persisted in snapshots,
// or cached in retrieval fingerprints.
func IsCredentialKey(key string) bool {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	normalized := strings.NewReplacer("_", "", "-", "").Replace(lower)
	if strings.HasSuffix(normalized, "token") || normalized == "tokenvalue" {
		return true
	}
	for _, marker := range []string{
		"password",
		"passwd",
		"secret",
		"authorization",
		"apikey",
		"cookie",
		"credential",
		"privatekey",
		"sessionid",
		"jwt",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	for _, seg := range splitIdentifierSegments(trimmed) {
		if seg == "pwd" || seg == "passwd" || seg == "jwt" {
			return true
		}
	}
	return false
}

func splitIdentifierSegments(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1]) {
			flush()
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// RedactAppInfo returns a deep copy of v with all credential keys stripped,
// including any top-level "token" key.
func RedactAppInfo(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			if IsCredentialKey(key) {
				continue
			}
			out[key] = RedactAppInfo(value)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, value := range typed {
			out[i] = RedactAppInfo(value)
		}
		return out
	default:
		return v
	}
}

// RedactLiveAppInfo preserves the top-level token for the current model turn.
func RedactLiveAppInfo(v map[string]any) map[string]any {
	out := RedactAppInfo(v).(map[string]any)
	if token, ok := v["token"]; ok {
		out["token"] = RedactAppInfo(token)
	}
	return out
}

// SanitizeLiveAppInfoJSON keeps the current turn token while removing other credentials.
func SanitizeLiveAppInfoJSON(raw string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(raw), &parsed) != nil || parsed == nil {
		return SanitizeAppInfoJSON(raw)
	}
	encoded, err := json.Marshal(RedactLiveAppInfo(parsed))
	if err != nil {
		return raw
	}
	return string(encoded)
}

// SanitizeAppInfoJSON strips all credential fields from a JSON-encoded app_info
// payload and returns deterministic compact JSON. Non-JSON inputs are returned
// unchanged; empty/whitespace inputs return "".
func SanitizeAppInfoJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return raw
	}
	redacted := RedactAppInfo(parsed)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return raw
	}
	return string(encoded)
}

// SanitizeAppInfoInText scrubs credential fields inside any <app_info ...>...</app_info>
// blocks embedded in conversation transcripts before summarization or persistence.
func SanitizeAppInfoInText(text string) string {
	if !strings.Contains(text, "<app_info") {
		return text
	}
	var sb strings.Builder
	rest := text
	for {
		start := strings.Index(rest, "<app_info")
		if start < 0 {
			sb.WriteString(rest)
			break
		}
		sb.WriteString(rest[:start])
		afterStart := rest[start:]
		openEnd := strings.IndexByte(afterStart, '>')
		if openEnd < 0 {
			sb.WriteString(afterStart)
			break
		}
		closeIdx := strings.Index(afterStart[openEnd+1:], "</app_info>")
		if closeIdx < 0 {
			sb.WriteString(afterStart)
			break
		}
		sb.WriteString(afterStart[:openEnd+1])
		body := afterStart[openEnd+1 : openEnd+1+closeIdx]
		sb.WriteString(sanitizeAppInfoEnvelopeBody(body))
		sb.WriteString("</app_info>")
		rest = afterStart[openEnd+1+closeIdx+len("</app_info>"):]
	}
	return sb.String()
}

func sanitizeAppInfoEnvelopeBody(body string) string {
	// Body may carry an HTML comment note after the JSON object.
	jsonPart := body
	suffix := ""
	if noteIdx := strings.Index(body, "\n  <!--"); noteIdx >= 0 {
		jsonPart = body[:noteIdx]
		suffix = body[noteIdx:]
	}
	unescaped := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(strings.TrimSpace(jsonPart))
	var parsed map[string]any
	if err := json.Unmarshal([]byte(unescaped), &parsed); err != nil || parsed == nil {
		return body
	}
	encoded, err := json.Marshal(RedactAppInfo(parsed))
	if err != nil {
		return body
	}
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(string(encoded))
	return escaped + suffix
}
