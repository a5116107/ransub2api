<template>
  <section class="space-y-4 border-t border-gray-100 pt-5 dark:border-dark-700" data-testid="monitoring-probe-budget">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h3 class="font-semibold text-gray-900 dark:text-white">{{ t(`${prefix}.title`) }}</h3>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading || saving || previewing" data-testid="probe-budget-refresh" @click="load">{{ t('common.refresh') }}</button>
    </div>
    <p class="text-sm text-gray-600 dark:text-gray-300">{{ t(`${prefix}.scopeHint`) }}</p>
    <p v-if="loading" class="text-sm text-gray-500" role="status">{{ t('common.loading') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ error }}</p>
    <p v-if="notice" role="status" class="text-sm text-emerald-700 dark:text-emerald-300">{{ notice }}</p>
    <template v-if="summary">
      <p class="text-sm font-medium text-gray-800 dark:text-gray-200" data-testid="probe-budget-mode">{{ t(`${prefix}.${summary.settings.enabled ? 'enforced' : 'observeOnly'}`) }}</p>
      <div class="grid gap-4 sm:grid-cols-3" data-testid="probe-budget-usage">
        <div v-for="counter in counters" :key="counter.label" class="min-w-0">
          <p class="text-sm text-gray-700 dark:text-gray-200">{{ t(`${prefix}.${counter.label}`) }}</p>
          <p class="font-mono text-sm text-gray-900 dark:text-white">{{ counter.used }} / {{ counter.limit }}</p>
          <progress class="h-2 w-full accent-blue-600" :aria-label="t(`${prefix}.${counter.label}`)" :value="counter.used" :max="counter.limit" />
        </div>
      </div>
      <div class="grid gap-4 sm:grid-cols-2" data-testid="probe-budget-tokens">
        <div v-for="counter in tokenCounters" :key="counter.label" class="min-w-0">
          <p class="text-sm text-gray-700 dark:text-gray-200">{{ t(`${prefix}.${counter.label}`) }}</p>
          <p class="font-mono text-sm text-gray-900 dark:text-white">{{ displayUsage(counter.used) }} / {{ counter.limit > 0 ? counter.limit : t(`${prefix}.unlimited`) }}</p>
          <progress v-if="counter.limit > 0 && counter.used !== undefined" class="h-2 w-full accent-blue-600" :aria-label="t(`${prefix}.${counter.label}`)" :value="counter.used" :max="counter.limit" />
        </div>
      </div>
      <p class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-200" data-testid="probe-budget-coverage">{{ t(`${prefix}.usageCoverage`, { reported: displayUsage(summary.usage_reported), unknown: displayUsage(summary.usage_unknown), pending: displayUsage(summary.usage_pending) }) }}</p>
      <p class="text-xs text-gray-600 dark:text-gray-300">{{ t(`${prefix}.observedAt`, { value: summary.observed_at }) }}</p>
    </template>
    <fieldset v-if="loaded" :disabled="loading || saving || previewing" class="space-y-4">
      <div class="flex items-center justify-between gap-4">
        <label id="probe-budget-enabled-label" class="font-medium text-gray-900 dark:text-white">{{ t(`${prefix}.enabled`) }}</label>
        <Toggle v-model="form.enabled" aria-labelledby="probe-budget-enabled-label" data-testid="probe-budget-enabled" />
      </div>
      <div class="grid gap-4 sm:grid-cols-2">
        <div v-for="field in fields" :key="field.key" class="min-w-0">
          <label :for="`probe-budget-${field.key}`" class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t(`${prefix}.${field.key}`) }}</label>
          <input :id="`probe-budget-${field.key}`" v-model.number="form[field.key]" type="number" :min="field.min" :max="field.max" step="1" class="input w-full" />
        </div>
      </div>
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ t(`${prefix}.reserveHint`) }}</p>
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ t(`${prefix}.tokenHint`) }}</p>
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ t(`${prefix}.deferralHint`) }}</p>
      <div class="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-950 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-100" data-testid="probe-budget-capacity" aria-live="polite">
        <h4 class="font-semibold">{{ t(`${prefix}.capacityTitle`) }}</h4>
        <template v-if="coverage && coverageMatches">
          <p class="mt-2 font-medium" data-testid="probe-budget-capacity-verdict">{{ t(`${prefix}.${coverage.blocking_reasons.length ? 'capacityExceeded' : !coverage.billing_enabled && coverage.account_plans === 0 && coverage.group_plans === 0 && coverage.candy_probes === 0 && coverage.candy_inventory_complete === true && coverage.active_monitors === 0 && coverage.active_inventory_complete === true ? 'capacityInactive' : 'capacityPartial'}`) }}</p>
          <dl class="mt-3 grid gap-3 sm:grid-cols-2">
            <div><dt>{{ t(`${prefix}.capacityTargets`) }}</dt><dd class="font-mono">{{ coverage.inventory_complete ? '' : '≥ ' }}{{ coverage.accounts }} / {{ coverage.interval_minutes }} {{ t(`${prefix}.capacityMinutes`) }}</dd></div>
            <div><dt>{{ t(`${prefix}.capacitySweep`) }}</dt><dd class="font-mono">≥ {{ coverage.minimum_requests_per_sweep }}</dd></div>
            <div><dt>{{ t(`${prefix}.capacityDay`) }}</dt><dd class="font-mono">≥ {{ coverage.minimum_requests_per_day }}</dd></div>
            <div><dt>{{ t(`${prefix}.capacityRegular`) }}</dt><dd class="font-mono">{{ coverage.regular_minute_capacity }} / {{ coverage.regular_day_capacity }}</dd></div>
          </dl>
          <p class="mt-3">{{ t(`${prefix}.capacityBatches`, { batches: coverage.minimum_sweep_batches, rate: coverage.runner_accounts_per_minute }) }}</p>
          <p class="mt-2">{{ t(`${prefix}.capacityInventory`, { newapi: coverage.new_api_accounts, unknown: coverage.unresolved_accounts }) }}</p>
          <div class="mt-4 border-t border-amber-300 pt-3 dark:border-amber-800" data-testid="probe-budget-scheduled-capacity">
            <h5 class="font-semibold">{{ t(`${prefix}.capacitySchedules`) }}</h5>
            <p class="mt-2">{{ t(`${prefix}.capacityPlanInventory`, { accounts: displayUsage(coverage.account_plans), groups: displayUsage(coverage.group_plans), unknown: displayUsage(coverage.unresolved_plans) }) }}</p>
            <p class="mt-2 break-words text-xs">{{ t(`${prefix}.capacityScheduleWindow`, { start: displayWindow(coverage.schedule_window_start), end: displayWindow(coverage.schedule_window_end) }) }}</p>
            <dl class="mt-3 grid gap-3 sm:grid-cols-2">
              <div><dt>{{ t(`${prefix}.capacityScheduleRuns`) }}</dt><dd class="font-mono">{{ displayUsage(coverage.scheduled_runs) }}</dd></div>
              <div><dt>{{ t(`${prefix}.capacityScheduleSamples`) }}</dt><dd class="font-mono">{{ displayUsage(coverage.scheduled_samples) }}</dd></div>
              <div><dt>{{ t(`${prefix}.capacityJudgeCalls`) }}</dt><dd class="font-mono">{{ displayUsage(coverage.minimum_judge_requests) }}</dd></div>
              <div><dt>{{ t(`${prefix}.capacityScheduleCalls`) }}</dt><dd class="font-mono">{{ displayUsage(coverage.minimum_scheduled_requests) }}</dd></div>
              <div class="sm:col-span-2" data-testid="probe-budget-candy-capacity"><dt>{{ t(`${prefix}.capacityCandy`) }}</dt><dd>{{ t(`${prefix}.capacityCandyInventory`, { probes: displayUsage(coverage.candy_probes), unknown: displayUsage(coverage.unresolved_candy_probes) }) }}</dd><dd class="font-mono">{{ t(`${prefix}.capacityCandyCalls`, { calls: displayUsage(coverage.minimum_candy_requests) }) }}</dd></div>
              <div class="sm:col-span-2" data-testid="probe-budget-active-capacity">
                <dt>{{ t(`${prefix}.capacityActive`) }}</dt>
                <dd>{{ t(`${prefix}.capacityActiveInventory`, { monitors: displayUsage(coverage.active_monitors), quota: displayUsage(coverage.active_quota_monitors), unknown: displayUsage(coverage.unresolved_active_monitors) }) }}</dd>
                <dd class="font-mono">{{ t(`${prefix}.capacityActiveCalls`, { runs: displayUsage(coverage.active_monitor_runs), calls: displayUsage(coverage.minimum_active_requests) }) }}</dd>
                <dd class="mt-1 text-xs">{{ t(`${prefix}.capacityActiveLimits`) }}</dd>
              </div>
              <div class="font-semibold sm:col-span-2"><dt>{{ t(`${prefix}.capacityTotalCalls`) }}</dt><dd class="font-mono" data-testid="probe-budget-total-calls">{{ displayUsage(coverage.minimum_total_requests_per_day) }} / {{ coverage.regular_day_capacity }}</dd></div>
            </dl>
            <p v-if="coverage.plan_inventory_complete !== true" class="mt-2">{{ t(`${prefix}.capacityPlansUnknown`) }}</p>
            <p v-if="coverage.candy_inventory_complete !== true" class="mt-2">{{ t(`${prefix}.capacityCandyUnknown`) }}</p>
            <p v-if="coverage.active_inventory_complete !== true" class="mt-2">{{ t(`${prefix}.capacityActiveUnknown`) }}</p>
          </div>
          <ul v-if="coverage.blocking_reasons.length || coverage.warnings.length" class="mt-2 list-inside list-disc space-y-1" data-testid="probe-budget-capacity-reasons">
            <li v-for="reason in [...coverage.blocking_reasons, ...coverage.warnings]" :key="reason">{{ capacityReason(reason) }}</li>
          </ul>
          <p v-if="!coverage.enforced" class="mt-2 font-medium">{{ t(`${prefix}.capacityObserve`) }}</p>
          <p v-if="!coverage.inventory_complete" class="mt-2">{{ t(`${prefix}.capacityTruncated`) }}</p>
          <p class="mt-2 text-xs">{{ t(`${prefix}.observedAt`, { value: coverage.observed_at }) }}</p>
        </template>
        <p v-else class="mt-2" data-testid="probe-budget-capacity-stale">{{ t(`${prefix}.capacityStale`) }}</p>
        <p class="mt-3">{{ t(`${prefix}.capacityLimits`) }}</p>
      </div>
      <div class="flex flex-wrap justify-end gap-3 border-t border-gray-100 pt-4 dark:border-dark-700">
        <button type="button" class="btn btn-secondary btn-sm" data-testid="probe-budget-preview" @click="preview">{{ previewing ? t('common.loading') : t(`${prefix}.capacityPreview`) }}</button>
        <button type="button" class="btn btn-primary btn-sm" data-testid="probe-budget-save" @click="save">{{ saving ? t('common.saving') : t('common.save') }}</button>
      </div>
    </fieldset>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import Toggle from '@/components/common/Toggle.vue'
import type { MonitoringProbeBudgetSummary, MonitoringProbeCoverage } from '@/types/monitoringProbeBudget'

const { t } = useI18n()
const prefix = 'admin.settings.upstreamBillingProbe.budget'
const form = reactive({ enabled: false, revision: 0, requests_per_minute: 600, requests_per_day: 100000,
  tokens_per_minute: 0, tokens_per_day: 0, site_tokens_per_minute: 0, key_tokens_per_minute: 0,
  site_requests_per_minute: 600, key_requests_per_minute: 200, concurrency: 16,
  site_concurrency: 4, key_concurrency: 2, recovery_reserve_percent: 20 })
const fields = [
  { key: 'requests_per_minute', min: 1, max: 1000000 },
  { key: 'requests_per_day', min: 1, max: 1000000000 },
  { key: 'site_requests_per_minute', min: 1, max: 1000000 },
  { key: 'key_requests_per_minute', min: 1, max: 1000000 },
  { key: 'tokens_per_minute', min: 0, max: 1000000000000 },
  { key: 'tokens_per_day', min: 0, max: 1000000000000 },
  { key: 'site_tokens_per_minute', min: 0, max: 1000000000000 },
  { key: 'key_tokens_per_minute', min: 0, max: 1000000000000 },
  { key: 'concurrency', min: 1, max: 1024 },
  { key: 'site_concurrency', min: 1, max: 1024 },
  { key: 'key_concurrency', min: 1, max: 1024 },
  { key: 'recovery_reserve_percent', min: 0, max: 80 },
] as const
const summary = ref<MonitoringProbeBudgetSummary | null>(null)
const loading = ref(false)
const saving = ref(false)
const loaded = ref(false)
const previewing = ref(false)
const coverage = ref<MonitoringProbeCoverage | null>(null)
const coverageKey = ref('')
const coverageMatches = computed(() => coverageKey.value === JSON.stringify(form))
const error = ref('')
const notice = ref('')
const counters = computed(() => summary.value ? [
  { label: 'minuteUsage', used: summary.value.minute_used, limit: summary.value.settings.requests_per_minute },
  { label: 'dayUsage', used: summary.value.day_used, limit: summary.value.settings.requests_per_day },
  { label: 'activeUsage', used: summary.value.active_requests, limit: summary.value.settings.concurrency },
] : [])
const tokenCounters = computed(() => summary.value ? [
  { label: 'minuteTokens', used: summary.value.minute_tokens, limit: summary.value.settings.tokens_per_minute ?? 0 },
  { label: 'dayTokens', used: summary.value.day_tokens, limit: summary.value.settings.tokens_per_day ?? 0 },
] : [])
function displayUsage(value: number | undefined) {
  return Number.isSafeInteger(value) && Number(value) >= 0 ? value : t(`${prefix}.unknownUsage`)
}

function capacityReason(reason: string) {
  const known = ['runner_capacity', 'minute_capacity', 'day_capacity', 'runner_no_headroom', 'invalid_interval', 'scheduled_unresolved', 'scheduled_inventory_truncated', 'candy_unresolved', 'candy_inventory_truncated', 'active_unresolved', 'active_inventory_truncated', 'active_quota_unestimated']
  return t(`${prefix}.capacityReason.${known.includes(reason) ? reason : 'unknown'}`)
}

function displayWindow(value: string | undefined) {
  return value && Number.isFinite(Date.parse(value)) ? value : t(`${prefix}.unknownUsage`)
}

function validateForm() {
  if (fields.some(({ key, min, max }) => !Number.isInteger(form[key]) || form[key] < min || form[key] > max)) {
    error.value = t(`${prefix}.invalid`)
    return false
  }
  return true
}

async function compileCoverage() {
  const draft = { ...form }
  coverage.value = await adminAPI.accounts.previewMonitoringProbeCoverage(draft)
  coverageKey.value = JSON.stringify(draft)
  return coverage.value
}

async function preview() {
  if (!loaded.value || loading.value || saving.value || previewing.value || !validateForm()) return
  previewing.value = true
  error.value = notice.value = ''
  try { await compileCoverage() }
  catch (cause: unknown) {
    coverage.value = null
    error.value = t(`${prefix}.${(cause as { status?: number })?.status === 409 ? 'conflict' : 'capacityPreviewFailed'}`)
  } finally { previewing.value = false }
}

async function load() {
  if (loading.value || saving.value || previewing.value) return
  loading.value = true
  error.value = notice.value = ''
  try {
    summary.value = await adminAPI.accounts.getMonitoringProbeBudget()
    Object.assign(form, summary.value.settings)
    coverage.value = summary.value.coverage ?? null
    coverageKey.value = JSON.stringify(form)
    loaded.value = true
  } catch {
    error.value = t(`${prefix}.loadFailed`)
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!loaded.value || loading.value || saving.value || previewing.value) return
  notice.value = ''
  if (!validateForm()) return
  saving.value = true
  error.value = ''
  let previewCompleted = false
  try {
    const capacity = await compileCoverage()
    previewCompleted = true
    if (!capacity.can_save) {
      error.value = t(`${prefix}.capacityBlocked`)
      return
    }
    Object.assign(form, await adminAPI.accounts.updateMonitoringProbeBudget({ ...form }))
    notice.value = t(`${prefix}.saved`)
    // A failed read must not turn a completed policy write into a failed save.
    try {
      summary.value = await adminAPI.accounts.getMonitoringProbeBudget()
      coverage.value = summary.value.coverage ?? null
      coverageKey.value = JSON.stringify(form)
    }
    catch { error.value = t(`${prefix}.summaryFailed`) }
  } catch (cause: unknown) {
    const failure = cause as { status?: number; reason?: string }
    const changedCapacity = failure?.reason === 'MONITORING_PROBE_COVERAGE_IMPOSSIBLE'
    if (changedCapacity || !previewCompleted) coverage.value = null
    error.value = t(`${prefix}.${failure?.status === 409 ? 'conflict' : changedCapacity ? 'capacityBlocked' : !previewCompleted ? 'capacityPreviewFailed' : 'saveFailed'}`)
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>
