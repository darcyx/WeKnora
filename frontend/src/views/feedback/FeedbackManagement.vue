<template>
  <div class="feedback-page">
    <header class="page-header">
      <div><h2>{{ t('feedbackAdmin.title') }}</h2><p>{{ t('feedbackAdmin.description') }}</p></div>
      <t-button variant="outline" :loading="exporting" :disabled="loading || !stats.total" @click="downloadExport">{{ t('feedbackAdmin.export') }}</t-button>
    </header>
    <t-alert v-if="!allowed" theme="warning" :message="t('feedbackAdmin.denied')" />
    <template v-else>
      <div class="stats" :aria-busy="loading">
        <div class="stat"><span>{{ t('feedbackAdmin.count') }}</span><strong>{{ stats.total }}</strong></div>
        <button class="stat clickable" @click="onlyDislikes"><span>{{ t('feedbackAdmin.dislikes') }}</span><strong>{{ stats.dislikes }}</strong></button>
        <div class="stat"><span>{{ t('feedbackAdmin.ratio') }}</span><strong>{{ feedbackRatio(stats.likes, stats.total) }}</strong></div>
      </div>
      <p class="scope-note">{{ t('feedbackAdmin.scope') }}</p>
      <t-tabs :value="mode" @change="changeMode">
        <t-tab-panel value="detail" :label="t('feedbackAdmin.details')" />
        <t-tab-panel value="summary" :label="t('feedbackAdmin.summary')" />
      </t-tabs>
      <form class="filters" @submit.prevent="apply()">
        <div class="filter-row">
          <t-input v-model="filters.q" clearable :placeholder="t('feedbackAdmin.search')" :aria-label="t('feedbackAdmin.search')" class="search" />
          <t-select v-model="filters.source" :disabled="mode === 'summary'" :aria-label="t('feedbackAdmin.source')" class="small-select">
            <t-option value="" :label="t('feedbackAdmin.allSources')" /><t-option value="faq" :label="t('feedbackAdmin.faq')" /><t-option value="message" :label="t('feedbackAdmin.message')" />
          </t-select>
          <t-select v-model="filters.type" :aria-label="t('feedbackAdmin.rating')" class="small-select" @change="voteChanged">
            <t-option value="" :label="t('feedbackAdmin.allVotes')" /><t-option value="like" :label="t('feedbackAdmin.like')" /><t-option value="dislike" :label="t('feedbackAdmin.dislike')" />
          </t-select>
          <t-select v-model="reasons" multiple clearable :placeholder="t('feedbackAdmin.reasons')" :aria-label="t('feedbackAdmin.reasons')" class="reason-select" @change="reasonsChanged">
            <t-option v-for="reason in reasonCodes" :key="reason" :value="reason" :label="reasonLabel(reason)" />
          </t-select>
        </div>
        <div class="filter-row">
          <label>{{ t('feedbackAdmin.from') }}<input v-model="filters.start" type="date" required /></label>
          <label>{{ t('feedbackAdmin.to') }}<input v-model="filters.end" type="date" required /></label>
          <t-button variant="text" @click="preset(7)">{{ t('feedbackAdmin.days7') }}</t-button>
          <t-button variant="text" @click="preset(30)">{{ t('feedbackAdmin.days30') }}</t-button>
          <t-button type="submit" :loading="loading">{{ t('feedbackAdmin.apply') }}</t-button>
          <t-button variant="text" @click="reset">{{ t('feedbackAdmin.reset') }}</t-button>
        </div>
        <details><summary>{{ t('feedbackAdmin.advanced') }}</summary><div class="advanced">
          <label>session_id<t-input v-model="filters.session_id" clearable /></label>
          <label>{{ filters.source === 'faq' || mode === 'summary' ? t('feedbackAdmin.faqID') : t('feedbackAdmin.request') }}<t-input v-model="filters.target_id" clearable /></label>
          <label>{{ t('feedbackAdmin.user') }}<t-input v-model="filters.user_id" clearable /></label>
          <template v-if="filters.source === 'faq' || mode === 'summary'">
            <label>{{ t('feedbackAdmin.kb') }}<t-input v-model="filters.knowledge_base_id" clearable /></label>
            <label>{{ t('feedbackAdmin.tag') }}<t-input v-model="filters.tag_name" clearable /></label>
          </template>
        </div></details>
      </form>
      <div v-if="mode === 'summary'" class="summary-toolbar"><p>{{ t('feedbackAdmin.summaryNote') }}</p><t-select v-model="filters.sort" class="small-select" @change="apply()"><t-option value="dislikes" :label="t('feedbackAdmin.sortDislikes')" /><t-option value="total" :label="t('feedbackAdmin.sortTotal')" /></t-select></div>
      <p v-if="applied.target_id && applied.source === 'faq'" class="scope-note">{{ t('feedbackAdmin.selectedFAQ', { id: applied.target_id }) }}</p>
      <t-alert v-if="error" theme="error" :message="error"><template #operation><t-button size="small" @click="load">{{ t('feedbackAdmin.retry') }}</t-button></template></t-alert>
      <t-loading :loading="loading" class="results">
        <div class="data-table-shell">
          <t-table v-if="mode === 'detail'" row-key="row_key" :data="tableRows" :columns="columns" hover :empty="t('feedbackAdmin.empty')">
            <template #time="{ row }"><span class="time">{{ dateTime(row.feedback.created_at) }}</span></template>
            <template #source="{ row }"><t-tag variant="light">{{ sourceLabel(row.source) }}</t-tag></template>
            <template #question="{ row }"><div class="question">{{ row.question || t('feedbackAdmin.unavailable') }}</div><div class="subtext">{{ row.source === 'faq' ? [row.knowledge_base_name || row.knowledge_base_id, row.tag_name].filter(Boolean).join(' / ') : row.session_title }}</div></template>
            <template #rating="{ row }"><t-tag :theme="row.feedback.type === 'dislike' ? 'danger' : 'success'" variant="light">{{ voteLabel(row.feedback.type) }}</t-tag><div class="subtext">{{ (row.feedback.reasons || []).map(reasonLabel).join(' · ') }}</div></template>
            <template #user="{ row }"><span class="user-label">{{ row.user_label || row.user_id || '—' }}</span></template>
            <template #operation="{ row }"><t-button variant="text" theme="primary" size="small" @click="openDetail(row)">{{ t('feedbackAdmin.view') }}</t-button></template>
          </t-table>
          <t-table v-else row-key="entry_id" :data="summaries" :columns="summaryColumns" hover :empty="t('feedbackAdmin.empty')">
            <template #question="{ row }"><div class="question">{{ row.standard_question || t('feedbackAdmin.unavailable') }}</div><div class="subtext">{{ [row.knowledge_base_name || row.knowledge_base_id, row.tag_name].filter(Boolean).join(' / ') }}</div></template>
            <template #ratio="{ row }">{{ feedbackRatio(row.likes, row.total) }}</template>
            <template #latest="{ row }">{{ dateTime(row.last_feedback_at) }}</template>
            <template #operation="{ row }"><t-button size="small" variant="text" theme="primary" @click="drillDown(row)">{{ t('feedbackAdmin.details') }}</t-button></template>
          </t-table>
        </div>
      </t-loading>
      <t-pagination :current="page" :page-size="20" :page-size-options="[]" :total="total" :disabled="loading" @current-change="changePage" />
    </template>
    <t-drawer v-model:visible="drawerVisible" :header="t('feedbackAdmin.detailTitle')" size="min(640px, 100vw)" :close-on-overlay-click="true" @close="closeDetail">
      <t-loading :loading="detailLoading">
        <t-alert v-if="detailError" theme="error" :message="detailError"><template #operation><t-button @click="selected && openDetail(selected)">{{ t('feedbackAdmin.retry') }}</t-button></template></t-alert>
        <article v-if="detail" class="feedback-detail">
          <div class="detail-heading"><t-tag :theme="detail.feedback.type === 'dislike' ? 'danger' : 'success'">{{ voteLabel(detail.feedback.type) }}</t-tag><span>{{ dateTime(detail.feedback.created_at) }} · {{ detail.user_label || detail.user_id }}</span></div>
          <div class="reason-tags"><t-tag v-for="r in detail.feedback.reasons || []" :key="r" variant="light">{{ reasonLabel(r) }}</t-tag></div>
          <section class="user-comment"><h4>{{ t('feedbackAdmin.comment') }}</h4><p>{{ detail.feedback.reason_text || t('feedbackAdmin.noComment') }}</p></section>
          <h3>{{ detail.source === 'faq' ? t('feedbackAdmin.faqContent') : t('feedbackAdmin.messageContent') }}</h3>
          <p v-if="detail.source === 'faq'" class="subtext">{{ t('feedbackAdmin.faqNote') }}</p>
          <h4>{{ detail.source === 'faq' ? t('feedbackAdmin.standardQuestion') : t('feedbackAdmin.question') }}</h4><p class="plain-content">{{ detail.question || t('feedbackAdmin.unavailable') }}</p>
          <h4>{{ t('feedbackAdmin.answers') }}</h4><div v-for="(answer, index) in detail.answers" :key="index" class="answer markdown-content" v-html="markdown(answer)" />
          <template v-if="detail.source === 'faq'">
            <details class="faq-more"><summary>{{ t('feedbackAdmin.moreFAQ') }}</summary><h4>{{ t('feedbackAdmin.similar') }}</h4><p>{{ (detail.similar_questions || []).join('\n') || '—' }}</p><h4>{{ t('feedbackAdmin.negative') }}</h4><p>{{ (detail.negative_questions || []).join('\n') || '—' }}</p><h4>{{ t('feedbackAdmin.strategy') }}</h4><p>{{ detail.answer_strategy === 'all' ? t('feedbackAdmin.allStrategy') : detail.answer_strategy === 'random' ? t('feedbackAdmin.randomStrategy') : detail.answer_strategy || '—' }}</p></details>
            <details v-if="detail.current_faq" class="current-faq"><summary>{{ t('feedbackAdmin.currentFAQ') }}</summary><h4>{{ t('feedbackAdmin.currentLabel') }}</h4><p>{{ detail.current_faq.standard_question }}</p><div v-for="(answer, i) in detail.current_faq.answers" :key="i" class="answer markdown-content" v-html="markdown(answer)" /></details>
            <p v-else class="subtext">{{ t('feedbackAdmin.currentUnavailable') }}</p>
          </template>
          <details v-if="detail.references?.length"><summary>{{ t('feedbackAdmin.references') }} ({{ detail.references.length }})</summary><section v-for="(reference, i) in detail.references" :key="i"><h4>{{ reference.knowledge_title || reference.id }}</h4><p class="plain-content">{{ reference.content }}</p></section></details>
          <dl class="identity"><template v-for="item in identity" :key="item.label"><dt>{{ item.label }}</dt><dd>{{ item.value || '—' }} <t-button v-if="item.value" size="small" variant="text" :aria-label="t('feedbackAdmin.copy') + ' ' + item.label" @click="copy(item.value)"><template #icon><t-icon name="copy" /></template></t-button></dd></template></dl>
        </article>
      </t-loading>
      <template #footer><div class="drawer-footer"><t-button variant="outline" :disabled="selectedIndex <= 0 || detailLoading" @click="stepDetail(-1)">{{ t('feedbackAdmin.previous') }}</t-button><t-button variant="outline" :disabled="selectedIndex < 0 || selectedIndex >= records.length - 1 || detailLoading" @click="stepDetail(1)">{{ t('feedbackAdmin.next') }}</t-button><t-button v-if="detail" @click="openSession">{{ t('feedbackAdmin.openSession') }}</t-button></div></template>
    </t-drawer>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { MessagePlugin } from 'tdesign-vue-next'
import { marked } from 'marked'
import { useAuthStore } from '@/stores/auth'
import { sanitizeMarkdownHTML } from '@/utils/security'
import { copyToClipboard } from '@/utils/clipboard'
import { listFeedback, summarizeFAQFeedback, getFeedbackDetail, exportFeedback, type FeedbackRecord, type FAQSummary, type FeedbackParams } from '@/api/feedback'
import { feedbackDateParams, feedbackDateRange, feedbackRatio, feedbackRowKey, localDate } from './feedbackQuery'
const { t, locale } = useI18n(), route = useRoute(), router = useRouter(), auth = useAuthStore()
const allowed = computed(() => auth.hasRole('admin'))
const mode = ref('detail'), page = ref(1), loading = ref(false), exporting = ref(false), error = ref('')
const records = ref<FeedbackRecord[]>([]), summaries = ref<FAQSummary[]>([]), total = ref(0)
const stats = ref({ total: 0, likes: 0, dislikes: 0 })
const filters = reactive({ q: '', source: '', type: '', start: '', end: '', session_id: '', target_id: '', user_id: '', knowledge_base_id: '', tag_name: '', sort: 'dislikes' })
const reasons = ref<string[]>([]), applied = ref<FeedbackParams>({})
const reasonCodes = ['inaccurate', 'incomplete', 'off_topic', 'other']
const reasonLabel = (value: string) => ({ inaccurate: t('feedbackAdmin.inaccurate'), incomplete: t('feedbackAdmin.incomplete'), off_topic: t('feedbackAdmin.off_topic'), other: t('feedbackAdmin.other') })[value] || value
const voteLabel = (value: string) => value === 'like' ? t('feedbackAdmin.like') : t('feedbackAdmin.dislike')
const sourceLabel = (value: string) => value === 'faq' ? t('feedbackAdmin.faq') : t('feedbackAdmin.message')
const dateTime = (value: string) => { const d = new Date(value); return Number.isFinite(+d) ? d.toLocaleString(locale.value) : '—' }
const tableRows = computed(() => records.value.map(row => ({ ...row, row_key: feedbackRowKey(row) })))
const columns = computed(() => [
  { colKey: 'time', title: t('feedbackAdmin.time'), width: 160 }, { colKey: 'source', title: t('feedbackAdmin.source'), width: 100 },
  { colKey: 'question', title: t('feedbackAdmin.question'), minWidth: 240 }, { colKey: 'rating', title: t('feedbackAdmin.rating'), width: 170 },
  { colKey: 'user', title: t('feedbackAdmin.user'), width: 140 }, { colKey: 'operation', title: '', width: 76 },
])
const summaryColumns = computed(() => [
  { colKey: 'question', title: t('feedbackAdmin.question'), minWidth: 240 }, { colKey: 'total', title: t('feedbackAdmin.total'), width: 90 },
  { colKey: 'likes', title: t('feedbackAdmin.likes'), width: 80 }, { colKey: 'dislikes', title: t('feedbackAdmin.dislikes'), width: 80 },
  { colKey: 'ratio', title: t('feedbackAdmin.ratio'), width: 100 }, { colKey: 'latest', title: t('feedbackAdmin.latest'), width: 160 }, { colKey: 'operation', title: '', width: 96 },
])
let loadSequence = 0, detailSequence = 0
function requestParams(): FeedbackParams | null {
  const dates = feedbackDateParams(filters.start, filters.end)
  if (!dates) { MessagePlugin.warning(t('feedbackAdmin.dateError')); return null }
  const params: FeedbackParams = { ...dates, page: page.value, page_size: 20, sort: filters.sort }
  for (const key of ['q', 'source', 'type', 'session_id', 'target_id', 'user_id', 'knowledge_base_id', 'tag_name'] as const) if (filters[key]) params[key] = filters[key].trim()
  if (mode.value === 'summary') params.source = 'faq'
  if (params.source !== 'faq') { delete params.knowledge_base_id; delete params.tag_name }
  if (reasons.value.length) { params.reasons = reasons.value.join(','); params.type = 'dislike' }
  return params
}
async function apply(nextPage = 1) {
  page.value = nextPage
  const params = requestParams(); if (!params) return
  const query = Object.fromEntries(Object.entries(params).map(([key, value]) => [key, String(value)]))
  query.view = mode.value
  if (JSON.stringify(query) === JSON.stringify(route.query)) { applied.value = params; await load(); return }
  await router.replace({ query })
}
function voteChanged() { if (filters.type === 'like') reasons.value = [] }
function reasonsChanged() { if (reasons.value.length) filters.type = 'dislike' }
function preset(days: number) { const range = feedbackDateRange(days); filters.start = range.from; filters.end = range.to; void apply() }
function onlyDislikes() { filters.type = 'dislike'; void apply() }
function reset() { Object.assign(filters, { q: '', source: '', type: '', session_id: '', target_id: '', user_id: '', knowledge_base_id: '', tag_name: '', sort: 'dislikes' }); reasons.value = []; preset(7) }
function changeMode(value: string | number) { mode.value = String(value); filters.source = mode.value === 'summary' ? 'faq' : ''; filters.target_id = ''; void apply() }
function changePage(value: number) { void apply(value) }
function drillDown(row: FAQSummary) { mode.value = 'detail'; filters.source = 'faq'; filters.target_id = row.entry_id; void apply() }
async function load() {
  const seq = ++loadSequence
  if (!allowed.value) return
  loading.value = true; error.value = ''; records.value = []; summaries.value = []; total.value = 0; stats.value = { total: 0, likes: 0, dislikes: 0 }
  try {
    if (mode.value === 'summary') { const result = await summarizeFAQFeedback(applied.value); if (seq !== loadSequence) return; summaries.value = result.data.items; total.value = result.data.total; stats.value = result.data.stats }
    else { const result = await listFeedback(applied.value); if (seq !== loadSequence) return; records.value = result.data.items; total.value = result.data.stats.total; stats.value = result.data.stats }
  } catch { if (seq === loadSequence) error.value = t('feedbackAdmin.loadFailed') }
  finally { if (seq === loadSequence) loading.value = false }
}

const drawerVisible = ref(false), detailLoading = ref(false), detailError = ref(''), detail = ref<FeedbackRecord | null>(null), selected = ref<FeedbackRecord | null>(null)
const selectedIndex = computed(() => selected.value ? records.value.findIndex(r => feedbackRowKey(r) === feedbackRowKey(selected.value!)) : -1)
async function openDetail(row: FeedbackRecord) {
  selected.value = row; drawerVisible.value = true; detail.value = null; detailError.value = ''; detailLoading.value = true
  const seq = ++detailSequence
  try { const result = await getFeedbackDetail(row); if (seq === detailSequence) detail.value = result.data }
  catch { if (seq === detailSequence) detailError.value = t('feedbackAdmin.loadFailed') }
  finally { if (seq === detailSequence) detailLoading.value = false }
}
function closeDetail() { ++detailSequence; drawerVisible.value = false; detail.value = null; selected.value = null; detailLoading.value = false; detailError.value = '' }
function stepDetail(delta: number) { const row = records.value[selectedIndex.value + delta]; if (row) void openDetail(row) }
const identity = computed(() => detail.value ? [
  { label: t('feedbackAdmin.source'), value: sourceLabel(detail.value.source) }, { label: t('feedbackAdmin.user'), value: detail.value.user_id },
  { label: 'session_id', value: detail.value.session_id }, { label: detail.value.source === 'faq' ? t('feedbackAdmin.faqID') : 'request_id', value: detail.value.target_id },
  { label: detail.value.source === 'faq' ? t('feedbackAdmin.kb') : t('feedbackAdmin.messageID'), value: detail.value.source === 'faq' ? detail.value.knowledge_base_id : detail.value.message_id },
] : [])
function markdown(text: string) { return sanitizeMarkdownHTML(marked.parse(text || '', { async: false })) }
async function copy(value: string) { try { if (!await copyToClipboard(value)) throw new Error('copy failed'); MessagePlugin.success(t('feedbackAdmin.copied')) } catch { MessagePlugin.error(t('feedbackAdmin.copyFailed')) } }
function openSession() { if (detail.value) void router.push({ path: `/platform/chat/${encodeURIComponent(detail.value.session_id)}`, query: detail.value.message_id ? { focus_message: detail.value.message_id } : {} }) }
async function downloadExport() {
  exporting.value = true; const tenant = auth.effectiveTenantId
  try {
    const blob = await exportFeedback(applied.value); if (auth.effectiveTenantId !== tenant) return
    if (blob.type.includes('json')) throw new Error('export failed')
    const url = URL.createObjectURL(blob), link = document.createElement('a'); link.href = url; link.download = 'feedback.csv'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000)
  } catch { MessagePlugin.error(t('feedbackAdmin.exportFailed')) } finally { exporting.value = false }
}
watch(() => [route.query, auth.effectiveTenantId, allowed.value], () => {
  closeDetail(); ++loadSequence; loading.value = false
  records.value = []; summaries.value = []; total.value = 0; stats.value = { total: 0, likes: 0, dislikes: 0 }; applied.value = {}
  const val = (key: string) => typeof route.query[key] === 'string' ? String(route.query[key]) : ''
  mode.value = val('view') === 'summary' ? 'summary' : 'detail'
  page.value = Math.max(1, Number(val('page')) || 1)
  for (const key of ['q', 'source', 'type', 'session_id', 'target_id', 'user_id', 'knowledge_base_id', 'tag_name'] as const) filters[key] = val(key)
  filters.sort = val('sort') === 'total' ? 'total' : 'dislikes'
  reasons.value = val('reasons').split(',').filter(r => reasonCodes.includes(r))
  const defaults = feedbackDateRange(7), from = new Date(val('from')), to = new Date(val('to'))
  filters.start = Number.isFinite(+from) ? localDate(from) : defaults.from
  if (Number.isFinite(+to)) { to.setDate(to.getDate() - 1); filters.end = localDate(to) } else filters.end = defaults.to
  const params = requestParams(); if (!params) return
  applied.value = params
  if (!val('from') || !val('to')) { void apply(page.value); return }
  if (allowed.value) void load(); else { records.value = []; summaries.value = []; stats.value = { total: 0, likes: 0, dislikes: 0 }; total.value = 0 }
}, { immediate: true })
onBeforeUnmount(() => { ++loadSequence; ++detailSequence })
</script>

<style scoped lang="less">
.feedback-page { height: 100%; overflow: auto; padding: 28px 32px; color: var(--td-text-color-primary); background: var(--td-bg-color-page); }
.page-header { display: flex; justify-content: space-between; align-items: center; gap: 16px; margin-bottom: 24px; h2 { margin: 0; font-size: 22px; font-weight: 600; } p { margin: 8px 0 0; color: var(--td-text-color-secondary); } }
.stats { display: grid; grid-template-columns: repeat(3, 1fr); gap: 16px; }
.stat { text-align: left; padding: 18px 22px; border: 1px solid var(--td-component-border); border-radius: 8px; background: var(--td-bg-color-container); color: inherit; font: inherit; span { display: block; color: var(--td-text-color-secondary); font-size: 13px; } strong { display: block; font-size: 30px; font-weight: 600; font-variant-numeric: tabular-nums; margin-top: 8px; } }
.clickable { cursor: pointer; &:hover { border-color: var(--td-brand-color); } }
.scope-note { color: var(--td-text-color-secondary); font-size: 12px; margin: 10px 0 20px; }
.filters { padding: 18px 0; }.filter-row { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; margin-bottom: 12px; }.search { flex: 1; min-width: 210px; }.small-select { width: 140px; }.reason-select { width: 230px; } label { display: flex; align-items: center; gap: 8px; font-size: 12px; color: var(--td-text-color-secondary); }
input[type=date] { font: inherit; color: var(--td-text-color-primary); background: var(--td-bg-color-container); border: 1px solid var(--td-component-border); border-radius: 4px; padding: 6px; } summary { cursor: pointer; font-size: 13px; color: var(--td-text-color-secondary); }.advanced { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 12px; margin-top: 14px; label { align-items: stretch; flex-direction: column; } }
.summary-toolbar { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 14px; p { color: var(--td-text-color-secondary); font-size: 12px; } }
.results { display: block; min-height: 190px; margin: 10px 0 20px; }.data-table-shell { background: var(--td-bg-color-container); border: 1px solid var(--td-component-border); border-radius: 8px; overflow: auto; }.question { font-weight: 500; line-height: 1.6; overflow-wrap: anywhere; }.subtext { color: var(--td-text-color-secondary); font-size: 12px; line-height: 1.7; margin-top: 6px; }.time { font-size: 12px; }.user-label { overflow-wrap: anywhere; }
.feedback-detail { font-size: 14px; h3 { font-size: 16px; margin-top: 26px; } h4 { color: var(--td-text-color-secondary); font-size: 13px; font-weight: 500; margin: 20px 0 10px; } p { line-height: 1.8; } }.detail-heading { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; span { font-size: 12px; } }.reason-tags { display: flex; gap: 6px; margin-top: 12px; }.user-comment { border-radius: 6px; padding: 4px 16px; margin-top: 16px; background: var(--td-bg-color-secondarycontainer); p { white-space: pre-wrap; overflow-wrap: anywhere; } }.plain-content { white-space: pre-wrap; overflow-wrap: anywhere; }.answer { overflow-wrap: anywhere; line-height: 1.8; padding-bottom: 16px; :deep(img) { max-width: 100%; } :deep(pre) { overflow: auto; } :deep(table) { display: block; overflow: auto; } }.identity { display: grid; grid-template-columns: 105px minmax(0, 1fr); gap: 10px; border-top: 1px solid var(--td-component-border); padding-top: 18px; margin-top: 24px; font-size: 12px; dt { color: var(--td-text-color-secondary); } dd { margin: 0; overflow-wrap: anywhere; } }.faq-more,.current-faq { margin: 18px 0; white-space: pre-wrap; }.drawer-footer { display: flex; gap: 10px; flex-wrap: wrap; }
@media (max-width: 768px) { .feedback-page { padding: 18px 14px; }.page-header { align-items: flex-start; }.stats { gap: 8px; }.stat { padding: 12px; strong { font-size: 24px; } }.filter-row label { flex: 1; }.search,.reason-select { width: 100%; }.small-select { flex: 1; min-width: 120px; } }
</style>
