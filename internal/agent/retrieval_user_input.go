package agent

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
)

const retrievalUserConditions retrievalStatus = "user_conditions"

type retrievalUserAnswer struct {
	Field   string `json:"field"`
	InputID string `json:"input_id"`
	Quote   string `json:"quote"`
}

// RetrievalMissingUserFields shares the answer boundary with post-answer consumers.
func RetrievalMissingUserFields(raw json.RawMessage) []string {
	var state retrievalState
	if json.Unmarshal(raw, &state) != nil {
		return nil
	}
	return (&retrievalControl{state: state}).missingUserFields()
}

func (c *retrievalControl) missingUserFields() []string {
	var fields []string
	for _, g := range c.state.Gaps {
		if g.Status != retrievalNeedsUser || !retrievalScopeCovers(c.state.Scope, g.Scope) || !c.factNeeded(&g, make(map[string]bool)) {
			continue
		}
		for _, field := range g.MissingUserFields {
			if !slices.Contains(fields, field) {
				fields = append(fields, field)
			}
		}
	}
	return fields
}

// SetRetrievalUserMessage binds the stored user message before Execute/Resume.
// Composed queries also contain runtime and attachment text, which is not an answer.
func (e *AgentEngine) SetRetrievalUserMessage(id, text string) {
	e.retrievalUserMessage.id, e.retrievalUserMessage.text = id, text
}

func (e *AgentEngine) recordRetrievalUserMessage() {
	if e.retrieval != nil && strings.TrimSpace(e.retrievalUserMessage.id) != "" {
		e.retrieval.recordUserInput("message:"+e.retrievalUserMessage.id, e.retrievalUserMessage.text)
	}
	if e.retrieval != nil && e.appInfo != "" {
		e.retrieval.recordUserInput(e.projectInputID(), e.appInfo)
	}
}

func (c *retrievalControl) recordUserInput(id, text string) {
	if !validRetrievalInputID(id) || strings.TrimSpace(text) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.userInputs == nil {
		c.userInputs = make(map[string]string)
	}
	if previous, exists := c.userInputs[id]; exists && previous != text {
		// An identity conflict cannot authorize a condition update.
		c.userInputs[id] = ""
		return
	}
	c.userInputs[id] = text
	if !slices.Contains(c.state.UserInputIDs, id) {
		c.state.UserInputIDs = append(c.state.UserInputIDs, id)
		_ = c.persistLocked()
	}
}

func validRetrievalInputID(id string) bool {
	if value, ok := strings.CutPrefix(id, "app_info:"); ok {
		decoded, err := hex.DecodeString(value)
		return err == nil && len(decoded) == 32
	}
	for _, prefix := range []string{"message:", "ask_user:"} {
		if strings.HasPrefix(id, prefix) && strings.TrimSpace(strings.TrimPrefix(id, prefix)) != "" {
			return true
		}
	}
	return false
}

func (c *retrievalControl) availableUserInputs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]string, 0, len(c.userInputs))
	for _, id := range c.state.UserInputIDs {
		if c.userInputs[id] != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (c *retrievalControl) resolveUserConditions(review retrievalReview) error {
	g := c.gap(review.GapID)
	if g == nil || g.Status != retrievalNeedsUser || len(g.MissingUserFields) == 0 || len(review.UserAnswers) == 0 ||
		review.NewFact != nil || review.CallID != "" || len(review.Quotes) > 0 || len(review.MissingUserFields) > 0 {
		return fmt.Errorf("user_conditions requires an existing needs_user fact and actual answers to its missing fields")
	}
	for _, answer := range review.UserAnswers {
		answer.Field, answer.Quote = strings.TrimSpace(answer.Field), strings.TrimSpace(answer.Quote)
		index := slices.IndexFunc(g.MissingUserFields, func(field string) bool { return strings.EqualFold(field, answer.Field) })
		input := c.userInputs[answer.InputID]
		if index < 0 || input == "" || answer.Quote == "" || !strings.Contains(input, answer.Quote) {
			return fmt.Errorf("each answer must resolve a distinct missing field using a quote from an available user input")
		}
		answer.Field = g.MissingUserFields[index]
		g.UserAnswers = append(g.UserAnswers, answer)
		g.MissingUserFields = slices.Delete(g.MissingUserFields, index, index+1)
	}
	if len(g.MissingUserFields) == 0 {
		// Reassess the existing observation; answering never refunds a search.
		g.Status, g.LastReview = retrievalPending, ""
	}
	return nil
}

func (c *retrievalControl) validateUserInputs() error {
	seen := make(map[string]bool)
	for _, id := range c.state.UserInputIDs {
		if !validRetrievalInputID(id) || seen[id] {
			return fmt.Errorf("invalid saved user input identity")
		}
		seen[id] = true
	}
	return nil
}

func (c *retrievalControl) validateUserAnswers(g *retrievalGap) error {
	fields := slices.Clone(g.MissingUserFields)
	for _, answer := range g.UserAnswers {
		if strings.TrimSpace(answer.Field) == "" || strings.TrimSpace(answer.Quote) == "" ||
			!slices.Contains(c.state.UserInputIDs, answer.InputID) ||
			slices.ContainsFunc(fields, func(field string) bool { return strings.EqualFold(field, answer.Field) }) {
			return fmt.Errorf("invalid saved user condition")
		}
		fields = append(fields, answer.Field)
	}
	return nil
}

// Call with the controller locked. Saved answers retain only the supplied
// value and provenance, not another copy of the entire conversation.
func (c *retrievalControl) hasUserQuote(quote string) bool {
	if strings.Contains(c.state.Goal, quote) {
		return true
	}
	for _, input := range c.userInputs {
		if strings.Contains(input, quote) {
			return true
		}
	}
	for _, gap := range c.state.Gaps {
		for _, answer := range gap.UserAnswers {
			if strings.Contains(answer.Quote, quote) {
				return true
			}
		}
	}
	return false
}

func (e *AgentEngine) recordRetrievalUserAnswer(call types.ToolCall) {
	if e.retrieval == nil || call.Name != agenttools.ToolAskUser || call.ID == "" ||
		call.Result == nil || !call.Result.Success || call.Result.Suspend != nil {
		return
	}
	answered, _ := call.Result.Data["answered"].(bool)
	if !answered {
		return
	}
	encoded, err := json.Marshal(call.Result.Data["answer"])
	if err != nil {
		return
	}
	var answer types.AskUserPromptResponse
	if json.Unmarshal(encoded, &answer) != nil || answer.Declined {
		return
	}
	parts := append([]string{answer.Text}, answer.Selected...)
	keys := make([]string, 0, len(answer.Values))
	for key := range answer.Values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := answer.Values[key]
		if text, ok := value.(string); ok {
			parts = append(parts, text)
		} else if value != nil {
			data, err := json.Marshal(value)
			if err == nil {
				parts = append(parts, string(data))
			}
		}
	}
	e.retrieval.recordUserInput("ask_user:"+e.retrievalCallReference(call.ID), strings.Join(parts, "\n"))
}
