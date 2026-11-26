package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestCloneAgentConfigClonesFixedResponses(t *testing.T) {
	orig := &types.AgentConfig{
		FixedResponses: map[string]string{
			"OOS":       "range out",
			"NO_ANSWER": "no answer",
		},
	}
	cloned := cloneAgentConfig(orig)
	require.Equal(t, orig.FixedResponses, cloned.FixedResponses)
	cloned.FixedResponses["OOS"] = "mutated"
	require.Equal(t, "range out", orig.FixedResponses["OOS"])
}

func TestBuildAgentStepMessagesPreservesStructuredReferenceURLs(t *testing.T) {
	persisted := tools.SanitizeAgentStepsForStorage([]types.AgentStep{{
		Iteration: 0,
		ToolCalls: []types.ToolCall{
			{
				ID:   "call-search",
				Name: tools.ToolSearchKnowledge,
				Result: &types.ToolResult{
					Success: true,
					Output:  "<retrieval>raw xml</retrieval>",
					Data: map[string]any{
						"display_type": "search_results",
						"results": []map[string]any{
							{"source_url": "HTTPS://Docs.Dobest.Cn/docs-pack/pay/#"},
						},
					},
				},
			},
			{
				ID:   "call-read",
				Name: tools.ToolReadDocument,
				Result: &types.ToolResult{
					Success: true,
					Output:  "<retrieval>chunks</retrieval>",
					Data: map[string]any{
						"display_type": "knowledge_chunks_list",
						"source_url":   "https://docs.dobest.cn/docs-client/login/",
						"chunks":       []map[string]any{{"content": "big body"}},
					},
				},
			},
			{
				ID:   "call-fetch",
				Name: tools.ToolWebFetch,
				Result: &types.ToolResult{
					Success: true,
					Data: map[string]any{
						"display_type": "web_fetch_results",
						"results": []map[string]any{
							{"url": "https://developer.apple.com/storekit", "status": "success"},
							{"url": "https://broken.example.com/404", "status": "failed"},
						},
					},
				},
			},
		},
	}})

	msgs := buildAgentStepMessages(persisted[0])
	// assistant + 3 tool messages
	require.Len(t, msgs, 4)
	require.Equal(t, []string{"https://docs.dobest.cn/docs-pack/pay/"}, msgs[1].ReferenceURLs)
	require.Equal(t, []string{"https://docs.dobest.cn/docs-client/login/"}, msgs[2].ReferenceURLs)
	require.Equal(t, []string{"https://developer.apple.com/storekit"}, msgs[3].ReferenceURLs)
}
