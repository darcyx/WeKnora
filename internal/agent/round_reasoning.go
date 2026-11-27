package agent

import (
	"context"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// skipPreEvidenceThinkingEnv opts in to running tool rounds without extended
// thinking until the current question has a successful retrieval. Off by
// default: it trades query-planning depth for ~4s per round, so enable it only
// where an eval shows answers hold up.
const skipPreEvidenceThinkingEnv = "WEKNORA_AGENT_SKIP_PRE_EVIDENCE_THINKING"

// roundReasoningEffort is the configured level for this round. With the opt-in
// set, a round that may call tools and has no retrieved result for the current
// question yet runs with thinking off; it mostly picks searches, and the round
// that sees results keeps the configured level.
func (e *AgentEngine) roundReasoningEffort(ctx context.Context, tools []chat.Tool) chat.ReasoningEffort {
	configured := chat.SanitizeReasoningEffort(ctx, e.config.ReasoningEffort, "agent config")
	if len(tools) == 0 || e.retrieval == nil || !agentOptIn(skipPreEvidenceThinkingEnv) ||
		e.retrievedForCurrentQuestion() {
		return configured
	}
	return api.ReasoningOff
}

// agentOptIn reports whether an experimental agent latency switch is set to true.
func agentOptIn(env string) bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(env)), "true")
}

// retrievedForCurrentQuestion reports whether any retrieval for the current
// user message completed successfully. An empty result still counts: the model
// has something to reason about.
func (e *AgentEngine) retrievedForCurrentQuestion() bool {
	state := e.retrieval.snapshot()
	question := state.questionID()
	for _, attempt := range state.Attempts {
		if attempt.Completed && attempt.Outcome == retrievalSuccess && attempt.QuestionID == question {
			return true
		}
	}
	return false
}
