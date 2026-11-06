package types

// FAQFeedback stores session-scoped user votes for directly displayed FAQ entries.
// The composite primary key enforces one-shot submission under concurrency.
type FAQFeedback struct {
	SessionID string `gorm:"primaryKey"`
	TenantID  uint64 `gorm:"primaryKey;autoIncrement:false"`
	UserID    string `gorm:"not null"`
	EntryID   int64  `gorm:"primaryKey;autoIncrement:false"`
	// FAQ content is copied into these columns at feedback submission time.
	ChunkID           string
	KnowledgeID       string
	KnowledgeBaseID   string
	TagID             int64
	TagName           string
	StandardQuestion  string
	SimilarQuestions  []string `gorm:"serializer:json;type:jsonb"`
	NegativeQuestions []string `gorm:"serializer:json;type:jsonb"`
	Answers           []string `gorm:"serializer:json;type:jsonb"`
	AnswerStrategy    AnswerStrategy
	Feedback          *MessageFeedback `gorm:"type:jsonb;not null"`
}

func (FAQFeedback) TableName() string { return "faq_feedbacks" }
