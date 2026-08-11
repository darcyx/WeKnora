package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	tokenQuotaRequestOverheadTokens int64 = 64
	tokenQuotaImageReserveTokens    int64 = 4096
)

// quotaChat is the application-layer model boundary for interactive API
// callers. Provider adapters remain unaware of accounting, while every
// normal-chat and Agent call obtained through ModelService is covered.
type quotaChat struct {
	upstream chat.Chat
	quota    interfaces.TokenQuotaService
}

func newQuotaChat(model chat.Chat, quota interfaces.TokenQuotaService) chat.Chat {
	if model == nil || quota == nil {
		return model
	}
	return &quotaChat{upstream: model, quota: quota}
}

func (c *quotaChat) Chat(
	ctx context.Context,
	messages []chat.Message,
	opts *chat.ChatOptions,
) (*types.ChatResponse, error) {
	reservation, boundedOpts, err := c.reserve(ctx, messages, opts)
	if err != nil {
		return nil, err
	}
	if reservation == nil {
		return c.upstream.Chat(ctx, messages, boundedOpts)
	}
	response, err := c.upstream.Chat(ctx, messages, boundedOpts)
	if err != nil {
		_ = c.settleReserved(ctx, reservation)
		return nil, err
	}
	if response == nil || !hasReportedTokenUsage(&response.Usage) {
		_ = c.settleReserved(ctx, reservation)
		return response, nil
	}
	if err := c.settle(ctx, reservation.ID, &response.Usage); err != nil {
		// The upstream call already succeeded and the provider already billed
		// it, so a settlement failure must not discard the answer. The
		// reservation stays pending and keeps holding its conservative upper
		// bound, which is the safe direction: the caller is over-charged until
		// the period rolls over rather than escaping the quota. This mirrors
		// how ChatStream treats an ambiguous settlement.
		logger.Errorf(ctx, "token quota settlement failed for reservation %s: %v", reservation.ID, err)
	}
	return response, nil
}

func (c *quotaChat) ChatStream(
	ctx context.Context,
	messages []chat.Message,
	opts *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	reservation, boundedOpts, err := c.reserve(ctx, messages, opts)
	if err != nil {
		return nil, err
	}
	stream, err := c.upstream.ChatStream(ctx, messages, boundedOpts)
	if err != nil {
		if reservation != nil {
			_ = c.settleReserved(ctx, reservation)
		}
		return nil, err
	}
	if reservation == nil {
		return stream, nil
	}

	output := make(chan types.StreamResponse)
	go func() {
		defer close(output)
		settled := false
		defer func() {
			if !settled {
				// The provider may have accepted the request even when a caller
				// disconnects or it omits final usage. Charge the durable upper
				// bound so the strict quota cannot be bypassed through an
				// ambiguous outcome.
				_ = c.settleReserved(ctx, reservation)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case response, ok := <-stream:
				if !ok {
					return
				}
				if response.Done && hasReportedTokenUsage(response.Usage) && !settled {
					if err := c.settle(ctx, reservation.ID, response.Usage); err == nil {
						settled = true
					} else {
						// A failed settlement may be an ambiguous database outcome.
						// The deferred conservative settlement keeps the quota held
						// if reporting actual usage cannot be persisted.
					}
				}
				select {
				case output <- response:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return output, nil
}

func (c *quotaChat) GetModelName() string { return c.upstream.GetModelName() }

func (c *quotaChat) GetModelID() string { return c.upstream.GetModelID() }

func (c *quotaChat) reserve(
	ctx context.Context,
	messages []chat.Message,
	opts *chat.ChatOptions,
) (*types.TokenQuotaReservation, *chat.ChatOptions, error) {
	subjectID, ok := types.TokenQuotaSubjectFromContext(ctx)
	if !ok {
		return nil, opts, nil
	}
	promptTokens := estimateQuotaPromptTokens(messages, opts)
	completionTokens := requestedCompletionTokens(opts)
	reservation, err := c.quota.Reserve(ctx, subjectID, promptTokens, completionTokens)
	if err != nil || reservation == nil {
		return reservation, opts, err
	}
	return reservation, withQuotaCompletionLimit(opts, reservation.ReservedTokens-promptTokens), nil
}

func (c *quotaChat) settle(ctx context.Context, reservationID string, usage *types.TokenUsage) error {
	settlementCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return c.quota.Settle(settlementCtx, reservationID, usage)
}

func (c *quotaChat) settleReserved(ctx context.Context, reservation *types.TokenQuotaReservation) error {
	if reservation == nil {
		return nil
	}
	return c.settle(ctx, reservation.ID, &types.TokenUsage{TotalTokens: int(reservation.ReservedTokens)})
}

func (c *quotaChat) release(ctx context.Context, reservationID string) error {
	settlementCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return c.quota.Release(settlementCtx, reservationID)
}

func requestedCompletionTokens(opts *chat.ChatOptions) int64 {
	if opts == nil {
		return 0
	}
	if opts.MaxCompletionTokens > 0 {
		return int64(opts.MaxCompletionTokens)
	}
	if opts.MaxTokens > 0 {
		return int64(opts.MaxTokens)
	}
	return 0
}

func withQuotaCompletionLimit(opts *chat.ChatOptions, maxTokens int64) *chat.ChatOptions {
	if maxTokens <= 0 {
		maxTokens = 1
	}
	bounded := chat.ChatOptions{}
	if opts != nil {
		bounded = *opts
	}
	// MaxTokens is honoured by Ollama and Anthropic. OpenAI reasoning adapters
	// convert it to max_completion_tokens, so one cap safely reaches all
	// existing provider paths without emitting mutually-exclusive fields.
	bounded.MaxTokens = int(maxTokens)
	bounded.MaxCompletionTokens = 0
	return &bounded
}

func estimateQuotaPromptTokens(messages []chat.Message, opts *chat.ChatOptions) int64 {
	payload, err := json.Marshal(struct {
		Messages []chat.Message    `json:"messages"`
		Options  *chat.ChatOptions `json:"options,omitempty"`
	}{Messages: messages, Options: opts})
	if err != nil {
		return tokenQuotaRequestOverheadTokens
	}
	imageCount := 0
	for _, message := range messages {
		imageCount += len(message.Images)
		for _, part := range message.MultiContent {
			if part.ImageURL != nil {
				imageCount++
			}
		}
	}
	// UTF-8 bytes are a conservative upper bound for text-token encodings;
	// image URLs are not representative of visual token cost, so reserve a
	// separate fixed safety budget per image.
	return int64(len(payload)) + tokenQuotaRequestOverheadTokens + int64(imageCount)*tokenQuotaImageReserveTokens
}

func hasReportedTokenUsage(usage *types.TokenUsage) bool {
	return usage != nil && (usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0)
}
