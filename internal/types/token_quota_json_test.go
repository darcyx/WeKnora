package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTokenQuotaUsageSnapshotUsesSnakeCaseUsageFields(t *testing.T) {
	payload, err := json.Marshal(TokenQuotaUsageSnapshot{
		SubjectID: "external-user-1",
		Daily: &TokenQuotaPeriodUsage{
			TotalTokens:    42,
			ReservedTokens: 7,
		},
	})
	if err != nil {
		t.Fatalf("marshal token quota snapshot: %v", err)
	}

	encoded := string(payload)
	for _, field := range []string{"\"total_tokens\":42", "\"reserved_tokens\":7"} {
		if !strings.Contains(encoded, field) {
			t.Fatalf("expected %s in %s", field, encoded)
		}
	}
	if strings.Contains(encoded, "TotalTokens") || strings.Contains(encoded, "ReservedTokens") {
		t.Fatalf("usage fields must not expose Go field names: %s", encoded)
	}
}
