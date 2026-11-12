package agent

import (
	"encoding/base64"
	"strings"
)

const retrievalCallPrefix = "retrieval-call/"

func retrievalTurnPrefix(turnID string) string {
	if turnID == "" {
		return ""
	}
	return retrievalCallPrefix + base64.RawURLEncoding.EncodeToString([]byte(turnID)) + "."
}

// Provider IDs are turn-local; never change the IDs replayed to the provider.
func retrievalCallReference(turnID, callID string) string {
	if turnID == "" || callID == "" {
		return callID
	}
	return retrievalTurnPrefix(turnID) + base64.RawURLEncoding.EncodeToString([]byte(callID))
}

func isRetrievalCallReference(ref string) bool {
	encoded, ok := strings.CutPrefix(ref, retrievalCallPrefix)
	if !ok {
		return false
	}
	turn, call, ok := strings.Cut(encoded, ".")
	if !ok || turn == "" || call == "" {
		return false
	}
	_, turnErr := base64.RawURLEncoding.DecodeString(turn)
	_, callErr := base64.RawURLEncoding.DecodeString(call)
	return turnErr == nil && callErr == nil
}

func (e *AgentEngine) retrievalCallReference(callID string) string {
	return retrievalCallReference(e.retrievalTurnID, callID)
}

func (e *AgentEngine) resolveRetrievalReference(ref string) string {
	if isRetrievalCallReference(ref) {
		return ref
	}
	return e.retrievalCallReference(ref)
}

func (e *AgentEngine) scopeRetrievalReview(review *retrievalReview) {
	review.CallID = e.resolveRetrievalReference(review.CallID)
	for i := range review.Evidence {
		review.Evidence[i].CallID = e.resolveRetrievalReference(review.Evidence[i].CallID)
	}
	if review.NewFact != nil && review.NewFact.Source != nil {
		review.NewFact.Source.CallID = e.resolveRetrievalReference(review.NewFact.Source.CallID)
	}
	for i := range review.UserAnswers {
		answer := &review.UserAnswers[i]
		if callID, ok := strings.CutPrefix(answer.InputID, "ask_user:"); ok {
			answer.InputID = "ask_user:" + e.resolveRetrievalReference(callID)
		}
	}
}
