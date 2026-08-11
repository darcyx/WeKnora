package types

import "time"

// FeedbackQuery describes a tenant-scoped, read-only administrative search.
type FeedbackQuery struct {
	Source          string
	Vote            string
	Reasons         []string
	Search          string
	SessionID       string
	TargetID        string
	UserID          string
	KnowledgeBaseID string
	TagName         string
	From            time.Time
	To              time.Time
	Page            int
	PageSize        int
	Sort            string
}

type FeedbackRecord struct {
	Source            string           `json:"source"`
	SessionID         string           `json:"session_id"`
	TargetID          string           `json:"target_id"`
	MessageID         string           `json:"message_id"`
	UserID            string           `json:"user_id"`
	UserLabel         string           `json:"user_label"`
	SessionTitle      string           `json:"session_title"`
	Question          string           `json:"question"`
	Answers           []string         `json:"answers" gorm:"serializer:json"`
	KnowledgeBaseID   string           `json:"knowledge_base_id"`
	KnowledgeBaseName string           `json:"knowledge_base_name"`
	TagName           string           `json:"tag_name"`
	SimilarQuestions  []string         `json:"similar_questions" gorm:"serializer:json"`
	NegativeQuestions []string         `json:"negative_questions" gorm:"serializer:json"`
	AnswerStrategy    string           `json:"answer_strategy"`
	References        []*SearchResult  `json:"references,omitempty" gorm:"serializer:json;column:knowledge_references"`
	Feedback          *MessageFeedback `json:"feedback" gorm:"type:jsonb"`
	FeedbackTime      float64          `json:"-"`
	CurrentFAQ        *FAQEntry        `json:"current_faq,omitempty" gorm:"-"`
	CurrentFAQStatus  string           `json:"current_faq_status,omitempty" gorm:"-"`
}

type FeedbackStats struct {
	Total    int64 `json:"total"`
	Likes    int64 `json:"likes"`
	Dislikes int64 `json:"dislikes"`
}

type FeedbackPage struct {
	Items    []*FeedbackRecord `json:"items"`
	Stats    FeedbackStats     `json:"stats"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

type FAQFeedbackSummary struct {
	EntryID           string `json:"entry_id"`
	StandardQuestion  string `json:"standard_question"`
	KnowledgeBaseID   string `json:"knowledge_base_id"`
	KnowledgeBaseName string `json:"knowledge_base_name"`
	TagName           string `json:"tag_name"`
	Total             int64  `json:"total"`
	Likes             int64  `json:"likes"`
	Dislikes          int64  `json:"dislikes"`
	LastFeedbackAt    string `json:"last_feedback_at"`
}

type FAQFeedbackSummaryPage struct {
	Items    []*FAQFeedbackSummary `json:"items"`
	Total    int64                 `json:"total"`
	Stats    FeedbackStats         `json:"stats"`
	Page     int                   `json:"page"`
	PageSize int                   `json:"page_size"`
}
