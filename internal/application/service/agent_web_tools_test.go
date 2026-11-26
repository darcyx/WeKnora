package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentWebToolsFollowRuntimeSwitch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		registry := tools.NewToolRegistry()
		svc := &agentService{}
		config := &types.AgentConfig{
			AllowedTools: []string{tools.ToolWebSearch, tools.ToolWebFetch}, WebSearchEnabled: enabled,
		}
		require.NoError(t, svc.registerTools(t.Context(), registry, config, nil, nil, "web-session", nil))
		for _, name := range []string{tools.ToolWebSearch, tools.ToolWebFetch} {
			_, err := registry.GetTool(name)
			assert.Equal(t, enabled, err == nil, name)
		}
		assert.NotContains(t, registry.ListTools(), tools.ToolSearchKnowledge)
		assert.NotContains(t, registry.ListTools(), tools.ToolReadDocument)
	}
}

type emptyWebClient struct{ interfaces.WebClient }

func (emptyWebClient) Capabilities() types.WebSearchCapabilities {
	return types.WebSearchCapabilities{}
}

func (emptyWebClient) Search(context.Context, types.WebSearchRequest) ([]*types.WebSearchResult, error) {
	return nil, nil
}

type emptyWebSearchService struct{}

func (emptyWebSearchService) Resolve(context.Context, string) (interfaces.WebClient, error) {
	return emptyWebClient{}, nil
}

// Web tools are not given the live token; other app_info identifiers stay blocked.
func TestAgentWebToolsDoNotReceiveAppInfoToken(t *testing.T) {
	registry := tools.NewToolRegistry()
	svc := &agentService{webSearchService: emptyWebSearchService{}}
	config := &types.AgentConfig{
		WebSearchEnabled: true, AppInfo: `{"token":"tok_live_9876","app_name":"starfall_cn"}`,
	}
	require.NoError(t, svc.registerTools(t.Context(), registry, config, nil, nil, "web-session", nil))
	search, err := registry.GetTool(tools.ToolWebSearch)
	require.NoError(t, err)

	run := func(query string) *types.ToolResult {
		args, err := json.Marshal(map[string]any{"query": query})
		require.NoError(t, err)
		result, err := search.Execute(t.Context(), args)
		require.NoError(t, err)
		return result
	}
	assert.NotContains(t, run("tok_live_9876 error").Error, "blocked")
	assert.Contains(t, run("starfall_cn error").Error, "blocked")
}
