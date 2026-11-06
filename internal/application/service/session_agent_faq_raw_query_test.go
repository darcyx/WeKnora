package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/stretchr/testify/require"
)

func TestAgentFAQExactQuestionWithHistorySkipsRewrite(t *testing.T) {
	svc, req, cfg := faqEvaluationFixture(t)
	calls := 0
	result := svc.evaluateAgentFAQ(t.Context(), req, cfg, faqMatchModel{
		response: `{"selected_id":"faq","scope_match":"same"}`,
		onChat:   func() { calls++ },
	}, nil, []chat.Message{{Role: "user", Content: "此前的问题"}})
	require.NotNil(t, result.Match)
	require.Equal(t, 1, calls, "exact hit still checks applicability, but does not rewrite")
	for _, observation := range result.Observations {
		require.NotEqual(t, "faq_rewrite", observation.Stage)
	}
}
