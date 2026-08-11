package types

import "testing"

func TestAgentStateAddUsageAccumulatesModelCalls(t *testing.T) {
	state := &AgentState{}
	state.AddUsage(TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120})
	state.AddUsage(TokenUsage{PromptTokens: 30, CompletionTokens: 10, TotalTokens: 40})

	if state.Usage.PromptTokens != 130 || state.Usage.CompletionTokens != 30 || state.Usage.TotalTokens != 160 {
		t.Fatalf("usage = %+v, want prompt=130 completion=30 total=160", state.Usage)
	}
}
