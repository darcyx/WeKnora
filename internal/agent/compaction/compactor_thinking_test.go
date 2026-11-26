package compaction

import (
	"context"
	"testing"

	agenttoken "github.com/Tencent/WeKnora/internal/agent/token"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type captureThinkingChat struct {
	lastOpts *chat.ChatOptions
}

func (c *captureThinkingChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return &types.ChatResponse{}, nil
}

func (c *captureThinkingChat) ChatStream(_ context.Context, _ []chat.Message, opts *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	c.lastOpts = opts
	ch := make(chan types.StreamResponse, 1)
	ch <- types.StreamResponse{
		ResponseType: types.ResponseTypeAnswer,
		Content:      "## Goal\nSummarized cleanly.",
		FinishReason: "stop",
		Done:         true,
	}
	close(ch)
	return ch, nil
}

func (c *captureThinkingChat) GetModelName() string { return "capture-test" }
func (c *captureThinkingChat) GetModelID() string   { return "capture-test-id" }

func TestCompactorSummarizeDisablesThinkingAndScrubsAppInfo(t *testing.T) {
	est, err := agenttoken.NewEstimator()
	require.NoError(t, err)
	model := &captureThinkingChat{}
	c := New(model, est, Settings{Enabled: true, MaxContextTokens: 4096})
	require.NotNil(t, c)

	msgs := []chat.Message{
		{
			Role:    "user",
			Content: `<runtime_context><app_info type="static_project_metadata">{"app_id":"10001","token":"super-secret-token"}</app_info></runtime_context>hello`,
		},
	}

	transcript := serializeConversation(msgs)
	require.NotContains(t, transcript, "super-secret-token")
	require.Contains(t, transcript, `"app_id":"10001"`)

	archive := rawArchive(msgs, 500, est)
	require.NotContains(t, archive, "super-secret-token")
	require.Contains(t, archive, `"app_id":"10001"`)

	summary, err := c.summarize(context.Background(), msgs, 0, "", initialSummarizationInstructions, 512)
	require.NoError(t, err)
	require.Contains(t, summary, "Summarized cleanly")
	require.NotNil(t, model.lastOpts)
	require.NotNil(t, model.lastOpts.Thinking)
	require.False(t, *model.lastOpts.Thinking, "compaction must explicitly disable thinking")
}
