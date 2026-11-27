package agent

import (
	"context"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

type streamAnswerPipelineConfig struct {
	sessionID                string
	answerID                 string
	thinkingID               string
	iteration                int
	emitThoughts             bool
	suppressLeadingSpace     bool
	deferTerminalOnDone      bool
	holdAnswer               bool
	respectCollaborationHold bool
	emittedEventTypes        map[string]int
}

// streamAnswerPipeline unifies the streaming output gate pipeline shared by
// ReAct thinking rounds (think.go) and final answer synthesis (finalize.go):
// inline <think> splitting, fixed-response sentinel buffering, reference-section
// holdback, live emission, retraction on reset/retry, and final reconciliation.
type streamAnswerPipeline struct {
	engine *AgentEngine
	ctx    context.Context
	cfg    streamAnswerPipelineConfig

	splitter     *agenttools.ThinkStreamSplitter
	sentinelGate *fixedSentinelStreamGate
	refGate      *referenceSectionStreamGate

	streamed        strings.Builder
	thinkingOpen    bool
	answerStreamed  bool
	providerDone    bool
	terminalContent string
}

func (e *AgentEngine) newStreamAnswerPipeline(ctx context.Context, cfg streamAnswerPipelineConfig) *streamAnswerPipeline {
	return &streamAnswerPipeline{
		engine:       e,
		ctx:          ctx,
		cfg:          cfg,
		splitter:     agenttools.NewThinkStreamSplitter(),
		sentinelGate: newFixedSentinelStreamGate(e.config.FixedResponses),
		refGate:      newReferenceSectionStreamGate(),
	}
}

func (p *streamAnswerPipeline) recordEvent(kind string) {
	if p.cfg.emittedEventTypes != nil {
		p.cfg.emittedEventTypes[kind]++
	}
}

func (p *streamAnswerPipeline) EmitThought(content string, done bool) {
	if !p.cfg.emitThoughts || (content == "" && !done) {
		return
	}
	p.recordEvent("thought_chunk")
	p.engine.eventBus.Emit(p.ctx, event.Event{
		ID:        p.cfg.thinkingID,
		Type:      event.EventAgentThought,
		SessionID: p.cfg.sessionID,
		Data: event.AgentThoughtData{
			Content:   content,
			Iteration: p.cfg.iteration,
			Done:      done,
		},
	})
}

// CloseThinking emits the thought Done marker once so the UI flips the thinking
// card to completed before answer streaming begins or when the stream terminates.
func (p *streamAnswerPipeline) CloseThinking() {
	if p.thinkingOpen {
		p.EmitThought("", true)
		p.thinkingOpen = false
	}
}

func (p *streamAnswerPipeline) EmitAnswer(content string) {
	if (p.cfg.respectCollaborationHold && p.engine.holdCollaborationAnswer) || p.cfg.holdAnswer || content == "" {
		return
	}
	if p.cfg.suppressLeadingSpace && !p.answerStreamed && strings.TrimSpace(content) == "" {
		return
	}
	p.CloseThinking()
	p.answerStreamed = true
	p.streamed.WriteString(content)
	p.recordEvent("final_answer_chunk")
	if p.cfg.deferTerminalOnDone {
		logger.Debugf(p.ctx, "[Agent][FinalAnswer] Emitting answer chunk: %d chars", len(content))
		_ = p.engine.eventBus.Emit(p.ctx, event.Event{
			ID:        p.cfg.answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: p.cfg.sessionID,
			Data: event.AgentFinalAnswerData{
				Content: content,
			},
		})
		return
	}
	p.engine.eventBus.Emit(p.ctx, event.Event{
		ID:        p.cfg.answerID,
		Type:      event.EventAgentFinalAnswer,
		SessionID: p.cfg.sessionID,
		Data: event.AgentFinalAnswerData{
			Content:    content,
			Done:       false,
			IsFallback: p.engine.isRuntimeFallbackAnswer(content),
		},
	})
}

// HandleThinkingChunk routes separate-channel reasoning (e.g. reasoning_content).
func (p *streamAnswerPipeline) HandleThinkingChunk(chunk *types.StreamResponse) {
	if !p.cfg.emitThoughts {
		return
	}
	if chunk.Content != "" {
		p.thinkingOpen = true
		p.EmitThought(chunk.Content, false)
	} else if chunk.Done && p.thinkingOpen {
		p.CloseThinking()
	}
}

// HandleContentChunk pushes a plain content chunk through <think> splitting,
// fixed-sentinel buffering, and reference-section holdback.
func (p *streamAnswerPipeline) HandleContentChunk(chunk *types.StreamResponse) {
	if p.cfg.deferTerminalOnDone {
		if p.cfg.holdAnswer {
			return
		}
		_, answerContent := p.splitter.Feed(chunk.Content)
		if p.splitter.TakeAnswerReset() {
			p.sentinelGate.Reset()
			p.refGate.Reset()
			p.engine.retractAnswer(p.ctx, p.cfg.sessionID, p.cfg.answerID)
			p.streamed.Reset()
			p.answerStreamed = false
		}
		if chunk.Done {
			_, tail := p.splitter.Flush()
			answerContent += tail
			p.providerDone = true
		}
		answerContent = p.refGate.Feed(p.sentinelGate.Feed(answerContent))
		if chunk.Done && !p.sentinelGate.Held() {
			answerContent += p.refGate.Flush()
		}
		if p.providerDone {
			p.terminalContent += answerContent
			return
		}
		if answerContent != "" {
			p.EmitAnswer(answerContent)
		}
		return
	}

	if chunk.Content != "" {
		thinkPart, answerPart := p.splitter.Feed(chunk.Content)
		if p.splitter.TakeAnswerReset() {
			p.sentinelGate.Reset()
			p.refGate.Reset()
			if p.answerStreamed {
				p.engine.retractAnswer(p.ctx, p.cfg.sessionID, p.cfg.answerID)
				p.answerStreamed = false
				p.streamed.Reset()
			}
		}
		if thinkPart != "" {
			p.thinkingOpen = true
			p.EmitThought(thinkPart, false)
		}
		p.EmitAnswer(p.refGate.Feed(p.sentinelGate.Feed(answerPart)))
	}
	if chunk.Done {
		thinkPart, answerPart := p.splitter.Flush()
		if thinkPart != "" {
			p.thinkingOpen = true
			p.EmitThought(thinkPart, false)
		}
		p.EmitAnswer(p.refGate.Feed(p.sentinelGate.Feed(answerPart)))
		if !p.sentinelGate.Held() {
			p.EmitAnswer(p.refGate.Flush())
		}
		p.CloseThinking()
	}
}

// Retract withdraws already-streamed answer text and clears local stream state.
func (p *streamAnswerPipeline) Retract() {
	if p.answerStreamed || p.streamed.Len() > 0 {
		p.engine.retractAnswer(p.ctx, p.cfg.sessionID, p.cfg.answerID)
		p.answerStreamed = false
		p.streamed.Reset()
	}
}

// ResetForRetry retracts any emitted draft and resets all streaming gates for a fresh attempt.
func (p *streamAnswerPipeline) ResetForRetry() {
	if p.streamed.Len() > 0 {
		p.engine.retractAnswer(p.ctx, p.cfg.sessionID, p.cfg.answerID)
	}
	p.streamed.Reset()
	p.answerStreamed = false
	p.providerDone = false
	p.terminalContent = ""
	p.splitter = agenttools.NewThinkStreamSplitter()
	p.sentinelGate.Reset()
	p.refGate.Reset()
}

// PublishOrReconcile reconciles the post-processed answer against what was
// already streamed during a ReAct round (streaming suffix or emitting Replace).
func (p *streamAnswerPipeline) PublishOrReconcile(fullContent string, holdAnswer bool) {
	p.cfg.holdAnswer = holdAnswer
	streamedText := p.streamed.String()
	if p.answerStreamed && fullContent != streamedText {
		if strings.HasPrefix(fullContent, streamedText) && !p.engine.isRuntimeFallbackAnswer(fullContent) {
			p.EmitAnswer(fullContent[len(streamedText):])
		} else {
			p.engine.eventBus.Emit(p.ctx, event.Event{
				ID: p.cfg.answerID, Type: event.EventAgentFinalAnswer, SessionID: p.cfg.sessionID,
				Data: event.AgentFinalAnswerData{
					Content:    fullContent,
					Replace:    true,
					IsFallback: p.engine.isRuntimeFallbackAnswer(fullContent),
				},
			})
		}
	} else if !p.answerStreamed {
		p.EmitAnswer(fullContent)
	}
}

// ReconcileFinal aligns the post-processed final synthesis text against any
// already-published stream chunks and returns (fullAnswer, terminalContent, tail).
func (p *streamAnswerPipeline) ReconcileFinal(public, fullAnswer string) (string, string, string) {
	_, tail := p.splitter.Flush()
	terminal := p.terminalContent
	if p.cfg.holdAnswer || p.sentinelGate.Held() || p.refGate.Held() {
		if p.streamed.Len() > 0 && strings.HasPrefix(public, p.streamed.String()) {
			return public, "", public[p.streamed.Len():]
		}
		if p.streamed.Len() > 0 {
			p.engine.retractAnswer(p.ctx, p.cfg.sessionID, p.cfg.answerID)
		}
		return public, "", public
	}
	if public != fullAnswer {
		if p.streamed.Len() > 0 {
			p.engine.retractAnswer(p.ctx, p.cfg.sessionID, p.cfg.answerID)
		}
		return public, "", public
	}
	return fullAnswer, terminal, tail
}

func (p *streamAnswerPipeline) Streamed() string     { return p.streamed.String() }
func (p *streamAnswerPipeline) StreamedLen() int     { return p.streamed.Len() }
func (p *streamAnswerPipeline) AnswerStreamed() bool { return p.answerStreamed }
func (p *streamAnswerPipeline) SentinelHeld() bool   { return p.sentinelGate.Held() }
func (p *streamAnswerPipeline) RefHeld() bool        { return p.refGate.Held() }
func (p *streamAnswerPipeline) ClearAnswerStreamed() { p.answerStreamed = false }
