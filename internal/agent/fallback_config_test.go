package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFinishStalledTurn_UsesConfiguredFallback(t *testing.T) {
	const fixed = "抱歉，暂未找到相关信息。"
	for _, tc := range []struct {
		name       string
		strategy   string
		response   string
		wantAnswer string
	}{
		{"fixed strategy with text", "fixed", "  " + fixed + "\n", fixed},
		{"fixed strategy with blank text", "fixed", "   ", stalledAnswerFallback},
		{"model strategy ignores text", "model", fixed, stalledAnswerFallback},
		{"unset strategy", "", fixed, stalledAnswerFallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newTestEngine(t, &mockChat{}, func(cfg *types.AgentConfig) {
				cfg.FallbackStrategy, cfg.FallbackResponse = tc.strategy, tc.response
			})
			recorder := &truncationRecorder{}
			recorder.attach(engine.eventBus)
			state := &types.AgentState{}

			engine.finishStalledTurn(context.Background(), state, "sess-1",
				&types.ChatResponse{Content: " "}, false, "q", nil)

			assert.Equal(t, tc.wantAnswer, state.FinalAnswer)
			emitted := recorder.snapshot()
			require.NotEmpty(t, emitted)
			assert.Equal(t, tc.wantAnswer, emitted[0].Content)
			assert.True(t, emitted[0].IsFallback)
			if tc.wantAnswer == fixed {
				assert.Contains(t, engine.configuredNoAnswerPrompt(), `"`+fixed+`"`)
			} else {
				assert.Empty(t, engine.configuredNoAnswerPrompt())
			}
		})
	}
}

func TestRuntimeRetrievalStatePrompt_IncludesConfiguredNoAnswer(t *testing.T) {
	const fixed = "**现状**：当前知识库没有查询到该问题对应的解答。"
	engine := newTestEngine(t, &mockChat{}, func(cfg *types.AgentConfig) {
		cfg.FallbackStrategy, cfg.FallbackResponse = "fixed", fixed
	})
	engine.runtimeRetrieval = true
	engine.retrieval = newRetrievalControl("taptap沙盒支付怎么设置", "scope-a")
	engine.retrieval.state.UserInputIDs = []string{"u1"}
	engine.retrieval.state.Gaps = []retrievalGap{{
		ID:          "gap-1",
		Scope:       "scope-a",
		Fact:        "taptap沙盒支付怎么设置",
		Status:      retrievalPending,
		SearchGrace: questionSearchGrace,
	}}
	for i := 0; i < questionSearchGrace; i++ {
		engine.retrieval.state.Attempts = append(engine.retrieval.state.Attempts, retrievalAttempt{
			ID:         fmt.Sprintf("call-%d", i+1),
			QuestionID: "u1",
			GapID:      "gap-1",
			Scope:      "scope-a",
			Tool:       "search_knowledge",
			Action:     retrievalSearch,
			Completed:  true,
			Outcome:    retrievalSuccess,
		})
	}
	prompt := engine.retrievalStatePrompt()
	assert.Contains(t, prompt, "The ordinary search allowance is spent.")
	assert.Contains(t, prompt, "do not read unrelated or placeholder docs")
	assert.Contains(t, prompt, `"`+fixed+`"`)
}
