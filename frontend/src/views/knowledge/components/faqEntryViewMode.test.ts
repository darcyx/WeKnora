import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import { parseFAQEntryViewMode } from './faqEntryViewMode.ts'

const manager = readFileSync(new URL('./FAQEntryManager.vue', import.meta.url), 'utf8')

test('restores the FAQ list view only for its explicit persisted value', () => {
  assert.equal(parseFAQEntryViewMode('list'), 'list')
  assert.equal(parseFAQEntryViewMode('card'), 'card')
  assert.equal(parseFAQEntryViewMode('grid'), 'card')
  assert.equal(parseFAQEntryViewMode(null), 'card')
})

test('FAQ manager exposes both views with explicit selection controls', () => {
  assert.match(manager, /faq-view-toggle/)
  assert.match(manager, /faq-list-view/)
  assert.match(manager, /allEntriesSelected/)
  assert.match(manager, /handleSelectAll/)
  assert.match(manager, /mergeFAQEntryPage/)
})
