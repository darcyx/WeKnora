package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

type retrievalFailure struct {
	CallID           string                                           `json:"call_id"`
	KnowledgeBaseIDs []string                                         `json:"knowledge_base_ids"`
	Outcome          retrievalOutcome                                 `json:"outcome"`
	Targets          types.SearchTargets                              `json:"targets,omitempty"`
	Stage            string                                           `json:"stage,omitempty"`
	Retry            func(context.Context) (*types.ToolResult, error) `json:"-"`
	RetryAvailable   bool                                             `json:"retry_available,omitempty"`
}

func outstandingRetrievalFailures(attempts []retrievalAttempt, gapID string) []retrievalFailure {
	var failures []retrievalFailure
	for _, attempt := range attempts {
		if attempt.GapID != gapID || !attempt.Completed {
			continue
		}
		if attempt.Action == retrievalRecovery && attempt.Outcome == retrievalSuccess && attempt.RecoveryCallID != "" {
			for i := range failures {
				failure := &failures[i]
				if failure.CallID != attempt.RecoveryCallID || len(failure.Targets) == 0 ||
					(!slices.Contains(attempt.RecoveryStages, failure.Stage) && !(len(attempt.RecoveryStages) == 0 && (failure.Stage == "search" || failure.Stage == "embedding"))) {
					continue
				}
				failure.Targets = slices.DeleteFunc(failure.Targets, func(target *types.SearchTarget) bool {
					return slices.ContainsFunc(attempt.RecoveryTargets, func(recovered *types.SearchTarget) bool {
						return reflect.DeepEqual(target, recovered)
					})
				})
				failure.KnowledgeBaseIDs = failure.Targets.GetAllKnowledgeBaseIDs()
			}
			failures = slices.DeleteFunc(failures, func(f retrievalFailure) bool { return len(f.KnowledgeBaseIDs) == 0 })
		}
		failures = append(failures, cloneRetrievalFailures(attempt.Failures)...)
	}
	return failures
}

func cloneRetrievalFailures(failures []retrievalFailure) []retrievalFailure {
	cloned := slices.Clone(failures)
	for i := range cloned {
		cloned[i].KnowledgeBaseIDs = slices.Clone(cloned[i].KnowledgeBaseIDs)
		cloned[i].Targets = cloned[i].Targets.Clone()
	}
	return cloned
}

func hasRecoverableSource(failures []retrievalFailure) bool {
	for _, failure := range failures {
		for _, id := range failure.KnowledgeBaseIDs {
			if recoverableSource(failures, id) {
				return true
			}
		}
	}
	return false
}

func recoverableSource(failures []retrievalFailure, id string) bool {
	return len(recoverableRetrievalTargets(failures, id)) > 0
}

func recoverableRetrievalTargets(failures []retrievalFailure, id string, restoring ...bool) types.SearchTargets {
	var targets types.SearchTargets
	canReplay := func(f retrievalFailure) bool {
		return f.Stage == "search" || f.Stage == "embedding" || f.Retry != nil || len(restoring) > 0 && restoring[0] && f.RetryAvailable
	}
	for _, failure := range failures {
		if failure.Outcome != retrievalTransient || !canReplay(failure) {
			continue
		}
		for _, target := range failure.Targets {
			if target == nil || target.KnowledgeBaseID != id {
				continue
			}
			blocked := slices.ContainsFunc(failures, func(other retrievalFailure) bool {
				if !slices.Contains(other.KnowledgeBaseIDs, id) ||
					(other.Outcome == retrievalTransient && canReplay(other)) {
					return false
				}
				return len(other.Targets) == 0 || retrievalScopesOverlap(
					[]string{retrievalScopeForTargets(other.Targets)}, retrievalScopeForTargets(types.SearchTargets{target}))
			})
			if !blocked && !slices.ContainsFunc(targets, func(existing *types.SearchTarget) bool { return reflect.DeepEqual(existing, target) }) {
				targets = append(targets, target)
			}
		}
	}
	return targets
}

func retrievalArgumentScope(arguments string) (map[string]any, []string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return nil, nil, fmt.Errorf("invalid recorded retrieval arguments")
	}
	var ids []string
	if value, present := args["knowledge_base_ids"]; present {
		encoded, _ := json.Marshal(value)
		// Empty or null selects every base, exactly like an absent key, so the
		// callers' len(ids) == 0 handling decides what that means for a recovery.
		if err := json.Unmarshal(encoded, &ids); err != nil {
			return nil, nil, fmt.Errorf("invalid recovery knowledge_base_ids")
		}
		seen := make(map[string]bool)
		for _, id := range ids {
			if id == "" || seen[id] {
				return nil, nil, fmt.Errorf("invalid recovery knowledge_base_ids")
			}
			seen[id] = true
		}
	}
	delete(args, "knowledge_base_ids")
	return args, ids, nil
}

func validateScopedRecovery(previous *retrievalAttempt, arguments string, restoring ...bool) error {
	before, _, err := retrievalArgumentScope(previous.Arguments)
	if err != nil {
		return err
	}
	after, ids, err := retrievalArgumentScope(arguments)
	if err != nil || !reflect.DeepEqual(before, after) {
		return fmt.Errorf("source recovery must preserve the original query, mode and other arguments")
	}
	if len(ids) == 0 {
		if previous.Outcome == retrievalSuccess || previous.Arguments != arguments {
			return fmt.Errorf("partial-result recovery must explicitly select failed knowledge_base_ids")
		}
		for _, failure := range previous.Failures {
			ids = append(ids, failure.KnowledgeBaseIDs...)
		}
	}
	for _, id := range ids {
		if len(recoverableRetrievalTargets(previous.Failures, id, restoring...)) == 0 {
			return fmt.Errorf("knowledge base %s has no recoverable failure in this observation", id)
		}
	}
	return nil
}

func validateRetrievalFailureTargets(attempt *retrievalAttempt, failure retrievalFailure) error {
	if len(failure.Targets) == 0 {
		return nil // Legacy observations remain readable, but cannot grant a precise recovery.
	}
	if strings.TrimSpace(failure.Stage) == "" ||
		!slices.Equal(sortedRetrievalIDs(failure.KnowledgeBaseIDs), sortedRetrievalIDs(failure.Targets.GetAllKnowledgeBaseIDs())) {
		return fmt.Errorf("retrieval failure has inconsistent target or stage identity")
	}
	for _, target := range failure.Targets {
		if target == nil || strings.TrimSpace(target.KnowledgeBaseID) == "" ||
			(target.Type != types.SearchTargetTypeKnowledgeBase && target.Type != types.SearchTargetTypeKnowledge) ||
			(target.Type == types.SearchTargetTypeKnowledgeBase && len(target.KnowledgeIDs) > 0) ||
			(target.Type == types.SearchTargetTypeKnowledge && len(target.KnowledgeIDs) == 0 && len(target.TagIDs) == 0) {
			return fmt.Errorf("invalid retrieval failure target")
		}
		for _, ids := range [][]string{target.KnowledgeIDs, target.TagIDs, target.ScopeTagIDs} {
			if len(ids) != len(sortedRetrievalIDs(ids)) {
				return fmt.Errorf("invalid retrieval failure filter")
			}
		}
	}
	if !retrievalScopeCovers(attempt.Scope, retrievalScopeForTargets(failure.Targets)) {
		return fmt.Errorf("retrieval failure target exceeds the observed operation")
	}
	return nil
}

func scopedRecoveryTargets(previous *retrievalAttempt, arguments string, restoring ...bool) (types.SearchTargets, error) {
	_, ids, err := retrievalArgumentScope(arguments)
	if err != nil {
		return nil, err
	}
	stageRecovery := false
	for _, failure := range previous.Failures {
		stageRecovery = stageRecovery || failure.Retry != nil
	}
	if len(restoring) > 0 && restoring[0] {
		for _, f := range previous.Failures {
			stageRecovery = stageRecovery || f.RetryAvailable
		}
	}
	if previous.Tool != "search_knowledge" && !stageRecovery {
		return nil, fmt.Errorf("this tool cannot dispatch the recorded failed targets or stage")
	}
	if len(ids) == 0 {
		for _, failure := range previous.Failures {
			ids = append(ids, failure.KnowledgeBaseIDs...)
		}
		ids = sortedRetrievalIDs(ids)
	}
	for _, failure := range previous.Failures {
		if !slices.ContainsFunc(failure.KnowledgeBaseIDs, func(id string) bool { return slices.Contains(ids, id) }) {
			continue
		}
		if err := validateRetrievalFailureTargets(previous, failure); err != nil {
			return nil, err
		}
	}
	var targets types.SearchTargets
	for _, id := range ids {
		recoverable := recoverableRetrievalTargets(previous.Failures, id, restoring...)
		if len(recoverable) == 0 {
			return nil, fmt.Errorf("the failed targets or stage cannot be safely dispatched for recovery")
		}
		targets = append(targets, recoverable.Clone()...)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no verified failed target remains for recovery")
	}
	for _, failure := range previous.Failures {
		if failure.Retry != nil && retrievalScopesOverlap([]string{retrievalScopeForTargets(failure.Targets)}, retrievalScopeForTargets(targets)) &&
			!retrievalScopeCovers(retrievalScopeForTargets(targets), retrievalScopeForTargets(failure.Targets)) {
			return nil, fmt.Errorf("an isolated stage must recover its complete original failed range")
		}
	}
	return targets, nil
}

func (c *retrievalControl) dispatchTargets(callID string) types.SearchTargets {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if attempt := c.attempt(callID); attempt != nil && attempt.Action == retrievalRecovery && !attempt.Completed {
		return attempt.RecoveryTargets.Clone()
	}
	return nil
}

func (c *retrievalControl) recoverySpent(original *retrievalAttempt, before string) bool {
	if original.Action == retrievalRecovery {
		return true
	}
	for _, attempt := range c.state.Attempts {
		if attempt.ID == before {
			break
		}
		if attempt.RecoveryCallID == original.ID {
			return true
		}
	}
	return false
}

func (c *retrievalControl) validateRecoveryReservation(attempt *retrievalAttempt) error {
	if attempt.Action != retrievalRecovery {
		if attempt.RecoveryCallID != "" || len(attempt.RecoveryTargets) > 0 {
			return fmt.Errorf("only a recovery can reserve failed targets")
		}
		return nil
	}
	if attempt.RecoveryCallID == "" {
		if len(attempt.RecoveryTargets) > 0 {
			return fmt.Errorf("recovery targets require an original call")
		}
		return nil // Historical attempts cannot be replayed or clear unassociated failures.
	}
	var previous *retrievalAttempt
	for i := range c.state.Attempts {
		candidate := &c.state.Attempts[i]
		if candidate.ID == attempt.ID {
			break
		}
		if candidate.ID == attempt.RecoveryCallID {
			previous = candidate
		}
	}
	if previous == nil || !previous.Completed || previous.GapID != attempt.GapID || previous.Tool != attempt.Tool {
		return fmt.Errorf("recovery must refer to an earlier observed operation of this gap")
	}
	if attempt.RuntimeManaged && c.recoverySpent(previous, attempt.ID) {
		return fmt.Errorf("recovery already used for this operation")
	}
	for _, stage := range attempt.RecoveryStages {
		if !slices.ContainsFunc(previous.Failures, func(f retrievalFailure) bool {
			return f.Stage == stage && f.Outcome == retrievalTransient && (stage == "search" || stage == "embedding" || f.RetryAvailable)
		}) {
			return fmt.Errorf("recovery stage has no matching failed operation")
		}
	}
	var expected types.SearchTargets
	if len(previous.Failures) > 0 {
		if err := validateScopedRecovery(previous, attempt.Arguments, true); err != nil {
			return err
		}
		var err error
		expected, err = scopedRecoveryTargets(previous, attempt.Arguments, true)
		if err != nil {
			return err
		}
	} else if (previous.Outcome != retrievalTransient && previous.Outcome != retrievalInput) ||
		(previous.Outcome == retrievalTransient && previous.Arguments != attempt.Arguments) ||
		(previous.Outcome == retrievalInput && previous.Arguments == attempt.Arguments) {
		return fmt.Errorf("recovery does not match the original failed operation")
	}
	if !reflect.DeepEqual(expected, attempt.RecoveryTargets) ||
		(len(expected) > 0 && attempt.Scope != retrievalScopeForTargets(expected)) {
		return fmt.Errorf("recovery reservation does not match its failed targets")
	}
	return nil
}

func independentRetrievalScope(failures []retrievalFailure, arguments string) bool {
	_, ids, err := retrievalArgumentScope(arguments)
	if err != nil || len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		for _, failure := range failures {
			if slices.Contains(failure.KnowledgeBaseIDs, id) {
				return false
			}
		}
	}
	return true
}
