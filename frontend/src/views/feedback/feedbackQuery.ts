import type { FeedbackParams, FeedbackRecord } from '@/api/feedback'
export function feedbackRowKey(row: FeedbackRecord): string {
  return JSON.stringify([row.source, row.session_id, row.target_id, row.message_id])
}
export function feedbackRatio(likes: number, total: number): string { return total > 0 ? `${(likes / total * 100).toFixed(1)}%` : '—' }
export function localDate(date: Date): string {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
}
export function feedbackDateRange(days: number, now = new Date()) {
  const from = new Date(now); from.setDate(from.getDate() - days + 1)
  return { from: localDate(from), to: localDate(now) }
}
export function feedbackDateParams(from: string, through: string): FeedbackParams | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(from) || !/^\d{4}-\d{2}-\d{2}$/.test(through)) return null
  const start = new Date(`${from}T00:00:00`), end = new Date(`${through}T00:00:00`)
  if (!Number.isFinite(+start) || !Number.isFinite(+end) || localDate(start) !== from || localDate(end) !== through) return null
  end.setDate(end.getDate() + 1)
  const span = +end - +start
  if (span <= 0 || span > 366 * 86400000) return null
  return { from: start.toISOString(), to: end.toISOString() }
}
