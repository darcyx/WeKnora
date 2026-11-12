package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// Retrieval state shares the existing internal execution_context column.
func (r *messageRepository) GetPreviousRetrievalState(ctx context.Context, sessionID, messageID string) (*types.Message, error) {
	current, err := r.GetMessage(ctx, sessionID, messageID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("current assistant message is missing")
	}
	predicate := "execution_context -> 'retrieval_state' IS NOT NULL"
	if r.db.Dialector.Name() == "sqlite" {
		predicate = "json_extract(execution_context, '$.retrieval_state') IS NOT NULL"
	}
	var messages []*types.Message
	err = r.db.WithContext(ctx).Select("id", "session_id", "execution_context").Where("session_id = ? AND role = 'assistant'", sessionID).
		Where("created_at < ? OR (created_at = ? AND id < ?)", current.CreatedAt, current.CreatedAt, current.ID).
		Where(predicate).Order("created_at DESC, id DESC").Limit(1).Find(&messages).Error
	if err != nil || len(messages) == 0 {
		return nil, err
	}
	return messages[0], nil
}

func (r *messageRepository) UpdateMessageRetrievalState(ctx context.Context, sessionID, messageID string, state json.RawMessage) error {
	if !json.Valid(state) {
		return fmt.Errorf("invalid retrieval state JSON")
	}
	expression := gorm.Expr("jsonb_set(COALESCE(execution_context, '{}'::jsonb), '{retrieval_state}', ?::jsonb, true)", string(state))
	if r.db.Dialector.Name() == "sqlite" {
		expression = gorm.Expr("json_set(COALESCE(execution_context, '{}'), '$.retrieval_state', json(?))", string(state))
	}
	result := r.db.WithContext(ctx).Model(&types.Message{}).Where("session_id = ? AND id = ? AND role = 'assistant'", sessionID, messageID).UpdateColumn("execution_context", expression)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("retrieval state assistant message is missing")
	}
	return nil
}
