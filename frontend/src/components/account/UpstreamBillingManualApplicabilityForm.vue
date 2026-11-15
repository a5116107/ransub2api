<template>
  <details class="mt-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="manual-applicability-form">
    <summary class="cursor-pointer text-sm font-medium">{{ t(`${prefix}.title`) }}</summary>
    <p class="input-hint mt-2">{{ t(`${prefix}.hint`) }}</p>
    <p v-if="current" class="mt-2 text-xs" data-testid="manual-current-status">{{ t(`${prefix}.${current.status}`) }}<template v-if="current.source_version"> · {{ current.source_version }}</template><template v-if="current.expires_at"> · {{ current.expires_at }}</template></p>
    <label class="mt-3 flex items-center gap-2 text-sm">
      <input v-model="editing" type="checkbox" data-testid="manual-edit" />{{ t(`${prefix}.edit`) }}
    </label>
    <fieldset v-if="editing" class="mt-3 space-y-3" :disabled="revoke">
      <label class="block text-xs">{{ t(`${prefix}.version`) }}<input v-model="version" class="input mt-1" maxlength="128" data-testid="manual-version" /></label>
      <label class="block text-xs">{{ t(`${prefix}.reason`) }}<textarea v-model="reason" class="input mt-1" maxlength="512" rows="2" data-testid="manual-reason" /></label>
      <label class="block text-xs">{{ t(`${prefix}.expiry`) }}<input v-model="expiry" type="datetime-local" class="input mt-1" data-testid="manual-expiry" /></label>
      <div v-for="(entry, index) in entries" :key="index" class="space-y-2 rounded bg-gray-50 p-3 dark:bg-dark-800" data-testid="manual-entry">
        <label class="block text-xs">{{ t(`${prefix}.model`) }}<input v-model="entry.model" class="input mt-1" maxlength="200" data-testid="manual-model" /></label>
        <label class="block text-xs">{{ t(`${prefix}.protocol`) }}
          <select v-model="entry.protocol" class="input mt-1" data-testid="manual-protocol"><option value="responses_http">Responses HTTP</option><option value="responses_ws_v2">Responses WS v2</option><option value="chat_http">Chat Completions</option></select>
        </label>
        <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
          <label class="block text-xs">{{ t(`${prefix}.tier`) }}<input v-model="entry.service_tier" class="input mt-1" data-testid="manual-tier" /></label>
          <label class="block text-xs">{{ t(`${prefix}.effort`) }}<input v-model="entry.reasoning_effort" class="input mt-1" data-testid="manual-effort" /></label>
        </div>
        <label class="block text-xs">{{ t(`${prefix}.applies`) }}
          <select v-model="entry.group_rate_applies" class="input mt-1" data-testid="manual-applies"><option :value="true">{{ t('admin.accounts.upstreamBilling.applicability.covered') }}</option><option :value="false">{{ t('admin.accounts.upstreamBilling.applicability.excluded') }}</option><option :value="null">{{ t('admin.accounts.upstreamBilling.applicability.unknown') }}</option></select>
        </label>
        <button type="button" class="text-xs text-red-600" :disabled="entries.length === 1" @click="entries.splice(index, 1)">{{ t(`${prefix}.remove`) }}</button>
      </div>
      <button type="button" class="btn btn-secondary" :disabled="entries.length >= 128" data-testid="manual-add" @click="entries.push(emptyEntry())">{{ t(`${prefix}.add`) }}</button>
      <label class="flex items-start gap-2 text-xs"><input v-model="confirmed" type="checkbox" class="mt-0.5" data-testid="manual-confirm" />{{ t(`${prefix}.confirm`) }}</label>
    </fieldset>
    <label v-if="current && current.status !== 'revoked'" class="mt-3 flex items-center gap-2 text-xs text-red-600"><input v-model="revoke" type="checkbox" data-testid="manual-revoke" />{{ t(`${prefix}.revoke`) }}</label>
  </details>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account, UpstreamBillingManualApplicabilityUpdate } from '@/types'

const props = defineProps<{ account: Account; show: boolean }>()
const { t } = useI18n()
const prefix = 'admin.accounts.upstreamBilling.manualApplicability'
const current = ref(props.account.extra?.upstream_billing_manual_applicability)
type Entry = { model: string; protocol: string; service_tier: string; reasoning_effort: string; context: string; group_rate_applies: boolean | null }
const emptyEntry = (): Entry => ({ model: '', protocol: 'responses_http', service_tier: 'unspecified', reasoning_effort: 'unspecified', context: 'all', group_rate_applies: true })
const editing = ref(false), revoke = ref(false), confirmed = ref(false)
const version = ref(''), reason = ref(''), expiry = ref('')
const entries = ref<Entry[]>([emptyEntry()])
watch([() => props.account.id, () => props.show], () => {
  // Keep the opened evidence and revision together until the form is reopened.
  // A list refresh must neither discard a draft nor authorize it with a new CAS.
  current.value = props.account.extra?.upstream_billing_manual_applicability
  editing.value = false; revoke.value = false; confirmed.value = false
  version.value = current.value?.source_version ?? ''
  reason.value = current.value?.reason ?? ''
  const date = new Date(current.value?.expires_at ?? Date.now() + 3600000)
  expiry.value = Number.isFinite(date.getTime()) ? new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16) : ''
  entries.value = current.value?.entries?.length
    ? current.value.entries.map(({ model, protocol, service_tier, reasoning_effort, context, group_rate_applies }) => ({ model, protocol, service_tier, reasoning_effort, context, group_rate_applies }))
    : [emptyEntry()]
}, { immediate: true })

function buildUpdate(): UpstreamBillingManualApplicabilityUpdate | undefined {
  const expected_revision = current.value?.revision ?? ''
  if (revoke.value) return { revoke: true, expected_revision }
  if (!editing.value) return undefined // An ordinary save never renews a TTL.
  const end = new Date(expiry.value).getTime(), duration = end - Date.now()
  if (!confirmed.value || !version.value.trim() || !reason.value.trim() || !Number.isFinite(duration) || duration <= 0 || duration > 86400000 || entries.value.some(entry => !entry.model.trim())) {
    throw new Error(t(`${prefix}.invalid`))
  }
  return { expected_revision, confirm_current_binding: true, source_version: version.value.trim(), reason: reason.value.trim(), expires_at: new Date(end).toISOString(), entries: entries.value.map(entry => ({ ...entry })) }
}
defineExpose({ buildUpdate })
</script>
