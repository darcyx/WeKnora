package types

import (
	"errors"
	"time"
)

var ErrTokenQuotaExceeded = errors.New("token quota exceeded")

// TokenQuotaOverride overrides platform token limits for one external user.
// SubjectID is deliberately not a foreign key to users: API integrations may
// use an external identity that has no local WeKnora account.
type TokenQuotaOverride struct {
	SubjectID         string `gorm:"primaryKey;type:varchar(160)" json:"subject_id"`
	DailyTokenLimit   *int64 `gorm:"column:daily_token_limit" json:"daily_token_limit,omitempty"`
	MonthlyTokenLimit *int64 `gorm:"column:monthly_token_limit" json:"monthly_token_limit,omitempty"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (TokenQuotaOverride) TableName() string { return "token_quota_overrides" }

// TokenQuotaPeriodUsage stores settled and reserved usage for one UTC period.
// A reservation is represented separately so abandoned streams can safely be
// released without losing the aggregate's concurrency protection.
type TokenQuotaPeriodUsage struct {
	SubjectID        string    `gorm:"primaryKey;type:varchar(160)" json:"subject_id"`
	Period           string    `gorm:"primaryKey;type:varchar(8)" json:"period"`
	PeriodStart      time.Time `gorm:"primaryKey;type:date" json:"period_start"`
	PromptTokens     int64     `gorm:"not null;default:0" json:"prompt_tokens"`
	CompletionTokens int64     `gorm:"not null;default:0" json:"completion_tokens"`
	TotalTokens      int64     `gorm:"not null;default:0" json:"total_tokens"`
	ReservedTokens   int64     `gorm:"not null;default:0" json:"reserved_tokens"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (TokenQuotaPeriodUsage) TableName() string { return "token_quota_period_usages" }

const (
	TokenQuotaPeriodDay   = "day"
	TokenQuotaPeriodMonth = "month"

	TokenQuotaReservationPending  = "pending"
	TokenQuotaReservationSettled  = "settled"
	TokenQuotaReservationReleased = "released"
)

// TokenQuotaReservation is the durable accounting handle for a single LLM
// call. It makes settlement/release idempotent and lets a later request clean
// up an expired abandoned stream.
type TokenQuotaReservation struct {
	ID         string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	SubjectID  string    `gorm:"index;type:varchar(160);not null" json:"subject_id"`
	DayStart   time.Time `gorm:"type:date;not null" json:"day_start"`
	MonthStart time.Time `gorm:"type:date;not null" json:"month_start"`
	// PromptTokens is request-local input for the repository's atomic output
	// cap calculation. It is intentionally not persisted.
	PromptTokens   int64      `gorm:"-" json:"-"`
	ReservedTokens int64      `gorm:"not null" json:"reserved_tokens"`
	Status         string     `gorm:"type:varchar(16);not null" json:"status"`
	ExpiresAt      time.Time  `gorm:"index;not null" json:"expires_at"`
	SettledAt      *time.Time `json:"settled_at,omitempty"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (TokenQuotaReservation) TableName() string { return "token_quota_reservations" }

// TokenQuotaLimits is the effective result after platform defaults and a
// subject-specific override are merged. Zero means that period is unlimited.
type TokenQuotaLimits struct {
	DailyTokenLimit   int64 `json:"daily_token_limit"`
	MonthlyTokenLimit int64 `json:"monthly_token_limit"`
}

// TokenQuotaUsageSnapshot is returned by the management API and is kept
// separate from persistence rows so callers do not couple to aggregate shape.
type TokenQuotaUsageSnapshot struct {
	SubjectID string                 `json:"subject_id"`
	Limits    TokenQuotaLimits       `json:"limits"`
	Override  *TokenQuotaOverride    `json:"override,omitempty"`
	Daily     *TokenQuotaPeriodUsage `json:"daily,omitempty"`
	Monthly   *TokenQuotaPeriodUsage `json:"monthly,omitempty"`
}

// TokenQuotaUser is one observed API-integration user within a workspace.
// Users appear after they consume quota or receive an explicit override.
type TokenQuotaUser struct {
	ExternalUserID string                   `json:"external_user_id"`
	Quota          *TokenQuotaUsageSnapshot `json:"quota"`
}

// TokenQuotaUserPage is a stable page of observed external users in one
// workspace. Page numbering is one-based.
type TokenQuotaUserPage struct {
	Items    []TokenQuotaUser `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}
