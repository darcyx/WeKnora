import { test } from 'node:test'
import assert from 'node:assert/strict'
import { feedbackDateParams, feedbackDateRange, feedbackRatio, feedbackRowKey } from './feedbackQuery'
import type { FeedbackRecord } from '../../api/feedback'

test('feedback rows distinguish source, session and actual assistant message', () => {
  const row = { source: 'message', session_id: 's1', target_id: 'req1', message_id: 'm1' } as FeedbackRecord
  assert.notEqual(feedbackRowKey(row), feedbackRowKey({ ...row, message_id: 'm2' }))
  assert.notEqual(feedbackRowKey(row), feedbackRowKey({ ...row, session_id: 's2' }))
  assert.notEqual(feedbackRowKey(row), feedbackRowKey({ ...row, source: 'faq' }))
})
test('date ranges include the selected final day and reject bad input', () => {
  assert.deepEqual(feedbackDateRange(7, new Date(2026, 8, 11)), { from: '2026-09-05', to: '2026-09-11' })
  const range = feedbackDateParams('2026-09-11', '2026-09-11')!
  assert.equal(new Date(String(range.to)).getDate(), 12)
  assert.equal(feedbackDateParams('2026-02-30', '2026-03-01'), null)
  assert.equal(feedbackDateParams('2026-09-12', '2026-09-11'), null)
  assert.equal(feedbackDateParams('bad', '2026-09-11'), null)
})
test('no-feedback ratio has no invented percentage', () => {
  assert.equal(feedbackRatio(0, 0), '—')
  assert.equal(feedbackRatio(1, 3), '33.3%')
})
