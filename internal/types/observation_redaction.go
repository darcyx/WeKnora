package types

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

type observationSecretsKey struct{}

// WithObservationAppInfo scopes token redaction to observation copies for this run.
func WithObservationAppInfo(ctx context.Context, raw string) context.Context {
	var info map[string]any
	if json.Unmarshal([]byte(raw), &info) != nil {
		return ctx
	}
	var values []string
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case string:
			if x != "" {
				values = append(values, x)
			}
		case map[string]any:
			for _, y := range x {
				collect(y)
			}
		case []any:
			for _, y := range x {
				collect(y)
			}
		}
	}
	collect(info["token"])
	if len(values) == 0 {
		return ctx
	}
	return context.WithValue(ctx, observationSecretsKey{}, values)
}

// SanitizeObservationText removes the run's allowed token, including JSON-escaped forms.
func SanitizeObservationText(ctx context.Context, text string) string {
	text = SanitizeAppInfoInText(text)
	secrets, _ := ctx.Value(observationSecretsKey{}).([]string)
	for _, secret := range secrets {
		// A provider JSON body can contain an app_info JSON string, itself
		// escaped inside another JSON string. Replace longest forms first.
		forms := []string{secret}
		for i := 0; i < 3; i++ {
			encoded, _ := json.Marshal(forms[len(forms)-1])
			forms = append(forms, string(encoded[1:len(encoded)-1]))
		}
		for i := len(forms) - 1; i >= 0; i-- {
			text = strings.ReplaceAll(text, forms[i], "[REDACTED]")
		}
	}
	return text
}

// SanitizeObservationValue creates a JSON-compatible redacted copy without changing execution data.
func SanitizeObservationValue(ctx context.Context, value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return "[unavailable]"
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if dec.Decode(&out) != nil {
		return "[unavailable]"
	}
	var scrub func(any) any
	scrub = func(v any) any {
		switch x := v.(type) {
		case string:
			return SanitizeObservationText(ctx, x)
		case map[string]any:
			for k, v := range x {
				x[k] = scrub(v)
			}
			return x
		case []any:
			for i, v := range x {
				x[i] = scrub(v)
			}
			return x
		default:
			return v
		}
	}
	return scrub(out)
}
