package repository

import (
	"context"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm/clause"
)

func (r *messageRepository) CreateFAQFeedback(ctx context.Context, feedback *types.FAQFeedback) error {
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "session_id"}, {Name: "entry_id"}},
		DoNothing: true,
	}).Create(feedback)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return apperrors.ErrMessageFeedbackAlreadySubmitted
	}
	return nil
}
