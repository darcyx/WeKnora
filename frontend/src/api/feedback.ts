import { get } from '@/utils/request'

export interface FeedbackVote { type: 'like' | 'dislike'; reasons?: string[]; reason_text?: string; created_at: string }
export interface FeedbackRecord {
  source: 'faq' | 'message'; session_id: string; target_id: string; message_id: string
  user_id: string; user_label: string; session_title: string; question: string; answers: string[]
  knowledge_base_id: string; knowledge_base_name: string; tag_name: string
  similar_questions: string[]; negative_questions: string[]; answer_strategy: string
  feedback: FeedbackVote; references?: Array<{ id: string; knowledge_title?: string; content?: string }>
  current_faq_status?: string
  current_faq?: { standard_question: string; answers: string[] }
}
export interface FeedbackStats { total: number; likes: number; dislikes: number }
export interface FeedbackPage { items: FeedbackRecord[]; stats: FeedbackStats; page: number; page_size: number }
export interface FAQSummary {
  entry_id: string; standard_question: string; knowledge_base_id: string; knowledge_base_name: string
  tag_name: string; total: number; likes: number; dislikes: number; last_feedback_at: string
}
export interface FAQSummaryPage { items: FAQSummary[]; total: number; stats: FeedbackStats; page: number; page_size: number }
export type FeedbackParams = Record<string, string | number>
export const listFeedback = (params: FeedbackParams) => get<{ data: FeedbackPage }>('/api/v1/feedback', { params })
export const summarizeFAQFeedback = (params: FeedbackParams) => get<{ data: FAQSummaryPage }>('/api/v1/feedback/faq-summary', { params })
export const getFeedbackDetail = (row: FeedbackRecord) => get<{ data: FeedbackRecord }>(
  `/api/v1/feedback/${row.source}/${encodeURIComponent(row.session_id)}/${encodeURIComponent(row.target_id)}`,
  { params: { message_id: row.message_id || undefined } },
)
export const exportFeedback = (params: FeedbackParams) => get<Blob>('/api/v1/feedback/export', { params, responseType: 'blob' })
