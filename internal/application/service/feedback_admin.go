package service

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type FeedbackAdminService struct {
	repo      *repository.FeedbackRepository
	knowledge interfaces.KnowledgeService
}

func NewFeedbackAdminService(repo *repository.FeedbackRepository, knowledge interfaces.KnowledgeService) *FeedbackAdminService {
	return &FeedbackAdminService{repo: repo, knowledge: knowledge}
}
func feedbackAdminTenant(ctx context.Context) (uint64, error) {
	tenant, ok := types.TenantIDFromContext(ctx)
	if !ok || tenant == 0 {
		return 0, apperrors.NewForbiddenError("workspace is required")
	}
	if !types.CallerFromContext(ctx).Role.HasPermission(types.TenantRoleAdmin) && !types.IsSystemAdminFromContext(ctx) {
		return 0, apperrors.NewForbiddenError("workspace admin role is required")
	}
	return tenant, nil
}
func validateFeedbackQuery(q *types.FeedbackQuery) error {
	if q.Source != "" && q.Source != "faq" && q.Source != "message" {
		return apperrors.NewBadRequestError("invalid feedback source")
	}
	if q.Vote != "" && q.Vote != "like" && q.Vote != "dislike" {
		return apperrors.NewBadRequestError("invalid feedback type")
	}
	if q.Sort != "" && q.Sort != "dislikes" && q.Sort != "total" {
		return apperrors.NewBadRequestError("invalid feedback sort")
	}
	for _, s := range []string{q.Search, q.SessionID, q.TargetID, q.UserID, q.KnowledgeBaseID, q.TagName} {
		if len([]rune(s)) > 512 {
			return apperrors.NewBadRequestError("feedback filter is too long")
		}
	}
	if q.Page < 1 || q.Page > 100000 || q.PageSize < 1 || q.PageSize > 100 {
		return apperrors.NewBadRequestError("invalid pagination")
	}
	if len(q.Reasons) > 4 {
		return apperrors.NewBadRequestError("too many reasons")
	}
	for _, reason := range q.Reasons {
		if _, ok := types.ValidMessageFeedbackReasons[reason]; !ok {
			return apperrors.NewBadRequestError("invalid feedback reason")
		}
	}
	if len(q.Reasons) > 0 {
		if q.Vote == "like" {
			return apperrors.NewBadRequestError("reasons only apply to dislike")
		}
		q.Vote = "dislike"
	}
	if q.From.IsZero() != q.To.IsZero() {
		return apperrors.NewBadRequestError("both from and to are required")
	}
	if q.From.IsZero() {
		q.To = time.Now().UTC()
		q.From = q.To.AddDate(0, 0, -7)
	}
	if !q.From.Before(q.To) || q.To.Sub(q.From) > 366*24*time.Hour {
		return apperrors.NewBadRequestError("date range must be between 0 and 366 days")
	}
	q.Search = strings.TrimSpace(q.Search)
	return nil
}
func (s *FeedbackAdminService) List(ctx context.Context, q types.FeedbackQuery) (*types.FeedbackPage, error) {
	tenant, err := feedbackAdminTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateFeedbackQuery(&q); err != nil {
		return nil, err
	}
	return s.repo.List(ctx, tenant, q, false)
}
func (s *FeedbackAdminService) Summary(ctx context.Context, q types.FeedbackQuery) (*types.FAQFeedbackSummaryPage, error) {
	tenant, err := feedbackAdminTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateFeedbackQuery(&q); err != nil {
		return nil, err
	}
	return s.repo.Summary(ctx, tenant, q)
}
func (s *FeedbackAdminService) Detail(ctx context.Context, source, session, target, messageID string) (*types.FeedbackRecord, error) {
	tenant, err := feedbackAdminTenant(ctx)
	if err != nil {
		return nil, err
	}
	if (source != "faq" && source != "message") || session == "" || target == "" {
		return nil, apperrors.NewBadRequestError("invalid feedback identity")
	}
	for _, v := range []string{session, target, messageID} {
		if len(v) > 512 {
			return nil, apperrors.NewBadRequestError("feedback identity is too long")
		}
	}
	rows, err := s.repo.Detail(ctx, tenant, source, session, target, messageID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, apperrors.NewNotFoundError("feedback not found")
	}
	if len(rows) > 1 {
		return nil, apperrors.NewBadRequestError("message_id is required to select the exact feedback")
	}
	row := rows[0]
	if source == "faq" {
		row.CurrentFAQStatus = "unavailable"
		if id, e := strconv.ParseInt(target, 10, 64); e == nil && s.knowledge != nil {
			current, e := s.knowledge.GetFAQEntry(ctx, row.KnowledgeBaseID, id)
			if e == nil && current != nil {
				row.CurrentFAQ = current
				row.CurrentFAQStatus = "available"
			}
		}
	}
	return row, nil
}

const FeedbackExportLimit = 10000

func (s *FeedbackAdminService) Export(ctx context.Context, q types.FeedbackQuery) ([]*types.FeedbackRecord, error) {
	tenant, err := feedbackAdminTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateFeedbackQuery(&q); err != nil {
		return nil, err
	}
	q.Page = 1
	q.PageSize = FeedbackExportLimit + 1
	result, err := s.repo.List(ctx, tenant, q, true)
	if err != nil {
		return nil, err
	}
	if result.Stats.Total > FeedbackExportLimit {
		return nil, apperrors.NewBadRequestError("export exceeds 10000 feedback records; narrow the filters")
	}
	return result.Items, nil
}
