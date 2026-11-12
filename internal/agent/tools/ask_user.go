package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/approval"
	"github.com/Tencent/WeKnora/internal/types"
)

const askUserDescription = `Ask the user for missing context, then end this turn immediately. The user answers in their next message.

Use it only when you cannot proceed without information or a decision that only the user can provide, such as:
- choosing between options that lead to different work;
- details that are missing and cannot be found with other tools;
- confirming a consequential choice.

Do not ask for information already supplied in the conversation or project context.
When the relevant branches or required fields must first be discovered in documents, read those documents before asking.
Ask only what changes the answer or next step; do not guess which branch applies.
Complete any independent parts of the request before calling this tool. Do not place dependent tool calls after it.
Ask one focused question, grouping necessary materials for the same task.

Shapes of a question:
- free text: only "question";
- choices: "options": ["first choice", "second choice"] — a JSON array, never an array encoded inside a string
  (set "multi_select" for several, "allow_free_text" to also accept a typed answer);
- a short form: "fields" with types text, number, select or multiselect.

Options and field labels are shown to the user as written, and the chosen text comes back as the answer. Write
them in the user's language as a person would say them ("No authentication", "API key"), never as internal ids
("api_key"), and offer only choices the next step accepts. Leave out a field you can fill yourself.

This tool returns the question for publication without waiting, opening an interactive card, or creating a suspended run.
Do not call another tool or generate a second version of the question after it. Continue when the user sends their next message.`

var askUserSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "The question shown to the user", "maxLength": %[1]d},
    "options": {
      "type": "array", "maxItems": %[2]d,
      "items": {"type": "string", "maxLength": %[3]d},
      "description": "Choices the user picks from, worded for the user"
    },
    "multi_select": {"type": "boolean", "description": "Allow choosing several options"},
    "allow_free_text": {"type": "boolean", "description": "Also accept a typed answer besides the options"},
    "fields": {
      "type": "array", "maxItems": %[4]d,
      "description": "A short form; use instead of options",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "maxLength": %[5]d, "description": "Key of the value in the answer"},
          "label": {"type": "string", "maxLength": %[3]d},
          "type": {"type": "string", "enum": ["text", "number", "select", "multiselect"]},
          "required": {"type": "boolean"},
          "options": {"type": "array", "maxItems": %[2]d, "items": {"type": "string", "maxLength": %[3]d}}
        },
        "required": ["name", "type"]
      }
    }
  },
  "required": ["question"]
}`, types.AskUserMaxQuestionLen, types.AskUserMaxOptions, types.AskUserMaxOptionLen,
	types.AskUserMaxFields, types.AskUserMaxNameLen))

// AskUserTool requests missing context and ends the turn without waiting.
type AskUserTool struct {
	BaseTool
	asker approval.UserAsker
}

// NewAskUserTool builds the ask_user tool.
func NewAskUserTool(asker approval.UserAsker) *AskUserTool {
	return &AskUserTool{BaseTool: NewBaseTool(ToolAskUser, askUserDescription, askUserSchema), asker: asker}
}

// Execute implements types.Tool.
func (t *AskUserTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var question types.AskUserPromptRequest
	if err := json.Unmarshal(args, &question); err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("Failed to parse args: %v", err)}, nil
	}
	if err := question.Validate(); err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("Invalid question: %v", err)}, nil
	}
	meta, _ := ToolExecFromContext(ctx)
	resumed := meta != nil && meta.Answered != nil && meta.Answered.Kind == types.AgentPromptKindAskUser
	if !resumed {
		return AskUserQuestion(question), nil
	}
	// Consume answers from runs suspended before ask_user became non-blocking.
	if t.asker == nil {
		return &types.ToolResult{Success: false, Error: "Cannot consume the stored user answer: no answer handler"}, nil
	}
	answer := t.asker.ResumeAnswer(ctx, meta.Answered, meta.EventBus)
	if answer.Suspended != nil {
		return &types.ToolResult{Success: false, Error: "waiting for the user to answer", Suspend: answer.Suspended}, nil
	}
	data := map[string]interface{}{
		"display_type": "ask_user",
		"question":     question,
		"answered":     answer.Answered,
	}
	if !answer.Answered {
		reason := answer.Reason
		if reason == "" {
			reason = "no answer"
		}
		data["reason"] = reason
		data["timed_out"] = answer.TimedOut
		data["canceled"] = answer.Canceled
		return &types.ToolResult{
			Success: true,
			Output: fmt.Sprintf("The user did not answer (%s). Continue without this information, "+
				"or tell the user what you need.", reason),
			Data: data,
		}, nil
	}
	data["answer"] = answer.Response
	return &types.ToolResult{Success: true, Output: formatAskUserAnswer(question, answer.Response), Data: data}, nil
}

// AskUserQuestion validates and renders a non-blocking clarification.
func AskUserQuestion(question types.AskUserPromptRequest) *types.ToolResult {
	if err := question.Validate(); err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("Invalid question: %v", err)}
	}
	var text strings.Builder
	text.WriteString(question.Question)
	for _, option := range question.Options {
		fmt.Fprintf(&text, "\n- %s", option)
	}
	for _, field := range question.Fields {
		label := field.Label
		if label == "" {
			label = field.Name
		}
		fmt.Fprintf(&text, "\n- %s", label)
		if len(field.Options) > 0 {
			fmt.Fprintf(&text, ": %s", strings.Join(field.Options, " / "))
		}
	}
	return &types.ToolResult{
		Success: true, Output: text.String(), UserQuestion: &question,
		Data: map[string]interface{}{"display_type": "user_question", "question": question, "answered": false},
	}
}

func formatAskUserAnswer(q types.AskUserPromptRequest, a types.AskUserPromptResponse) string {
	var b strings.Builder
	b.WriteString("The user answered.\n")
	if len(a.Selected) > 0 {
		fmt.Fprintf(&b, "Selected: %s\n", strings.Join(a.Selected, ", "))
	}
	if len(a.Values) > 0 {
		b.WriteString("Fields:\n")
		order := make([]string, 0, len(a.Values))
		for _, f := range q.Fields {
			if _, ok := a.Values[f.Name]; ok {
				order = append(order, f.Name)
			}
		}
		for _, name := range order {
			v, _ := json.Marshal(a.Values[name])
			fmt.Fprintf(&b, "- %s: %s\n", name, v)
		}
	}
	if text := strings.TrimSpace(a.Text); text != "" {
		fmt.Fprintf(&b, "Text: %s\n", text)
	}
	return strings.TrimRight(b.String(), "\n")
}
