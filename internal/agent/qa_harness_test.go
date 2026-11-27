package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	harnessQuery = "taptap沙盒支付怎么设置"
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

	attempts    []retrievalAttempt
	toolCalls   int
	mainCalls   int
	guardCalls  int
	guardSeen   []string
	errorEvents []string
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
	e.EnableRuntimeRetrieval()

	res := &harnessResult{}
	e.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, ev event.Event) error {
		data, ok := ev.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
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
		res.attempts = snapshot.Attempts
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
	require.Equal(t, 0, res.guardCalls, "deterministic RuntimeRetrieval never spends synchronous LLM guard calls")
	if res.state != nil {
		require.LessOrEqual(t, res.state.CurrentRound, maxIterations, "the loop never exceeds its round budget")
	}
	require.Len(t, res.attempts, res.toolCalls, "every executed retrieval has a recorded attempt")
	for _, attempt := range res.attempts {
		require.True(t, attempt.Completed, "a turn never ends with an unobserved attempt")
	}
}

// TestQAInstabilityHarness is the matrix: one row per failure mode, every row
// checked against the same invariants plus its own expectation.
func TestQAInstabilityHarness(t *testing.T) {
	const maxIterations = 8
	type scenario struct {
		name   string
		rounds []chaosRound
		check  func(t *testing.T, res *harnessResult)
	}
	allowThenAnswer := []chaosRound{harnessToolRound(harnessSearchCall("s1", harnessQuery)), harnessAnswerRound("沙盒支付无需真实扣款。")}

	scenarios := []scenario{
		{
			name:   "stable model: search then answer with zero guard calls",
			rounds: allowThenAnswer,
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer)
				require.Len(t, res.attempts, 1)
				require.Zero(t, res.guardCalls)
			},
		},
		{
			name: "model repeats the exact same search: blocked immediately",
			rounds: []chaosRound{
				harnessToolRound(harnessSearchCall("s1", harnessQuery)),
				harnessToolRound(harnessSearchCall("s2", harnessQuery)),
				harnessAnswerRound("沙盒支付无需真实扣款。"),
			},
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, 1, res.toolCalls, "exact duplicate search is blocked deterministically")
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer)
			},
		},
		{
			name: "model rewords searches past budget: capped at maxSearchesPerQuestion",
			rounds: []chaosRound{
				harnessToolRound(harnessSearchCall("s1", "沙盒支付")),
				harnessToolRound(harnessSearchCall("s2", "沙盒 支付 配置")),
				harnessToolRound(harnessSearchCall("s3", "TapTap 沙盒支付怎么配置")),
				harnessToolRound(harnessSearchCall("s4", "沙盒支付流程")),
				harnessToolRound(harnessSearchCall("s5", "沙盒测试白名单")),
				harnessToolRound(harnessSearchCall("s6", "沙盒支付回调")),
				harnessAnswerRound("沙盒支付无需真实扣款。"),
			},
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, maxSearchesPerQuestion, res.toolCalls,
					"rewording cannot exceed the per-question search budget")
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer, "the turn still ends with an answer")
			},
		},
		{
			name:   "model answers with no evidence at all",
			rounds: []chaosRound{harnessAnswerRound("沙盒支付无需真实扣款。")},
			check: func(t *testing.T, res *harnessResult) {
				require.Equal(t, "沙盒支付无需真实扣款。", res.answer)
				require.Empty(t, res.attempts)
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
			check: func(t *testing.T, res *harnessResult) {
				require.NotEmpty(t, res.answer)
				require.Equal(t, 1, res.toolCalls)
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			model := &chaosChat{rounds: sc.rounds}
			probe := &retrievalProbe{output: `<retrieval><document id="doc-1"/></retrieval>`}
			res := runInstabilityHarness(t, model, probe, maxIterations)
			require.NoError(t, res.runErr, "the turn surfaces its own failure instead of returning an error")
			checkHarnessInvariants(t, res, maxIterations)
			sc.check(t, res)
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
	model := &chaosChat{rounds: rounds}
	probe := &retrievalProbe{output: "evidence"}
	start := time.Now()
	res := runInstabilityHarnessWith(t, model, probe, 3, true)
	require.Less(t, time.Since(start), 5*time.Second, "bounded work, not bounded luck")
	require.LessOrEqual(t, res.state.CurrentRound, 3)
	require.NotEmpty(t, res.answer)
	require.Equal(t, 1, res.answerDones)
	require.LessOrEqual(t, res.toolCalls, 3, "the repeating model cannot execute past its budget")
}
