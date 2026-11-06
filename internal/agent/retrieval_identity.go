package agent

import (
	"encoding/base64"
	"strings"
)

const retrievalCallPrefix = "retrieval-call/"

// Provider IDs are turn-local; never change the IDs replayed to the provider.
func retrievalCallReference(turnID, callID string) string {
	if turnID == "" || callID == "" {
		return callID
	}
	return retrievalCallPrefix + base64.RawURLEncoding.EncodeToString([]byte(turnID)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(callID))
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

// Only a suspended run has a known owner for legacy, unscoped references.
func (e *AgentEngine) scopeLegacyRetrievalSnapshot(state *retrievalState) {
	for i := range state.Attempts {
		a := &state.Attempts[i]
		a.ID = e.resolveRetrievalReference(a.ID)
		a.RecoveryCallID = e.resolveRetrievalReference(a.RecoveryCallID)
		for j := range a.Failures {
			a.Failures[j].CallID = e.resolveRetrievalReference(a.Failures[j].CallID)
		}
	}
	for i := range state.Gaps {
		g := &state.Gaps[i]
		g.LastCallID = e.resolveRetrievalReference(g.LastCallID)
		g.EvidenceCallID = e.resolveRetrievalReference(g.EvidenceCallID)
		for j := range g.EvidenceSources {
			g.EvidenceSources[j].CallID = e.resolveRetrievalReference(g.EvidenceSources[j].CallID)
		}
		for j := range g.Origins {
			g.Origins[j].CallID = e.resolveRetrievalReference(g.Origins[j].CallID)
		}
		review := retrievalReview{UserAnswers: g.UserAnswers}
		e.scopeRetrievalReview(&review)
	}
	for i, id := range state.UserInputIDs {
		if callID, ok := strings.CutPrefix(id, "ask_user:"); ok {
			state.UserInputIDs[i] = "ask_user:" + e.resolveRetrievalReference(callID)
		}
	}
}
