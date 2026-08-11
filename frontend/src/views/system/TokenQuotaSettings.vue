<template>
  <section class="token-quota-settings">
    <div class="section-header">
      <h2>{{ t('tokenQuotaSettings.title') }}</h2>
      <p class="section-description">{{ t('tokenQuotaSettings.description') }}</p>
    </div>

    <t-alert theme="info" :message="t('tokenQuotaSettings.identityHint')" class="identity-hint" />

    <div class="quota-card">
      <t-form ref="formRef" :data="form" :rules="rules" label-align="top" @submit.prevent>
        <div class="quota-query-row">
          <t-form-item :label="t('tokenQuotaSettings.tenantId')" name="tenantId" class="tenant-field">
            <t-input
              v-model="form.tenantId"
              :placeholder="t('tokenQuotaSettings.tenantIdPlaceholder')"
              :disabled="loading || saving"
              clearable
              @enter="loadUsers()"
            />
          </t-form-item>
          <t-form-item :label="t('tokenQuotaSettings.subjectId')" name="subjectId" class="subject-field">
            <t-input
              v-model="form.subjectId"
              :placeholder="t('tokenQuotaSettings.subjectIdPlaceholder')"
              :disabled="loading || saving"
              clearable
              @enter="loadQuota"
            />
          </t-form-item>
          <t-button variant="outline" :loading="loading" :disabled="saving" @click="loadUsers()">
            <template #icon><t-icon name="search" /></template>
            {{ t('tokenQuotaSettings.listUsers') }}
          </t-button>
          <t-button theme="primary" :loading="loading" :disabled="saving" @click="loadQuota">
            {{ t('tokenQuotaSettings.query') }}
          </t-button>
        </div>
      </t-form>
    </div>

    <div v-if="userPage" class="quota-card quota-card--users">
      <div class="quota-card__header">
        <div>
          <h3>{{ t('tokenQuotaSettings.usersTitle') }}</h3>
          <p>{{ t('tokenQuotaSettings.usersDescription') }}</p>
        </div>
      </div>
      <t-table
        row-key="external_user_id"
        :data="userPage.items"
        :columns="userColumns"
        :hover="true"
        @row-click="selectUser"
      >
        <template #external_user_id="{ row }">
          <span class="quota-user-id">{{ row.external_user_id }}</span>
        </template>
        <template #daily="{ row }">
          {{ formatTokenCount(row.quota.daily?.total_tokens ?? 0) }}
        </template>
        <template #monthly="{ row }">
          {{ formatTokenCount(row.quota.monthly?.total_tokens ?? 0) }}
        </template>
        <template #override="{ row }">
          <t-tag :theme="row.quota.override ? 'success' : 'default'" variant="light">
            {{ row.quota.override ? t('tokenQuotaSettings.overridden') : t('tokenQuotaSettings.inheriting') }}
          </t-tag>
        </template>
      </t-table>
      <div v-if="userPage.total > userPage.page_size" class="quota-user-pagination">
        <t-button variant="text" :disabled="loading || userPage.page <= 1" @click="loadUsers(userPage.page - 1)">
          {{ t('tokenQuotaSettings.previousPage') }}
        </t-button>
        <span>{{ t('tokenQuotaSettings.pageInfo', { page: userPage.page, total: userPage.total }) }}</span>
        <t-button
          variant="text"
          :disabled="loading || userPage.page * userPage.page_size >= userPage.total"
          @click="loadUsers(userPage.page + 1)"
        >
          {{ t('tokenQuotaSettings.nextPage') }}
        </t-button>
      </div>
    </div>

    <template v-if="snapshot">
      <div class="quota-summary">
        <div class="quota-summary__item">
          <span>{{ t('tokenQuotaSettings.dailyUsage') }}</span>
          <strong>{{ formatTokenCount(snapshot.daily?.total_tokens ?? 0) }}</strong>
          <small>{{ limitText(snapshot.limits.daily_token_limit) }}</small>
        </div>
        <div class="quota-summary__item">
          <span>{{ t('tokenQuotaSettings.monthlyUsage') }}</span>
          <strong>{{ formatTokenCount(snapshot.monthly?.total_tokens ?? 0) }}</strong>
          <small>{{ limitText(snapshot.limits.monthly_token_limit) }}</small>
        </div>
        <div class="quota-summary__item quota-summary__item--reserved">
          <span>{{ t('tokenQuotaSettings.reservedTokens') }}</span>
          <strong>{{ formatTokenCount(reservedTokens) }}</strong>
          <small>{{ t('tokenQuotaSettings.reservedHint') }}</small>
        </div>
      </div>

      <div class="quota-card quota-card--override">
        <div class="quota-card__header">
          <div>
            <h3>{{ t('tokenQuotaSettings.overrideTitle') }}</h3>
            <p>{{ t('tokenQuotaSettings.overrideDescription') }}</p>
          </div>
          <t-tag :theme="snapshot.override ? 'success' : 'default'" variant="light">
            {{ snapshot.override ? t('tokenQuotaSettings.overridden') : t('tokenQuotaSettings.inheriting') }}
          </t-tag>
        </div>

        <t-form ref="overrideFormRef" :data="form" :rules="rules" label-align="top" @submit.prevent>
          <div class="quota-limit-fields">
            <t-form-item :label="t('tokenQuotaSettings.dailyLimit')" name="dailyTokenLimit">
              <t-input-number
                v-model="form.dailyTokenLimit"
                :min="0"
                :decimal-places="0"
                :disabled="saving"
                clearable
                :placeholder="t('tokenQuotaSettings.inheritPlaceholder')"
              />
            </t-form-item>
            <t-form-item :label="t('tokenQuotaSettings.monthlyLimit')" name="monthlyTokenLimit">
              <t-input-number
                v-model="form.monthlyTokenLimit"
                :min="0"
                :decimal-places="0"
                :disabled="saving"
                clearable
                :placeholder="t('tokenQuotaSettings.inheritPlaceholder')"
              />
            </t-form-item>
          </div>
          <p class="quota-form-hint">{{ t('tokenQuotaSettings.limitHint') }}</p>
          <div class="quota-actions">
            <t-popconfirm
              v-if="snapshot.override"
              :content="t('tokenQuotaSettings.resetConfirm')"
              :confirm-btn="{ content: t('tokenQuotaSettings.reset'), theme: 'warning' }"
              :cancel-btn="{ content: t('common.cancel') }"
              placement="left"
              @confirm="resetOverride"
            >
              <t-button variant="text" theme="warning" :disabled="saving">
                <template #icon><t-icon name="refresh" /></template>
                {{ t('tokenQuotaSettings.reset') }}
              </t-button>
            </t-popconfirm>
            <t-button theme="primary" :loading="saving" @click="saveOverride">
              {{ t('tokenQuotaSettings.save') }}
            </t-button>
          </div>
        </t-form>
      </div>
    </template>

    <div v-else-if="queried && !userPage?.items.length" class="empty-state">
      <t-icon name="info-circle" size="24px" />
      <span>{{ t('tokenQuotaSettings.noData') }}</span>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import type { FormInstanceFunctions, FormRule } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import {
  deleteUserTokenQuota,
  getUserTokenQuota,
  listTenantTokenQuotaUsers,
  updateUserTokenQuota,
  type TokenQuotaUser,
  type TokenQuotaUserPage,
  type TokenQuotaUsageSnapshot,
} from '@/api/system'

const { t } = useI18n()

const formRef = ref<FormInstanceFunctions>()
const overrideFormRef = ref<FormInstanceFunctions>()
const loading = ref(false)
const saving = ref(false)
const queried = ref(false)
const snapshot = ref<TokenQuotaUsageSnapshot>()
const userPage = ref<TokenQuotaUserPage>()
const form = reactive({
  tenantId: '',
  subjectId: '',
  dailyTokenLimit: null as number | null,
  monthlyTokenLimit: null as number | null,
})
const rules: Record<string, FormRule[]> = {
  tenantId: [
    { required: true, message: t('tokenQuotaSettings.tenantIdRequired'), trigger: 'blur' },
    { pattern: /^\d+$/, message: t('tokenQuotaSettings.tenantIdInvalid'), trigger: 'blur' },
  ],
}

// The workspace + external user ID pair that produced `snapshot`. Writes reuse
// it rather than the live form so an edit typed after a query cannot be applied
// to a different subject than the one on screen.
const queriedSubject = ref<{ tenantId: string; externalUserId: string }>()

const userColumns = computed(() => [
  { colKey: 'external_user_id', title: t('tokenQuotaSettings.subjectId'), minWidth: 220 },
  { colKey: 'daily', title: t('tokenQuotaSettings.dailyUsage'), width: 150 },
  { colKey: 'monthly', title: t('tokenQuotaSettings.monthlyUsage'), width: 150 },
  { colKey: 'override', title: t('tokenQuotaSettings.overrideTitle'), width: 170 },
])

const reservedTokens = computed(() =>
  snapshot.value?.daily?.reserved_tokens ?? 0,
)

watch(
  () => form.tenantId,
  () => {
    snapshot.value = undefined
    userPage.value = undefined
    queriedSubject.value = undefined
    queried.value = false
    form.subjectId = ''
    form.dailyTokenLimit = null
    form.monthlyTokenLimit = null
  },
)

function updateFormFromSnapshot(current: TokenQuotaUsageSnapshot) {
  form.dailyTokenLimit = current.override?.daily_token_limit ?? null
  form.monthlyTokenLimit = current.override?.monthly_token_limit ?? null
}

function formatTokenCount(value: number): string {
  return new Intl.NumberFormat().format(value)
}

function limitText(limit: number): string {
  if (limit === 0) return t('tokenQuotaSettings.unlimited')
  return t('tokenQuotaSettings.limitValue', { value: formatTokenCount(limit) })
}

async function validateTenant(): Promise<boolean> {
  const result = await formRef.value?.validate?.()
  return result === true
}

async function loadUsers(page: number | Event = 1, selectedExternalUserID?: string) {
  if (loading.value || !(await validateTenant())) return

  const tenantId = form.tenantId.trim()
  const currentPage = typeof page === 'number' && Number.isSafeInteger(page) && page > 0 ? page : 1
  loading.value = true
  queried.value = true
  try {
    const current = await listTenantTokenQuotaUsers(tenantId, currentPage)
    userPage.value = current
    snapshot.value = undefined
    queriedSubject.value = undefined
    form.dailyTokenLimit = null
    form.monthlyTokenLimit = null
    const selected = current.items.find((item) => item.external_user_id === selectedExternalUserID)
    if (selected) selectUser({ row: selected })
  } catch (err: any) {
    userPage.value = undefined
    MessagePlugin.error(err?.message || t('tokenQuotaSettings.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function loadQuota() {
  if (loading.value || !(await validateTenant())) return

  const tenantId = form.tenantId.trim()
  const externalUserId = form.subjectId.trim()
  if (!externalUserId) {
    MessagePlugin.warning(t('tokenQuotaSettings.subjectIdRequired'))
    return
  }

  loading.value = true
  queried.value = true
  snapshot.value = undefined
  queriedSubject.value = undefined
  form.dailyTokenLimit = null
  form.monthlyTokenLimit = null
  try {
    const current = await getUserTokenQuota(tenantId, externalUserId)
    snapshot.value = current
    queriedSubject.value = { tenantId, externalUserId }
    updateFormFromSnapshot(current)
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('tokenQuotaSettings.loadFailed'))
  } finally {
    loading.value = false
  }
}

function selectUser({ row }: { row: TokenQuotaUser }) {
  const tenantId = form.tenantId.trim()
  form.subjectId = row.external_user_id
  snapshot.value = row.quota
  queriedSubject.value = { tenantId, externalUserId: row.external_user_id }
  updateFormFromSnapshot(row.quota)
}

async function saveOverride() {
  const subject = queriedSubject.value
  if (saving.value || !subject) return
  if (form.dailyTokenLimit === null && form.monthlyTokenLimit === null) {
    MessagePlugin.warning(t('tokenQuotaSettings.overrideRequired'))
    return
  }

  saving.value = true
  try {
    await updateUserTokenQuota(subject.tenantId, subject.externalUserId, {
      daily_token_limit: form.dailyTokenLimit ?? undefined,
      monthly_token_limit: form.monthlyTokenLimit ?? undefined,
    })
    await loadUsers(userPage.value?.page ?? 1, subject.externalUserId)
    if (!queriedSubject.value) await loadQuota()
    MessagePlugin.success(t('tokenQuotaSettings.saveSuccess'))
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('tokenQuotaSettings.saveFailed'))
  } finally {
    saving.value = false
  }
}

async function resetOverride() {
  const subject = queriedSubject.value
  if (saving.value || !subject) return

  saving.value = true
  try {
    await deleteUserTokenQuota(subject.tenantId, subject.externalUserId)
    await loadUsers(userPage.value?.page ?? 1, subject.externalUserId)
    if (!queriedSubject.value) await loadQuota()
    MessagePlugin.success(t('tokenQuotaSettings.resetSuccess'))
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('tokenQuotaSettings.resetFailed'))
  } finally {
    saving.value = false
  }
}
</script>

<style lang="less" scoped>
.token-quota-settings {
  width: 100%;
}

.section-header {
  margin-bottom: 24px;

  h2 {
    margin: 0 0 8px;
    font-size: 20px;
    font-weight: 600;
    color: var(--td-text-color-primary);
  }
}

.section-description,
.quota-card__header p,
.quota-form-hint {
  margin: 0;
  font-size: 13px;
  line-height: 1.5;
  color: var(--td-text-color-secondary);
}

.identity-hint {
  margin-bottom: 16px;
}

.quota-card {
  padding: 20px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
}

.quota-card--override {
  margin-top: 16px;
}

.quota-card--users {
  margin-top: 16px;
}

.quota-query-row {
  display: flex;
  align-items: flex-end;
  gap: 12px;
}

.tenant-field {
  flex: 0 0 180px;
  margin-bottom: 0;
}

.subject-field {
  flex: 1;
  margin-bottom: 0;
}

.quota-summary {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 12px;
  margin-top: 16px;
}

.quota-summary__item {
  display: flex;
  flex-direction: column;
  min-height: 106px;
  padding: 16px;
  border-radius: 8px;
  background: var(--td-bg-color-secondarycontainer);

  span,
  small {
    font-size: 12px;
    color: var(--td-text-color-secondary);
  }

  strong {
    margin: 8px 0 4px;
    font-size: 22px;
    line-height: 1;
    color: var(--td-text-color-primary);
  }
}

.quota-summary__item--reserved strong {
  color: var(--td-warning-color);
}

.quota-card__header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 20px;

  h3 {
    margin: 0 0 4px;
    font-size: 15px;
    font-weight: 500;
    color: var(--td-text-color-primary);
  }
}

.quota-limit-fields {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px;
}

.quota-limit-fields :deep(.t-input-number) {
  width: 100%;
}

.quota-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 16px;
}

.quota-user-id {
  font-family: var(--td-font-family-mono, monospace);
}

.quota-user-pagination {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 12px;
  font-size: 13px;
  color: var(--td-text-color-secondary);
}

.empty-state {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 60px 0;
  color: var(--td-text-color-placeholder);
  font-size: 13px;
}

@media (max-width: 860px) {
  .quota-query-row,
  .quota-card__header {
    align-items: stretch;
    flex-direction: column;
  }

  .tenant-field {
    flex: 1;
  }

  .quota-summary,
  .quota-limit-fields {
    grid-template-columns: 1fr;
  }
}
</style>
