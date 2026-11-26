package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type auditStubTool struct {
	agenttools.BaseTool
}

func (s *auditStubTool) Execute(context.Context, json.RawMessage) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true}, nil
}

type cancelAfterSentinelChat struct {
	cancel context.CancelFunc
}

func (c *cancelAfterSentinelChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return &types.ChatResponse{}, nil
}

func (c *cancelAfterSentinelChat) ChatStream(_ context.Context, _ []chat.Message, _ *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	ch := make(chan types.StreamResponse, 1)
	go func() {
		defer close(ch)
		ch <- types.StreamResponse{ResponseType: types.ResponseTypeAnswer, Content: "[[FIXED:OOS"}
		c.cancel()
	}()
	return ch, nil
}

func (c *cancelAfterSentinelChat) GetModelName() string { return "cancel-test" }
func (c *cancelAfterSentinelChat) GetModelID() string   { return "cancel-test-id" }

func TestProjectInputIDIgnoresRotatingAppInfoToken(t *testing.T) {
	e1 := &AgentEngine{appInfo: `{"app_id":"10001","token":"tok-1","channel":"wechat"}`}
	e2 := &AgentEngine{appInfo: `{"app_id":"10001","token":"tok-2","channel":"wechat"}`}
	require.Equal(t, e1.projectInputID(), e2.projectInputID())

	rendered := renderAppInfoBlock(e1.appInfo)
	require.Contains(t, rendered, "tok-1")
	require.Contains(t, rendered, `"app_id":"10001"`)
}

func TestRuntimeDocumentObservedIncludesQuestionOriginAndPageContext(t *testing.T) {
	e := &AgentEngine{
		retrieval: newRetrievalControl("check doc", "kb:1"),
		questionOrigin: &QuestionOriginInfo{
			Document: &SelectedDocumentInfo{KnowledgeID: "doc-origin"},
		},
		pageContext: &PageContextInfo{
			Document: &SelectedDocumentInfo{KnowledgeID: "doc-page"},
			Message: &PageContextMessage{
				References: []*PageContextReference{{KnowledgeID: "doc-ref"}},
			},
		},
	}
	state := e.retrieval.snapshot()
	require.True(t, e.runtimeDocumentObserved(state, "doc-origin"))
	require.True(t, e.runtimeDocumentObserved(state, "doc-page"))
	require.True(t, e.runtimeDocumentObserved(state, "doc-ref"))
	require.False(t, e.runtimeDocumentObserved(state, "doc-unknown"))
}

func TestRetrievalControlRollsBackOnPersistErrorAndPublicAnswerChecksWithoutAppInfo(t *testing.T) {
	c := newRetrievalControl("check config", "kb:1")
	id, err := c.register(retrievalFactProposal{GoalQuote: "check config", Fact: "config"})
	require.NoError(t, err)

	c.persist = func(retrievalState) error { return errors.New("db write failed") }
	err = c.acquire(id, "call-1", retrievalInitial, "", "search_knowledge", `{"query":"config"}`)
	require.ErrorIs(t, err, errRetrievalStateSave)
	require.Empty(t, c.snapshot().Attempts, "failed persist must roll back in-memory state")
	require.Equal(t, retrievalPending, c.snapshot().Gaps[0].Status)

	e := &AgentEngine{
		config:    &types.AgentConfig{},
		retrieval: c,
		// AppInfo intentionally empty: damaged state must still be caught!
	}
	_, err = e.publicAnswer(context.Background(), "some answer", nil)
	require.ErrorIs(t, err, errRetrievalStateSave)
}

func TestApplyRetrievalReviewAllowsNewFactWithoutDistinctFromWhenGuardEnabled(t *testing.T) {
	c := newRetrievalControl("check login and pay", "kb:1")
	_, err := c.register(retrievalFactProposal{GoalQuote: "check login", Fact: "login"})
	require.NoError(t, err)

	review := retrievalReview{
		NewFact:           &retrievalFactProposal{GoalQuote: "pay", Fact: "pay"},
		Status:            retrievalNeedsUser,
		MissingUserFields: []string{"order_id"},
	}
	require.Error(t, c.applyReview(review))
	require.NoError(t, c.applyReviewWithOptions(review, false))
	require.Len(t, c.snapshot().Gaps, 2)
}

func TestRenderUserTurnContentHandlesNilSkillsManagerForPinnedSkills(t *testing.T) {
	e := NewAgentEngine(&types.AgentConfig{}, nil, agenttools.NewToolRegistry(), event.NewEventBus(), nil, nil, "s1", "")
	e.pinnedSkills = []*PinnedSkillInfo{{Name: "pdf"}}
	rendered := e.RenderUserTurnContent("s1", "hello")
	require.NotContains(t, rendered, "skill://pdf/SKILL.md")
	require.Contains(t, rendered, "<skill_unavailable>")
}

func TestCancelBeforeStreamingDropsRawFixedSentinel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := NewAgentEngine(&types.AgentConfig{
		FixedResponses: map[string]string{"OOS": "Out of scope answer"},
	}, &cancelAfterSentinelChat{cancel: cancel}, agenttools.NewToolRegistry(), event.NewEventBus(), nil, nil, "s1", "")

	resp, err := e.streamThinkingToEventBus(ctx, emptyMessages(), emptyTools(), 0, "s1")
	if err == nil && resp != nil {
		require.NotContains(t, resp.Content, "[[FIXED:")
	}
}

func TestRegistryListModelToolsExcludesDeferred(t *testing.T) {
	r := agenttools.NewToolRegistry()
	r.RegisterTool(&auditStubTool{BaseTool: agenttools.NewBaseTool("active_tool", "desc", json.RawMessage(`{}`))})
	r.RegisterDeferredTool(&auditStubTool{BaseTool: agenttools.NewBaseTool("deferred_tool", "desc", json.RawMessage(`{}`))})
	require.Equal(t, []string{"active_tool", "deferred_tool"}, r.ListTools())
	require.Equal(t, []string{"active_tool"}, r.ListModelTools())
}
