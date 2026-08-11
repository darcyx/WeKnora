package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type tokenQuotaRepository struct {
	db *gorm.DB
}

func NewTokenQuotaRepository(db *gorm.DB) interfaces.TokenQuotaRepository {
	return &tokenQuotaRepository{db: db}
}

func (r *tokenQuotaRepository) GetOverride(ctx context.Context, subjectID string) (*types.TokenQuotaOverride, error) {
	var override types.TokenQuotaOverride
	err := r.db.WithContext(ctx).Where("subject_id = ?", subjectID).First(&override).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &override, nil
}

func (r *tokenQuotaRepository) UpsertOverride(ctx context.Context, override *types.TokenQuotaOverride) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "subject_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"daily_token_limit", "monthly_token_limit", "updated_at",
		}),
	}).Create(override).Error
}

func (r *tokenQuotaRepository) DeleteOverride(ctx context.Context, subjectID string) (bool, error) {
	result := r.db.WithContext(ctx).Where("subject_id = ?", subjectID).Delete(&types.TokenQuotaOverride{})
	return result.RowsAffected > 0, result.Error
}

func (r *tokenQuotaRepository) GetPeriodUsage(
	ctx context.Context,
	subjectID, period string,
	periodStart time.Time,
) (*types.TokenQuotaPeriodUsage, error) {
	var usage types.TokenQuotaPeriodUsage
	err := r.db.WithContext(ctx).
		Where("subject_id = ? AND period = ? AND period_start = ?", subjectID, period, periodStart).
		First(&usage).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &usage, nil
}

func (r *tokenQuotaRepository) ListUserQuotaSnapshots(
	ctx context.Context,
	subjectPrefix string,
	dayStart, monthStart time.Time,
	limit, offset int,
) ([]*types.TokenQuotaUsageSnapshot, int64, error) {
	const subjectsSQL = `
SELECT subject_id FROM (
    SELECT subject_id FROM token_quota_overrides WHERE subject_id LIKE ?
    UNION
    SELECT subject_id FROM token_quota_period_usages WHERE subject_id LIKE ?
) AS token_quota_subjects
ORDER BY subject_id ASC
LIMIT ? OFFSET ?`
	const countSQL = `
SELECT COUNT(*) FROM (
    SELECT subject_id FROM token_quota_overrides WHERE subject_id LIKE ?
    UNION
    SELECT subject_id FROM token_quota_period_usages WHERE subject_id LIKE ?
) AS token_quota_subjects`

	pattern := subjectPrefix + "%"
	var total int64
	if err := r.db.WithContext(ctx).Raw(countSQL, pattern, pattern).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	var subjectIDs []string
	if err := r.db.WithContext(ctx).Raw(subjectsSQL, pattern, pattern, limit, offset).Scan(&subjectIDs).Error; err != nil {
		return nil, 0, err
	}
	if len(subjectIDs) == 0 {
		return []*types.TokenQuotaUsageSnapshot{}, total, nil
	}

	var overrides []types.TokenQuotaOverride
	if err := r.db.WithContext(ctx).Where("subject_id IN ?", subjectIDs).Find(&overrides).Error; err != nil {
		return nil, 0, err
	}
	var periodUsages []types.TokenQuotaPeriodUsage
	if err := r.db.WithContext(ctx).
		Where(
			"subject_id IN ? AND ((period = ? AND period_start = ?) OR (period = ? AND period_start = ?))",
			subjectIDs,
			types.TokenQuotaPeriodDay, dayStart,
			types.TokenQuotaPeriodMonth, monthStart,
		).
		Find(&periodUsages).Error; err != nil {
		return nil, 0, err
	}

	bySubject := make(map[string]*types.TokenQuotaUsageSnapshot, len(subjectIDs))
	for _, subjectID := range subjectIDs {
		bySubject[subjectID] = &types.TokenQuotaUsageSnapshot{SubjectID: subjectID}
	}
	for i := range overrides {
		if snapshot := bySubject[overrides[i].SubjectID]; snapshot != nil {
			snapshot.Override = &overrides[i]
		}
	}
	for i := range periodUsages {
		usage := &periodUsages[i]
		snapshot := bySubject[usage.SubjectID]
		if snapshot == nil {
			continue
		}
		switch usage.Period {
		case types.TokenQuotaPeriodDay:
			snapshot.Daily = usage
		case types.TokenQuotaPeriodMonth:
			snapshot.Monthly = usage
		}
	}

	snapshots := make([]*types.TokenQuotaUsageSnapshot, 0, len(subjectIDs))
	for _, subjectID := range subjectIDs {
		snapshots = append(snapshots, bySubject[subjectID])
	}
	return snapshots, total, nil
}

func (r *tokenQuotaRepository) Reserve(
	ctx context.Context,
	reservation *types.TokenQuotaReservation,
	limits types.TokenQuotaLimits,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		periods := []struct {
			name  string
			start time.Time
			limit int64
		}{
			{types.TokenQuotaPeriodDay, reservation.DayStart, limits.DailyTokenLimit},
			{types.TokenQuotaPeriodMonth, reservation.MonthStart, limits.MonthlyTokenLimit},
		}
		rows := make([]types.TokenQuotaPeriodUsage, 0, len(periods))
		allowedReservation := reservation.ReservedTokens
		for _, period := range periods {
			seed := types.TokenQuotaPeriodUsage{
				SubjectID:   reservation.SubjectID,
				Period:      period.name,
				PeriodStart: period.start,
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
				return err
			}

			var row types.TokenQuotaPeriodUsage
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("subject_id = ? AND period = ? AND period_start = ?", reservation.SubjectID, period.name, period.start).
				First(&row).Error; err != nil {
				return err
			}
			if period.limit > 0 {
				remaining := period.limit - row.TotalTokens - row.ReservedTokens
				if remaining < allowedReservation {
					allowedReservation = remaining
				}
			}
			rows = append(rows, row)
		}
		if allowedReservation <= reservation.PromptTokens {
			return types.ErrTokenQuotaExceeded
		}
		reservation.ReservedTokens = allowedReservation

		for _, row := range rows {
			if err := tx.Model(&types.TokenQuotaPeriodUsage{}).
				Where("subject_id = ? AND period = ? AND period_start = ?", row.SubjectID, row.Period, row.PeriodStart).
				UpdateColumn("reserved_tokens", gorm.Expr("reserved_tokens + ?", reservation.ReservedTokens)).Error; err != nil {
				return err
			}
		}
		return tx.Create(reservation).Error
	})
}

func (r *tokenQuotaRepository) Settle(ctx context.Context, reservationID string, usage types.TokenUsage) error {
	return r.finish(ctx, reservationID, types.TokenQuotaReservationSettled, usage)
}

func (r *tokenQuotaRepository) Release(ctx context.Context, reservationID string) error {
	return r.finish(ctx, reservationID, types.TokenQuotaReservationReleased, types.TokenUsage{})
}

func (r *tokenQuotaRepository) ReleaseExpired(ctx context.Context, subjectID string, before time.Time) error {
	var reservations []types.TokenQuotaReservation
	if err := r.db.WithContext(ctx).
		Where("subject_id = ? AND status = ? AND expires_at <= ?", subjectID, types.TokenQuotaReservationPending, before).
		Find(&reservations).Error; err != nil {
		return err
	}
	for _, reservation := range reservations {
		if err := r.Release(ctx, reservation.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r *tokenQuotaRepository) finish(
	ctx context.Context,
	reservationID, targetStatus string,
	usage types.TokenUsage,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var reservation types.TokenQuotaReservation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", reservationID).
			First(&reservation).Error; err != nil {
			return err
		}
		if reservation.Status != types.TokenQuotaReservationPending {
			return nil
		}

		periods := []struct {
			name  string
			start time.Time
		}{
			{types.TokenQuotaPeriodDay, reservation.DayStart},
			{types.TokenQuotaPeriodMonth, reservation.MonthStart},
		}
		for _, period := range periods {
			var row types.TokenQuotaPeriodUsage
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("subject_id = ? AND period = ? AND period_start = ?", reservation.SubjectID, period.name, period.start).
				First(&row).Error; err != nil {
				return err
			}
			updates := map[string]any{
				"reserved_tokens": gorm.Expr("reserved_tokens - ?", reservation.ReservedTokens),
			}
			if targetStatus == types.TokenQuotaReservationSettled {
				updates["prompt_tokens"] = gorm.Expr("prompt_tokens + ?", usage.PromptTokens)
				updates["completion_tokens"] = gorm.Expr("completion_tokens + ?", usage.CompletionTokens)
				updates["total_tokens"] = gorm.Expr("total_tokens + ?", usage.TotalTokens)
			}
			if err := tx.Model(&row).Updates(updates).Error; err != nil {
				return err
			}
		}

		now := time.Now().UTC()
		return tx.Model(&reservation).Updates(map[string]any{
			"status":     targetStatus,
			"settled_at": &now,
		}).Error
	})
}
