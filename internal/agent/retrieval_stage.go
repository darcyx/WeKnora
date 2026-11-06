package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

// shortcut: isolated stage callbacks are live-run only; cross-process replay requires saved candidates.
func (c *retrievalControl) dispatchRecovery(callID string) func(context.Context) (*types.ToolResult, error) {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempt(callID)
	if a == nil || a.Action != retrievalRecovery || a.Completed {
		return nil
	}
	previous := c.attempt(a.RecoveryCallID)
	if previous == nil {
		return nil
	}
	var retries []retrievalFailure
	for _, failure := range previous.Failures {
		if failure.Retry != nil && failure.Outcome == retrievalTransient && retrievalScopeCovers(retrievalScopeForTargets(a.RecoveryTargets), retrievalScopeForTargets(failure.Targets)) {
			retries = append(retries, failure)
		}
	}
	if len(retries) == 0 {
		return nil
	}
	return func(ctx context.Context) (*types.ToolResult, error) {
		var combined *types.ToolResult
		var causes []error
		for _, retry := range retries {
			if ctx.Err() != nil {
				return combined, ctx.Err()
			}
			result, err := retry.Retry(ctx)
			if result == nil {
				if err == nil {
					err = fmt.Errorf("isolated retrieval stage returned no observation")
				}
				result = &types.ToolResult{Output: err.Error(), Error: err.Error()}
			}
			if err != nil {
				causes = append(causes, err)
				result.RetrievalFailures = append(result.RetrievalFailures, types.RetrievalFailure{KnowledgeBaseIDs: retry.KnowledgeBaseIDs, Targets: retry.Targets.Clone(), Stage: retry.Stage, Cause: err})
			}
			if combined == nil {
				combined = result
			} else {
				combined = mergeFAQSearchResult(combined, result)
			}
		}
		if combined != nil && combined.Success {
			return combined, nil
		}
		return combined, errors.Join(causes...)
	}
}
