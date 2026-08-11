import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { SYSTEM_ADMIN_SETTINGS_SECTIONS } from '../../config/settingsAccess.ts'
import enUS from '../../i18n/locales/en-US.ts'
import koKR from '../../i18n/locales/ko-KR.ts'
import ruRU from '../../i18n/locales/ru-RU.ts'
import zhCN from '../../i18n/locales/zh-CN.ts'

const componentPath = fileURLToPath(new URL('./TokenQuotaSettings.vue', import.meta.url))
const settingsModalPath = fileURLToPath(new URL('../settings/Settings.vue', import.meta.url))
const systemAPIPath = fileURLToPath(new URL('../../api/system/index.ts', import.meta.url))

test('exposes external-user token quota management through system administration', () => {
  assert.ok(SYSTEM_ADMIN_SETTINGS_SECTIONS.has('token-quotas'))

  const component = readFileSync(componentPath, 'utf8')
  assert.match(component, /getUserTokenQuota/)
  assert.match(component, /updateUserTokenQuota/)
  assert.match(component, /deleteUserTokenQuota/)
  assert.match(component, /daily_token_limit/)
  assert.match(component, /monthly_token_limit/)
  assert.match(component, /queriedSubject\.value/)
  assert.doesNotMatch(component, /daily\?\.reserved_tokens[^\n]*\+[^\n]*monthly\?\.reserved_tokens/)

  // Quota is keyed per workspace + external user ID, so the workspace must be
  // part of every query and write.
  assert.match(component, /form\.tenantId/)

  const settingsModal = readFileSync(settingsModalPath, 'utf8')
  assert.match(settingsModal, /<TokenQuotaSettings\s*\/>/)
  assert.match(settingsModal, /key: 'token-quotas'/)

  const systemAPI = readFileSync(systemAPIPath, 'utf8')
  assert.match(systemAPI, /export async function getUserTokenQuota/)
  assert.match(systemAPI, /export async function updateUserTokenQuota/)
  assert.match(systemAPI, /export async function deleteUserTokenQuota/)
  assert.match(systemAPI, /tenant_id=\$\{encodeURIComponent/)
})

test('translates every token quota control and permission in every supported locale', () => {
  const quotaKeys = [
    'title', 'description', 'identityHint', 'tenantId', 'tenantIdPlaceholder', 'tenantIdRequired', 'tenantIdInvalid',
    'subjectId', 'subjectIdPlaceholder', 'subjectIdRequired', 'query', 'listUsers', 'usersTitle', 'usersDescription', 'previousPage', 'nextPage', 'pageInfo', 'dailyUsage', 'monthlyUsage', 'reservedTokens',
    'reservedHint', 'unlimited', 'limitValue', 'overrideTitle', 'overrideDescription', 'overridden', 'inheriting',
    'dailyLimit', 'monthlyLimit', 'inheritPlaceholder', 'limitHint', 'reset', 'resetConfirm', 'save', 'noData',
    'loadFailed', 'overrideRequired', 'saveSuccess', 'saveFailed', 'resetSuccess', 'resetFailed',
  ]
  const settingsPaths = [
    'settings.tokenQuotas',
    'system.globalSettings.keyLabels.token_quota.default_daily_limit',
    'system.globalSettings.keyLabels.token_quota.default_monthly_limit',
    'system.globalSettings.keyLabels.token_quota.max_completion_tokens',
    'system.globalSettings.keyDescriptions.token_quota.default_daily_limit',
    'system.globalSettings.keyDescriptions.token_quota.default_monthly_limit',
    'system.globalSettings.keyDescriptions.token_quota.max_completion_tokens',
    'platformApiKeys.capabilities.tokenQuotaRead',
    'platformApiKeys.capabilities.tokenQuotaManage',
    'platformApiKeys.capabilityHints.tokenQuotaRead',
    'platformApiKeys.capabilityHints.tokenQuotaManage',
  ]

  for (const locale of [zhCN, enUS, koKR, ruRU]) {
    const messages = locale as Record<string, any>
    for (const key of quotaKeys) {
      assert.equal(typeof messages.tokenQuotaSettings?.[key], 'string', key)
      assert.notEqual(messages.tokenQuotaSettings[key].trim(), '', key)
    }
    for (const path of settingsPaths) {
      const value = path.split('.').reduce<any>((current, key) => current?.[key], messages)
      assert.equal(typeof value, 'string', path)
      assert.notEqual(value.trim(), '', path)
    }
  }
})

test('lists observed external users and supports preconfiguring a new external user', () => {
  const component = readFileSync(componentPath, 'utf8')
  const systemAPI = readFileSync(systemAPIPath, 'utf8')

  assert.match(component, /listTenantTokenQuotaUsers/)
  assert.match(component, /@row-click="selectUser"/)
  assert.match(component, /name="subjectId"/)
  assert.match(component, /getUserTokenQuota/)
  assert.match(component, /@click="loadQuota"/)
  assert.match(systemAPI, /export async function listTenantTokenQuotaUsers/)
  assert.match(systemAPI, /\/token-quotas\/users\?tenant_id=/)
})

test('lists users with a numeric page when triggered by a UI event', () => {
  const component = readFileSync(componentPath, 'utf8')

  assert.match(component, /@enter="loadUsers\(\)"/)
  assert.match(component, /@click="loadUsers\(\)"/)
  assert.match(component, /async function loadUsers\(page: number \| Event = 1/)
  assert.match(component, /listTenantTokenQuotaUsers\(tenantId, currentPage\)/)
})
