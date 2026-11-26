package agent

import (
	"context"
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
