package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// suspendedSnapshot runs a turn until it suspends on call-b and returns the
// saved run state.
func suspendedSnapshot(t *testing.T) []byte {
	t.Helper()
	f := newSuspendFixture(t, "first_tool", false)
	f.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))
	state := f.run(t)
	require.NotNil(t, state.Suspended)
	require.NoError(t, f.blocked.err)
	return f.blocked.snapshot
}

func TestResumeRestoresShownSearchChunks(t *testing.T) {
	snapshot := suspendedSnapshot(t)
	snap, err := DecodeRunSnapshot(snapshot)
	require.NoError(t, err)
	snap.CompletedSteps = append(snap.CompletedSteps, types.AgentStep{ToolCalls: []types.ToolCall{{
		Name:   agenttools.ToolSearchKnowledge,
		Result: &types.ToolResult{Success: true, Data: map[string]interface{}{"results": []map[string]interface{}{{"chunk_id": "completed"}}}},
	}}})
	snap.PendingRound.Step.ToolCalls[0].Name = agenttools.ToolSearchKnowledge
	snap.PendingRound.Step.ToolCalls[0].Result = &types.ToolResult{Success: true, Data: map[string]interface{}{"results": []map[string]interface{}{{"faq_id": "pending"}}}}
	snapshot, err = json.Marshal(snap)
	require.NoError(t, err)
	f := newSuspendFixture(t, "first_tool", false)
	f.engine.chatModel.(*mockChat).responses = []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "done", Done: true, FinishReason: "stop"}}}}
	_, err = f.engine.Resume(t.Context(), "sess-1", "msg-1", nil, snapshot, answeredPrompt("call-b"))
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"completed": true, "pending": true}, f.engine.snapshotTurnShownChunks())
}

// An old snapshot without structured sources loses its ledger, so the model sees
// the earlier search as omitted. Its chunks must not count as shown either, or a
// limit=1 re-search would skip the best result the model never saw.
func TestResumeWithDiscardedLedgerDoesNotMarkOmittedChunksShown(t *testing.T) {
	snapshot := suspendedSnapshot(t)
	snap, err := DecodeRunSnapshot(snapshot)
	require.NoError(t, err)
	snap.CompletedSteps = append(snap.CompletedSteps, types.AgentStep{ToolCalls: []types.ToolCall{{
		ID: "search-old", Name: agenttools.ToolSearchKnowledge,
		Result: &types.ToolResult{Success: true, Data: map[string]interface{}{"results": []map[string]interface{}{{"chunk_id": "best"}}}},
	}}})
	snap.Retrieval = &retrievalState{Version: 3, Goal: "test query", Scope: "scope", Scopes: []string{"scope"}, Attempts: []retrievalAttempt{{
		ID: "search-old", Scope: "scope", Tool: agenttools.ToolSearchKnowledge, Completed: true, Outcome: retrievalSuccess,
	}}}
	snapshot, err = json.Marshal(snap)
	require.NoError(t, err)

	f := newSuspendFixture(t, "first_tool", false)
	f.engine.config.SearchTargets = types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 1}}
	f.engine.toolRegistry.RegisterTool(newCountingTool(agenttools.ToolSearchKnowledge))
	model := f.engine.chatModel.(*mockChat)
	model.responses = []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "done", Done: true, FinishReason: "stop"}}}}
	_, err = f.engine.Resume(t.Context(), "sess-1", "msg-1", nil, snapshot, answeredPrompt("call-b"))
	require.NoError(t, err)

	require.Len(t, model.calls, 1)
	var seen string
	for _, m := range model.calls[0] {
		if m.ToolCallID == "search-old" {
			seen = m.Content
		}
	}
	require.Equal(t, previousRetrievalOmitted, seen)
	require.NotContains(t, f.engine.snapshotTurnShownChunks(), "best")
}

func answeredPrompt(callID string) *types.AgentPrompt {
	return &types.AgentPrompt{
		ID: "p1", Kind: types.AgentPromptKindToolApproval, ToolCallID: callID,
		Status: types.AgentPromptAnswered, Response: types.JSON(`{"approved":true}`),
	}
}

func TestResumeFinishesInterruptedRoundAndContinues(t *testing.T) {
	snapshot := suspendedSnapshot(t)

	// Another replica: fresh engine, the model only answers.
	f := newSuspendFixture(t, "first_tool", false)
	model := f.engine.chatModel.(*mockChat)
	model.responses = []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "all done", Done: true, FinishReason: "stop"},
	}}}
	f.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))

	state, err := f.engine.Resume(context.Background(), "sess-1", "msg-1", nil, snapshot, answeredPrompt("call-b"))
	require.NoError(t, err)
	require.True(t, state.IsComplete)
	require.Nil(t, state.Suspended)
	require.Equal(t, "all done", state.FinalAnswer)

	require.Zero(t, f.first.calls, "calls that finished before the suspension are not rerun")
	require.NotNil(t, f.blocked.answered)
	require.Equal(t, "p1", f.blocked.answered.ID)
	require.Equal(t, 1, f.after.calls)

	require.Len(t, state.RoundSteps, 2)
	round := state.RoundSteps[0]
	require.Equal(t, "checking first", round.Thought)
	require.Len(t, round.ToolCalls, 3)
	for i, id := range []string{"call-a", "call-b", "call-c"} {
		require.Equal(t, id, round.ToolCalls[i].ID)
		require.True(t, round.ToolCalls[i].Result.Success, id)
	}

	// The model sees the original turn and every tool result of the round.
	require.Len(t, model.calls, 1)
	sent := model.calls[0]
	var toolResults []string
	sawQuery := false
	for _, m := range sent {
		if m.Role == "tool" {
			toolResults = append(toolResults, m.ToolCallID)
		}
		if m.Role == "user" && m.Content != "" {
			sawQuery = true
		}
	}
	require.True(t, sawQuery)
	require.Equal(t, []string{"call-a", "call-b", "call-c"}, toolResults)

	require.NotNil(t, f.completed)
	require.Nil(t, f.completed.Suspended)
}

func TestResumeRejectsPromptForAnotherCall(t *testing.T) {
	snapshot := suspendedSnapshot(t)
	f := newSuspendFixture(t, "first_tool", false)
	_, err := f.engine.Resume(context.Background(), "sess-1", "msg-1", nil, snapshot, answeredPrompt("call-c"))
	require.Error(t, err)
	require.Nil(t, f.blocked.answered)
	require.Zero(t, f.after.calls)
}

func TestResumeSuspendsAgainOnALaterPrompt(t *testing.T) {
	snapshot := suspendedSnapshot(t)
	snap, err := DecodeRunSnapshot(snapshot)
	require.NoError(t, err)
	// The call after the blocked one raises a prompt of its own.
	snap.PendingRound.Calls[2].Name = suspendingToolName
	snap.PendingRound.Calls[2].ID = "call-d"
	snapshot, err = json.Marshal(snap)
	require.NoError(t, err)

	f := newSuspendFixture(t, "first_tool", false)
	f.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))
	model := f.engine.chatModel.(*mockChat)
	model.responses = nil

	// The blocked call consumes the answer; the next one suspends.
	state, err := f.engine.Resume(context.Background(), "sess-1", "msg-1", nil, snapshot,
		answeredPrompt("call-b"))
	require.NoError(t, err)
	require.NotNil(t, f.blocked.answered)
	require.NotNil(t, state.Suspended)
	require.Empty(t, model.calls, "a re-suspended run makes no model call")

	again, err := DecodeRunSnapshot(f.blocked.snapshot)
	require.NoError(t, err)
	require.Equal(t, 2, again.PendingRound.BlockedIndex)
	require.Len(t, again.PendingRound.Step.ToolCalls, 2)
	require.Equal(t, "call-b", again.PendingRound.Step.ToolCalls[1].ID)

	require.NotNil(t, f.completed)
	require.Equal(t, state.Suspended, f.completed.Suspended)
}

func TestClosedStepsFailUnfinishedCalls(t *testing.T) {
	snap, err := DecodeRunSnapshot(suspendedSnapshot(t))
	require.NoError(t, err)
	steps := snap.ClosedSteps("expired")
	require.Len(t, steps, 1)
	calls := steps[0].ToolCalls
	require.Len(t, calls, 3)
	require.True(t, calls[0].Result.Success)
	for _, c := range calls[1:] {
		require.False(t, c.Result.Success)
		require.Equal(t, "expired", c.Result.Error)
	}
	require.Equal(t, "/etc", calls[1].Args["path"])
}

func TestResumeReplaysSteeringAtItsOriginalBoundaries(t *testing.T) {
	snap, err := DecodeRunSnapshot(suspendedSnapshot(t))
	require.NoError(t, err)
	snap.CompletedSteps = []types.AgentStep{{Thought: "previous round", UserMessagesBefore: []string{"u1"}}}
	snap.PendingRound.Step.UserMessagesBefore = []string{"u2"}
	snap.UserMessages = map[string]string{"u1": "use staging", "u2": "only two replicas"}
	data, err := json.Marshal(snap)
	require.NoError(t, err)
	f := newSuspendFixture(t, "first_tool", false)
	model := f.engine.chatModel.(*mockChat)
	model.responses = []mockResponse{{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer, Content: "done", Done: true, FinishReason: "stop",
	}}}}
	_, err = f.engine.Resume(t.Context(), "sess-1", "msg-1", nil, data, answeredPrompt("call-b"))
	require.NoError(t, err)
	var contents, images []string
	for _, m := range model.calls[0] {
		if len(m.MultiContent) == 0 {
			contents = append(contents, m.Content)
			images = append(images, m.Images...)
			continue
		}
		for _, part := range m.MultiContent {
			switch part.Type {
			case "text":
				contents = append(contents, part.Text)
			case "image_url":
				require.NotNil(t, part.ImageURL)
				images = append(images, part.ImageURL.URL)
			}
		}
	}
	require.Equal(t, []string{"https://img/1.png"}, images)
	// The provider normalization can merge adjacent user messages, including
	// the original query and the first steering message.
	transcript := strings.Join(contents, "\n")
	previous := -1
	for _, content := range []string{
		types.SteerMessageContent("use staging"), "previous round",
		types.SteerMessageContent("only two replicas"), "checking first",
	} {
		position := strings.Index(transcript, content)
		require.Greater(t, position, previous, "missing or misplaced content: %s", content)
		previous = position
	}
}

func TestSteeringTextSurvivesAnotherSuspension(t *testing.T) {
	f := newSuspendFixture(t, "first_tool", false)
	f.engine.EnableSuspension(nil)
	f.engine.SetSteerSink(&fakeSteerSink{queued: []map[string]interface{}{steerEntry("s1", "use staging")}})
	state := f.run(t)
	require.NotNil(t, state.Suspended)
	snap, err := DecodeRunSnapshot(f.blocked.snapshot)
	require.NoError(t, err)
	require.Len(t, snap.PendingRound.Step.UserMessagesBefore, 1)
	id := snap.PendingRound.Step.UserMessagesBefore[0]
	require.Equal(t, "use staging", snap.UserMessages[id])
	// A second suspension retains the already-consumed steering text.
	snap.PendingRound.Calls[2].Name = suspendingToolName
	data, err := json.Marshal(snap)
	require.NoError(t, err)
	f2 := newSuspendFixture(t, "first_tool", false)
	f2.engine.EnableSuspension(nil)
	_, err = f2.engine.Resume(t.Context(), "sess-1", "msg-1", nil, data, answeredPrompt("call-b"))
	require.NoError(t, err)
	again, err := DecodeRunSnapshot(f2.blocked.snapshot)
	require.NoError(t, err)
	require.Equal(t, snap.UserMessages, again.UserMessages)
}
