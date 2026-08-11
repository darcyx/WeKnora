package types

import (
	"database/sql/driver"
	"encoding/json"
	"time"
)

// MessageFeedbackType is a like/dislike vote an end user casts on an
// assistant answer. The two vote types are mutually exclusive per message.
type MessageFeedbackType string

const (
	MessageFeedbackLike    MessageFeedbackType = "like"
	MessageFeedbackDislike MessageFeedbackType = "dislike"
)

// Dislike reason codes, matching the fixed option set rendered by the
// feedback dialog. MessageFeedbackReasonOther is itself a selectable reason;
// selecting it is what unlocks the free-text explanation field.
const (
	MessageFeedbackReasonInaccurate = "inaccurate" // 回答不准确
	MessageFeedbackReasonIncomplete = "incomplete" // 回答不完整
	MessageFeedbackReasonOffTopic   = "off_topic"  // 答非所问
	MessageFeedbackReasonOther      = "other"      // 其他
)

// ValidMessageFeedbackReasons is the fixed set of reason codes the client is
// allowed to send with a dislike vote.
var ValidMessageFeedbackReasons = map[string]struct{}{
	MessageFeedbackReasonInaccurate: {},
	MessageFeedbackReasonIncomplete: {},
	MessageFeedbackReasonOffTopic:   {},
	MessageFeedbackReasonOther:      {},
}

// MessageFeedbackReasonTextMaxRunes bounds the free-text explanation attached
// to the "other" reason, so a dislike vote can't smuggle in unbounded text.
const MessageFeedbackReasonTextMaxRunes = 500

// MessageFeedback records a user's like/dislike vote on an assistant answer.
// It is one-shot: written once by SubmitMessageFeedback and never edited
// afterwards, so CreatedAt also serves as "when the user voted".
type MessageFeedback struct {
	Type MessageFeedbackType `json:"type"`
	// Reasons is only populated for a dislike vote; multiple reasons may be
	// selected at once, "other" being one of them.
	Reasons []string `json:"reasons,omitempty"`
	// ReasonText is the free-text explanation, required only when Reasons
	// includes "other".
	ReasonText string    `json:"reason_text,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Value persists the vote as a jsonb column; nil writes SQL NULL, matching
// the nullable-pointer pattern TokenUsage uses.
func (f *MessageFeedback) Value() (driver.Value, error) {
	if f == nil {
		return nil, nil
	}
	return json.Marshal(f)
}

// Scan restores a jsonb feedback column; NULL leaves the receiver untouched.
func (f *MessageFeedback) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return nil
	}
	return json.Unmarshal(b, f)
}
