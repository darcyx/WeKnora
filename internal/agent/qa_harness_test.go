package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// Instability harness for the agent QA path.
//
// The model is not trusted: it loops, answers without evidence, invents tool
// arguments, returns protocol junk where a decision is expected, and the
// provider sometimes just fails. This harness injects exactly those failures
// into the two lanes that exist in a turn — the main reasoning lane and the
// boundary/guard lane — and asserts the properties the flow must keep no matter
// what the model does. It is deterministic: no network, no wall-clock, no
// randomness.
//
// It answers the question "is this complexity necessary?" with evidence: every
// guard below is tied to a scenario that fails without it.

const (
	harnessQuery = "TapTap 沙盒支付怎么配置"
	harnessKB    = "kb"
)

// chaosChat routes the two lanes of a turn:
//   - ChatStream is the main reasoning lane (scripted rounds).
//   - Chat with an isolated 768-token budget is the boundary/guard lane.
//   - any other isolated Chat is the answer-repair side call.
type chaosChat struct {
	mu sync.Mutex

	rounds []chaosRound
	// guard replies, indexed by guard call number (1-based); nil means "allow".
	guard func(n int) (string, error)

	mainCalls  int
	guardCalls int
	repairCall int
	guardSeen  []string
}

// chaosRound is one scripted main-lane round: either streamed chunks or the
// provider error the call raises.
type chaosRound struct {
	chunks []types.StreamResponse
	err    error
}

func (m *chaosChat) ChatStream(
	_ context.Context, messages []chat.Message, opts *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if opts != nil && opts.Isolated {
		return nil, errors.New("harness: isolated call routed to the streaming lane")
	}
	if m.mainCalls >= len(m.rounds) {
		return nil, fmt.Errorf("harness: main lane ran past its script (%d rounds)", len(m.rounds))
	}
	round := m.rounds[m.mainCalls]
	m.mainCalls++
	if round.err != nil {
		return nil, round.err
	}
	ch := make(chan types.StreamResponse, len(round.chunks))
	for _, chunk := range round.chunks {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

func (m *chaosChat) GetModelName() string { return "harness-model" }
func (m *chaosChat) GetModelID() string   { return "harness-model" }

func (m *chaosChat) Chat(
	_ context.Context, messages []chat.Message, opts *chat.ChatOptions,
) (*types.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if opts == nil || !opts.Isolated {
		return nil, errors.New("harness: non-isolated call routed to the sync lane")
	}
	if opts.MaxTokens != 768 {
		m.repairCall++
		return &types.ChatResponse{Content: "修复后的回答"}, nil
	}
	m.guardCalls++
	payload := ""
	if len(messages) > 0 {
		payload = messages[len(messages)-1].Content
	}
	m.guardSeen = append(m.guardSeen, payload)
	if m.guard == nil {
		return &types.ChatResponse{Content: `{"decision":"allow","reason":"evidence is requested"}`}, nil
	}
	reply, err := m.guard(m.guardCalls)
	if err != nil {
		return nil, err
	}
	return &types.ChatResponse{Content: reply}, nil
}

// --- main-lane round builders -------------------------------------------------

func harnessToolRound(calls ...types.LLMToolCall) chaosRound {
	return chaosRound{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "tool_calls", ToolCalls: calls,
	}}}
}

// harnessEmptyRound is a natural stop with no content at all.
func harnessEmptyRound() chaosRound {
	return chaosRound{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "stop"}}}
}

func harnessAnswerRound(answer string) chaosRound {
	return chaosRound{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer, Content: answer, Done: true, FinishReason: "stop",
	}}}
}

func harnessSearchCall(id, query string) types.LLMToolCall {
	return types.LLMToolCall{ID: id, Function: types.FunctionCall{
		Name: agenttools.ToolSearchKnowledge, Arguments: fmt.Sprintf(`{"query":%q}`, query),
	}}
}

// --- guard-lane replies -------------------------------------------------------

func guardAllows(int) (string, error) {
	return `{"decision":"allow","reason":"the fact is required"}`, nil
}

func guardTimeout(int) (string, error) { return "", context.DeadlineExceeded }

func guardJunk(int) (string, error) { return `not a decision at all`, nil }

func guardNeedsUser(fields ...string) func(int) (string, error) {
	encoded, _ := json.Marshal(fields)
	return func(int) (string, error) {
		return fmt.Sprintf(`{"decision":"needs_user","missing_user_fields":%s,"reason":"business rules require it"}`, encoded), nil
	}
}

func guardReuses(int) (string, error) {
	return `{"decision":"reuse","gap_id":"gap-1","reason":"already asked"}`, nil
}

// --- engine + result ----------------------------------------------------------

type harnessResult struct {
	answer      string
	answerDones int
	answers     []string
	runErr      error
	state       *types.AgentState

	gaps        []retrievalGap
	attempts    []retrievalAttempt
	toolCalls   int
	mainCalls   int
	guardCalls  int
	guardSeen   []string
	errorEvents []string
}

func (r *harnessResult) gapStatuses() []string {
	out := make([]string, 0, len(r.gaps))
	for _, gap := range r.gaps {
		out = append(out, string(gap.Status))
	}
	return out
}

func runInstabilityHarness(
	t *testing.T, model *chaosChat, probe *retrievalProbe, maxIterations int,
) *harnessResult {
	t.Helper()
	return runInstabilityHarnessWith(t, model, probe, maxIterations, false)
}

// runInstabilityHarnessWith runs the same matrix row with the round budget
// expressed either as a plain MaxIterations or as an unlimited run bounded by
// the absolute hard cap.
func runInstabilityHarnessWith(
	t *testing.T, model *chaosChat, probe *retrievalProbe, maxIterations int, unlimited bool,
) *harnessResult {
	t.Helper()
	e := newTestEngine(t, model)
	if unlimited {
		e.config.MaxIterations = types.UnlimitedMaxIterations
		e.config.HardMaxIterations = maxIterations
	} else {
		e.config.MaxIterations = maxIterations
	}
	e.config.SearchTargets = types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: harnessKB, TenantID: 1}}
	e.toolRegistry = agenttools.NewToolRegistry()
	probe.BaseTool = agenttools.NewBaseTool(agenttools.ToolSearchKnowledge, "test search", json.RawMessage(`{
		"type":"object","properties":{"query":{"type":"string"},"knowledge_base_ids":{"type":"array","items":{"type":"string"}}},"required":["query"],"additionalProperties":false
	}`))
	e.toolRegistry.RegisterTool(probe)
	// The production wiring: boundary decisions plus the runtime-managed,
	// metadata-optional retrieval protocol.
	e.EnableRuntimeRetrieval()

	res := &harnessResult{}
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
		data, ok := ev.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		// A turn closes its answer stream with exactly one Done marker; content
		// arrives on the chunks before it (or, for single-shot paths, on the
		// Done event itself).
		if data.Done {
			res.answerDones++
			if data.Content != "" {
				res.answer = data.Content
				res.answers = append(res.answers, data.Content)
			}
			return nil
		}
		if data.Content != "" {
			res.answer = data.Content
			res.answers = append(res.answers, data.Content)
		}
		return nil
	})
	e.eventBus.On(event.EventError, func(_ context.Context, ev event.Event) error {
		if data, ok := ev.Data.(event.ErrorData); ok {
			res.errorEvents = append(res.errorEvents, data.Error)
		}
		return nil
	})

	state, err := e.Execute(t.Context(), "harness-session", "harness-message", harnessQuery, nil)
	res.runErr, res.state = err, state
	if e.retrieval != nil {
		snapshot := e.retrieval.snapshot()
		res.gaps, res.attempts = snapshot.Gaps, snapshot.Attempts
	}
	res.toolCalls = len(probe.calls)
	model.mu.Lock()
	res.mainCalls, res.guardCalls = model.mainCalls, model.guardCalls
	res.guardSeen = append([]string(nil), model.guardSeen...)
	model.mu.Unlock()
	return res
}

// checkHarnessInvariants are the properties that must hold regardless of model
// behaviour. A violation is a defect in the flow, not in the scenario.
func checkHarnessInvariants(t *testing.T, res *harnessResult, maxIterations int) {
	t.Helper()
	require.Equal(t, 1, res.answerDones, "a turn publishes exactly one terminal answer")
	if res.state != nil {
		require.LessOrEqual(t, res.state.CurrentRound, maxIterations, "the loop never exceeds its round budget")
	}
	// Every dispatched knowledge-base call is observable in the ledger: no
	// evidence can be gathered behind the harness's back.
	require.Len(t, res.attempts, res.toolCalls, "every executed retrieval has a recorded attempt")
	for _, attempt := range res.attempts {
		require.True(t, attempt.Completed, "a turn never ends with an unobserved attempt")
	}
}

// TestQAInstabilityHarness is the matrix: one row per failure mode, every row
// checked against the same invariants plus its own expectation.
func TestQAInstabilityHarness(t *testing.T) {
	const maxIterations = 4
	type scenario struct {
		name   string
		rounds []chaosRound
		guard  func(int) (string, error)
		check  func(t *testing.T, res *harnessResult)
	}
	allowThenAnswer := []chaosRound{harnessToolRound(harnessSearchCall("s1", harnessQuery)), harnessAnswerRound("沙盒支付无需真实扣款。")}

	scenarios := []scenario{
		{
			name:   "stable model: search then answer",
			rounds: allowThenAnswer,
			guard:  guardAllows,
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer)
				require.Len(t, res.attempts, 1)
				require.Equal(t, 1, res.guardCalls, "one boundary decision per dispatched fact")
				require.Equal(t, []string{"pending"}, res.gapStatuses())
			},
		},
		{
			name:   "guard lane times out: retrieval must still run",
			rounds: allowThenAnswer,
			guard:  guardTimeout,
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer)
				require.Equal(t, 1, res.toolCalls, "fail-open: the search still reaches the tool")
				require.Len(t, res.gaps, 1, "the fact is admitted under the ledger's own rules")
			},
		},
		{
			name:   "guard lane returns protocol junk: retrieval must still run",
			rounds: allowThenAnswer,
			guard:  guardJunk,
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, 1, res.toolCalls)
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer)
			},
		},
		{
			name:   "guard demands a missing field: no search, ask instead",
			rounds: []chaosRound{harnessToolRound(harnessSearchCall("s1", harnessQuery)), harnessAnswerRound("沙盒支付无需真实扣款。")},
			guard:  guardNeedsUser("平台"),
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, 0, res.toolCalls, "a confirmed blocker stops dependent retrieval")
				require.Contains(t, res.answer, "平台", "the user is asked for the blocking field")
				require.Equal(t, []string{"needs_user"}, res.gapStatuses())
			},
		},
		{
			name: "model repeats the same search: bounded, no infinite loop",
			rounds: []chaosRound{
				harnessToolRound(harnessSearchCall("s1", harnessQuery)),
				harnessToolRound(harnessSearchCall("s2", harnessQuery)),
				harnessToolRound(harnessSearchCall("s3", harnessQuery)),
				harnessToolRound(harnessSearchCall("s4", harnessQuery)),
				harnessToolRound(harnessSearchCall("s5", harnessQuery)),
				harnessAnswerRound("沙盒支付无需真实扣款。"),
			},
			guard: guardAllows,
			check: func(t *testing.T, res *harnessResult) {
				require.LessOrEqual(t, res.toolCalls, maxIterations, "a repeating model cannot exceed the round budget")
				require.NotEmpty(t, res.answer)
			},
		},
		{
			name: "model dodges the repeat guard by rewording the search",
			rounds: []chaosRound{
				harnessToolRound(harnessSearchCall("s1", "沙盒支付")),
				harnessToolRound(harnessSearchCall("s2", "沙盒 支付 配置")),
				harnessToolRound(harnessSearchCall("s3", "TapTap 沙盒支付怎么配置")),
				harnessToolRound(harnessSearchCall("s4", "沙盒支付流程")),
				harnessAnswerRound("沙盒支付无需真实扣款。"),
			},
			guard: guardAllows,
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, questionSearchGrace, res.toolCalls,
					"rewording does not reset the per-question search allowance")
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer, "the turn still ends with an answer")
			},
		},
		{
			name:   "model answers with no evidence at all",
			rounds: []chaosRound{harnessAnswerRound("沙盒支付无需真实扣款。")},
			guard:  guardAllows,
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer, "an unconfirmed verdict never replaced the answer")
				require.Empty(t, res.gaps)
			},
		},
		{
			name:   "model answers without evidence and the guard says reuse",
			rounds: []chaosRound{harnessAnswerRound("沙盒支付无需真实扣款。")},
			guard:  guardReuses,
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer, "reuse is not a reason to withhold the answer")
			},
		},
		{
			name: "model returns empty responses after collecting evidence",
			rounds: []chaosRound{
				harnessToolRound(harnessSearchCall("s1", harnessQuery)),
				harnessEmptyRound(),
				harnessEmptyRound(),
				harnessEmptyRound(),
				harnessAnswerRound("沙盒支付无需真实扣款。"),
			},
			guard: guardAllows,
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer,
					"a stalled model with evidence gets a synthesized answer, not an apology")
				require.NotEqual(t, stalledAnswerFallback, res.answer)
				require.Equal(t, 1, res.toolCalls, "the collected evidence is reused, not re-searched")
			},
		},
		{
			name: "provider fails the main lane after a search",
			rounds: []chaosRound{
				harnessToolRound(harnessSearchCall("s1", harnessQuery)),
				{err: errors.New("provider 500")},
				harnessAnswerRound("只能根据已检索到的内容回答。"),
			},
			guard: guardAllows,
			check: func(t *testing.T, res *harnessResult) {
				// Graceful degradation synthesizes from the evidence already collected.
				require.NotEmpty(t, res.answer)
				require.Equal(t, 1, res.toolCalls)
			},
		},
	}

	t.Logf("%-58s %5s %6s %6s %8s %-12s", "scenario", "main", "guard", "rounds", "attempts", "gap")
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			model := &chaosChat{rounds: sc.rounds, guard: sc.guard}
			probe := &retrievalProbe{output: `<retrieval><document id="doc-1"/></retrieval>`}
			res := runInstabilityHarness(t, model, probe, maxIterations)
			rounds := 0
			if res.state != nil {
				rounds = res.state.CurrentRound
			}
			t.Logf("%-58s %5d %6d %6d %8d %-12s", sc.name, res.mainCalls, res.guardCalls, rounds, len(res.attempts), strings.Join(res.gapStatuses(), ","))
			require.NoError(t, res.runErr, "the turn surfaces its own failure instead of returning an error")
			checkHarnessInvariants(t, res, maxIterations)
			sc.check(t, res)
			// Cost ceiling: the harness may not exceed this many guard calls for
			// any single turn, whatever the model does.
			require.LessOrEqual(t, res.guardCalls, maxIterations, "guard calls stay bounded by the round budget")
		})
	}
}

// TestQAInstabilityHarnessRejectsRunawayLoop pins the failure the harness exists
// to catch: a model that never converges must still finish, and the harness must
// see it finish in bounded time.
func TestQAInstabilityHarnessRejectsRunawayLoop(t *testing.T) {
	rounds := make([]chaosRound, 0, 8)
	for i := 0; i < 8; i++ {
		rounds = append(rounds, harnessToolRound(harnessSearchCall(fmt.Sprintf("s%d", i), harnessQuery)))
	}
	model := &chaosChat{rounds: rounds, guard: guardAllows}
	probe := &retrievalProbe{output: "evidence"}
	start := time.Now()
	// The agent declares unlimited rounds (smart-reasoning); the absolute hard
	// cap is what must stop it.
	res := runInstabilityHarnessWith(t, model, probe, 3, true)
	require.Less(t, time.Since(start), 5*time.Second, "bounded work, not bounded luck")
	require.LessOrEqual(t, res.state.CurrentRound, 3)
	require.NotEmpty(t, res.answer)
	require.Equal(t, 1, res.answerDones)
	require.LessOrEqual(t, res.toolCalls, 3, "the repeating model cannot execute past its budget")
}

// TestQAInstabilityHarnessGuardValue measures what the boundary layer actually
// buys. The same unstable model, with and without the guard, answering a
// question whose business rule requires an unstated condition.
func TestQAInstabilityHarnessGuardValue(t *testing.T) {
	for _, guarded := range []bool{true, false} {
		t.Run(map[bool]string{true: "guard on", false: "guard off"}[guarded], func(t *testing.T) {
			model := &chaosChat{
				rounds: []chaosRound{
					harnessToolRound(harnessSearchCall("s1", harnessQuery)),
					harnessAnswerRound("沙盒支付无需真实扣款。"),
				},
				guard: guardNeedsUser("平台"),
			}
			probe := &retrievalProbe{output: "沙盒支付无需真实扣款。"}

			e := newTestEngine(t, model)
			e.config.MaxIterations = 4
			e.config.SearchTargets = types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: harnessKB, TenantID: 1}}
			e.toolRegistry = agenttools.NewToolRegistry()
			probe.BaseTool = agenttools.NewBaseTool(agenttools.ToolSearchKnowledge, "test search",
				json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`))
			e.toolRegistry.RegisterTool(probe)
			if guarded {
				e.EnableRuntimeRetrieval()
			}

			var answer string
			e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
				if data, ok := ev.Data.(event.AgentFinalAnswerData); ok && data.Content != "" {
					answer = data.Content
				}
				return nil
			})
			_, err := e.Execute(t.Context(), "harness-session", "harness-message", harnessQuery, nil)
			require.NoError(t, err)
			t.Logf("%s: searches=%d guard_calls=%d answer=%q", map[bool]string{true: "guard on", false: "guard off"}[guarded], len(probe.calls), model.guardCalls, answer)
			if guarded {
				require.Empty(t, probe.calls, "the confirmed blocker stops dependent retrieval")
				require.Contains(t, answer, "平台")
			} else {
				require.Len(t, probe.calls, 1, "without the guard the model answers without the required condition")
				require.False(t, strings.Contains(answer, "平台"))
			}
		})
	}
}
