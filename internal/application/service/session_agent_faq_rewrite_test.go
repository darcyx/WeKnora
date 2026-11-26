package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// A rewrite cut off by the output limit must not replace the user's query.
func TestFAQSearchQuery_TruncatedRewriteKeepsRawQuery(t *testing.T) {
	history := []chat.Message{{Role: "user", Content: "SDK 支付失败"}, {Role: "assistant", Content: "请问是哪个平台？"}}
	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "iOS 上 SDK 支付失败怎么办"},
		{"length", "是 iOS"},
		{"max_tokens", "是 iOS"},
	}
	for _, tc := range cases {
		model := graphReplyChat{resp: &types.ChatResponse{Content: "iOS 上 SDK 支付失败怎么办", FinishReason: tc.finish}}
		got, err := faqSearchQuery(context.Background(), &types.AgentConfig{}, model, "是 iOS", history)
		if got != tc.want {
			t.Errorf("finish=%q: query = %q, want %q", tc.finish, got, tc.want)
		}
		if (err != nil) != (tc.want == "是 iOS") {
			t.Errorf("finish=%q: err = %v", tc.finish, err)
		}
	}
}

// Semantic hits must meet the floor; exact matches (floor 0) are never gated.
func TestFAQDirectAnswerAllowed(t *testing.T) {
	cases := []struct {
		floor float64
		score float64
		want  bool
	}{
		{0, 0.01, true},
		{0.6, 0.6, true},
		{0.6, 0.59, false},
		{0.6, 0.95, true},
	}
	for _, tc := range cases {
		if got := faqDirectAnswerAllowed(tc.floor, tc.score); got != tc.want {
			t.Errorf("floor=%v score=%v: got %v, want %v", tc.floor, tc.score, got, tc.want)
		}
	}
}
