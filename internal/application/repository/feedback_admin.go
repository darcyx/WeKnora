package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// FeedbackRepository provides read-only administrative queries across both stores.
type FeedbackRepository struct{ db *gorm.DB }

func NewFeedbackRepository(db *gorm.DB) *FeedbackRepository { return &FeedbackRepository{db: db} }

func (r *FeedbackRepository) jsonText(column, field string) string {
	if r.db.Dialector.Name() == "postgres" {
		return fmt.Sprintf("(CAST(%s AS jsonb)->>'%s')", column, field)
	}
	return fmt.Sprintf("json_extract(%s, '$.%s')", column, field)
}
func (r *FeedbackRepository) epoch(column string) string {
	value := r.jsonText(column, "created_at")
	if r.db.Dialector.Name() == "postgres" {
		return "EXTRACT(EPOCH FROM CAST(" + value + " AS timestamptz))"
	}
	return "((julianday(" + value + ") - 2440587.5) * 86400.0)"
}

// Every arm applies its tenant predicate BEFORE the union. Only the page's
// selected rows carry full answers/references; list payloads stay bounded.
func (r *FeedbackRepository) base(full bool) string {
	fq := "COALESCE(f.standard_question, '')"
	mq := "COALESCE((SELECT um.content FROM messages um WHERE um.session_id=m.session_id AND um.request_id=m.request_id AND um.role='user' AND um.deleted_at IS NULL ORDER BY um.created_at, um.id LIMIT 1), '')"
	fa, ma, refs, similar, negative := "'[]'", "'[]'", "'[]'", "'[]'", "'[]'"
	if full {
		fa = "COALESCE(CAST(f.answers AS TEXT), '[]')"
		similar = "COALESCE(CAST(f.similar_questions AS TEXT), '[]')"
		negative = "COALESCE(CAST(f.negative_questions AS TEXT), '[]')"
		refs = "COALESCE(CAST(m.knowledge_references AS TEXT), '[]')"
		if r.db.Dialector.Name() == "postgres" {
			ma = "CAST(jsonb_build_array(COALESCE(m.content, '')) AS TEXT)"
		} else {
			ma = "json_array(COALESCE(m.content, ''))"
		}
	}
	return fmt.Sprintf(`SELECT 'faq' AS source, f.session_id, CAST(f.entry_id AS TEXT) AS target_id, '' AS message_id,
 f.user_id, COALESCE(NULLIF(u.username,''), f.user_id) AS user_label, COALESCE(s.title,'') AS session_title,
 %s AS question, %s AS answers, COALESCE(f.knowledge_base_id,'') AS knowledge_base_id,
 COALESCE(k.name,'') AS knowledge_base_name, COALESCE(f.tag_name,'') AS tag_name,
 %s AS similar_questions, %s AS negative_questions, COALESCE(f.answer_strategy,'') AS answer_strategy,
 '[]' AS knowledge_references, CAST(f.feedback AS TEXT) AS feedback, %s AS feedback_time
 FROM faq_feedbacks f
 LEFT JOIN sessions s ON s.id=f.session_id AND s.tenant_id=f.tenant_id AND s.deleted_at IS NULL
 LEFT JOIN users u ON u.id=f.user_id AND u.deleted_at IS NULL
 LEFT JOIN knowledge_bases k ON k.id=f.knowledge_base_id AND k.tenant_id=f.tenant_id AND k.deleted_at IS NULL
 WHERE f.tenant_id=? AND f.feedback IS NOT NULL
 UNION ALL
 SELECT 'message', m.session_id, COALESCE(m.request_id,''), m.id,
 s.user_id, COALESCE(NULLIF(u.username,''),s.user_id), s.title,
 %s, %s, '', '', '', '[]', '[]', '', %s, CAST(m.feedback AS TEXT), %s
 FROM messages m JOIN sessions s ON s.id=m.session_id
 LEFT JOIN users u ON u.id=s.user_id AND u.deleted_at IS NULL
 WHERE s.tenant_id=? AND s.deleted_at IS NULL AND m.deleted_at IS NULL AND m.role='assistant' AND m.feedback IS NOT NULL`, fq, fa, similar, negative, r.epoch("f.feedback"), mq, ma, refs, r.epoch("m.feedback"))
}

func likeFeedbackSearch(s string) string {
	return "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(s)) + "%"
}
func (r *FeedbackRepository) filtered(tenant uint64, q types.FeedbackQuery, full bool) (string, []any) {
	clauses := []string{r.jsonText("fb.feedback", "type") + " IN ('like','dislike')"}
	args := []any{tenant, tenant}
	for _, f := range []struct{ column, value string }{{"source", q.Source}, {"session_id", q.SessionID}, {"target_id", q.TargetID}, {"user_id", q.UserID}, {"knowledge_base_id", q.KnowledgeBaseID}, {"tag_name", q.TagName}} {
		if f.value != "" {
			clauses = append(clauses, "fb."+f.column+" = ?")
			args = append(args, f.value)
		}
	}
	if q.Vote != "" {
		clauses = append(clauses, r.jsonText("fb.feedback", "type")+" = ?")
		args = append(args, q.Vote)
	}
	if !q.From.IsZero() {
		clauses = append(clauses, "fb.feedback_time >= ?")
		args = append(args, float64(q.From.UnixNano())/1e9)
	}
	if !q.To.IsZero() {
		clauses = append(clauses, "fb.feedback_time < ?")
		args = append(args, float64(q.To.UnixNano())/1e9)
	}
	if q.Search != "" {
		clauses = append(clauses, "(LOWER(fb.question) LIKE ? ESCAPE '!' OR LOWER(COALESCE("+r.jsonText("fb.feedback", "reason_text")+",'')) LIKE ? ESCAPE '!')")
		v := likeFeedbackSearch(q.Search)
		args = append(args, v, v)
	}
	if len(q.Reasons) > 0 {
		parts := []string{}
		for _, reason := range q.Reasons {
			if r.db.Dialector.Name() == "postgres" {
				parts = append(parts, "EXISTS (SELECT 1 FROM jsonb_array_elements_text(COALESCE(CAST(fb.feedback AS jsonb)->'reasons','[]'::jsonb)) reason WHERE reason.value=?)")
			} else {
				parts = append(parts, "EXISTS (SELECT 1 FROM json_each(fb.feedback,'$.reasons') reason WHERE reason.value=?)")
			}
			args = append(args, reason)
		}
		clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
	}
	return "SELECT fb.* FROM (" + r.base(full) + ") fb WHERE " + strings.Join(clauses, " AND "), args
}
func (r *FeedbackRepository) statsSQL(filtered string) string {
	typ := r.jsonText("fb.feedback", "type")
	return "SELECT COUNT(*) AS total, COALESCE(SUM(CASE WHEN " + typ + "='like' THEN 1 ELSE 0 END),0) AS likes, COALESCE(SUM(CASE WHEN " + typ + "='dislike' THEN 1 ELSE 0 END),0) AS dislikes FROM (" + filtered + ") fb"
}

const feedbackOrder = " ORDER BY feedback_time DESC, source, session_id, target_id, message_id"

func (r *FeedbackRepository) List(ctx context.Context, tenant uint64, q types.FeedbackQuery, full bool) (*types.FeedbackPage, error) {
	filtered, args := r.filtered(tenant, q, full)
	result := &types.FeedbackPage{Items: []*types.FeedbackRecord{}, Page: q.Page, PageSize: q.PageSize}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Raw(r.statsSQL(filtered), args...).Scan(&result.Stats).Error; err != nil {
			return err
		}
		if full && result.Stats.Total >= int64(q.PageSize) {
			return nil
		}
		pageArgs := append(append([]any{}, args...), q.PageSize, (q.Page-1)*q.PageSize)
		if err := tx.Raw("SELECT * FROM ("+filtered+") page"+feedbackOrder+" LIMIT ? OFFSET ?", pageArgs...).Scan(&result.Items).Error; err != nil {
			return err
		}
		if !full {
			for _, row := range result.Items {
				rs := []rune(row.Question)
				if len(rs) > 180 {
					row.Question = string(rs[:180]) + "…"
				}
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, err
}
func (r *FeedbackRepository) Detail(ctx context.Context, tenant uint64, source, session, target, messageID string) ([]*types.FeedbackRecord, error) {
	filtered, args := r.filtered(tenant, types.FeedbackQuery{Source: source, SessionID: session, TargetID: target}, true)
	if messageID != "" {
		filtered = "SELECT * FROM (" + filtered + ") detail WHERE message_id=?"
		args = append(args, messageID)
	}
	rows := []*types.FeedbackRecord{}
	err := r.db.WithContext(ctx).Raw(filtered+" LIMIT 2", args...).Scan(&rows).Error
	return rows, err
}
func (r *FeedbackRepository) Summary(ctx context.Context, tenant uint64, q types.FeedbackQuery) (*types.FAQFeedbackSummaryPage, error) {
	q.Source = "faq"
	filtered, args := r.filtered(tenant, q, false)
	typ := r.jsonText("feedback", "type")
	cte := "WITH filtered AS (" + filtered + "), ranked AS (SELECT *, ROW_NUMBER() OVER (PARTITION BY target_id ORDER BY feedback_time DESC, session_id) AS rn FROM filtered), counts AS (SELECT target_id, COUNT(*) AS total, SUM(CASE WHEN " + typ + "='like' THEN 1 ELSE 0 END) AS likes, SUM(CASE WHEN " + typ + "='dislike' THEN 1 ELSE 0 END) AS dislikes FROM filtered GROUP BY target_id) "
	result := &types.FAQFeedbackSummaryPage{Items: []*types.FAQFeedbackSummary{}, Page: q.Page, PageSize: q.PageSize}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Raw(r.statsSQL(filtered), args...).Scan(&result.Stats).Error; err != nil {
			return err
		}
		if err := tx.Raw("SELECT COUNT(DISTINCT target_id) FROM ("+filtered+") entries", args...).Scan(&result.Total).Error; err != nil {
			return err
		}
		order := "c.dislikes DESC, r.feedback_time DESC, r.target_id"
		if q.Sort == "total" {
			order = "c.total DESC, r.feedback_time DESC, r.target_id"
		}
		query := cte + "SELECT r.target_id AS entry_id, r.question AS standard_question, r.knowledge_base_id, r.knowledge_base_name, r.tag_name, c.total, c.likes, c.dislikes, " + r.jsonText("r.feedback", "created_at") + " AS last_feedback_at FROM ranked r JOIN counts c ON c.target_id=r.target_id WHERE r.rn=1 ORDER BY " + order + " LIMIT ? OFFSET ?"
		return tx.Raw(query, append(append([]any{}, args...), q.PageSize, (q.Page-1)*q.PageSize)...).Scan(&result.Items).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, err
}
