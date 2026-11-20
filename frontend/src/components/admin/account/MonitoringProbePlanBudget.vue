<template>
  <section class="mt-3 min-w-0 space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600" :aria-label="t(`${p}.title`)" data-testid="plan-budget">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h4 class="text-sm font-medium">{{ t(`${p}.title`) }}</h4>
      <button type="button" class="btn btn-secondary text-sm" :disabled="disabled || busy || !draft.model_id || !draft.cron_expression" data-testid="plan-budget-preview" @click="preview">{{ t(`${p}.${busy ? 'loading' : 'preview'}`) }}</button>
    </div>
    <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.boundary`) }}</p>
    <p v-if="error" role="alert" class="text-sm text-amber-800 dark:text-amber-300" data-testid="plan-budget-error">{{ t(`${p}.${error}`) }}</p>
    <div v-if="result?.plan_change" class="min-w-0 space-y-2" aria-live="polite" data-testid="plan-budget-result">
      <p class="text-sm font-medium" :class="!result.enforced || !result.can_save || partial ? 'text-amber-800 dark:text-amber-300' : 'text-primary-700 dark:text-primary-300'" data-testid="plan-budget-conclusion">{{ t(`${p}.${!result.enforced ? 'notEnforced' : !result.can_save ? 'insufficient' : partial ? 'partial' : 'fits'}`) }}</p>
      <dl class="grid min-w-0 grid-cols-2 gap-3 text-sm sm:grid-cols-4">
        <div v-for="[key, value] in metrics" :key="key" class="min-w-0"><dt class="text-xs text-gray-500">{{ t(`${p}.${key}`) }}</dt><dd class="break-all font-semibold tabular-nums" :data-testid="`plan-budget-${key}`">{{ key === 'delta' && Number(value) > 0 ? '+' : '' }}{{ value }}</dd></div>
      </dl>
      <p class="break-words text-xs text-gray-500">{{ t(`${p}.snapshot`, { revision: result.budget_revision, time: result.observed_at, start: result.schedule_window_start, end: result.schedule_window_end }) }}</p>
      <p v-if="!draft.enabled" class="text-xs">{{ t(`${p}.disabledPlan`) }}</p>
      <ul v-if="result.blocking_reasons.length || result.warnings.length" class="space-y-1 text-xs text-amber-800 dark:text-amber-300"><li v-for="reason in reasons" :key="reason">{{ t(`admin.settings.upstreamBillingProbe.budget.capacityReason.${knownReasons.has(reason) ? reason : 'unknown'}`) }}</li></ul>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getMonitoringProbeBudget, previewMonitoringProbeCoverage } from '@/api/admin/accounts'
import type { MonitoringProbeCoverage, MonitoringProbePlanDraft } from '@/types/monitoringProbeBudget'

const props = defineProps<{ draft: MonitoringProbePlanDraft; disabled?: boolean }>()
const { t } = useI18n()
const p = 'admin.scheduledTests.planBudget'
const result = ref<MonitoringProbeCoverage | null>(null), busy = ref(false)
const error = ref('')
let generation = 0, alive = true, controller: AbortController | undefined
const partial = computed(() => !!result.value && (result.value.inventory_complete !== true || result.value.plan_inventory_complete !== true ||
  result.value.candy_inventory_complete !== true || result.value.active_inventory_complete !== true || result.value.warnings.length > 0 ||
  !Array.isArray(result.value.unestimated_demand_sources) || result.value.unestimated_demand_sources.length > 0))
const metrics = computed(() => [
  ['current', result.value?.plan_change?.current_minimum_requests_per_day], ['delta', result.value?.plan_change?.delta_minimum_requests_per_day],
  ['proposed', result.value?.plan_change?.proposed_minimum_requests_per_day], ['capacity', result.value?.regular_day_capacity]
] as const)
const reasons = computed(() => [...new Set([...(result.value?.blocking_reasons ?? []), ...(result.value?.warnings ?? [])])])
const knownReasons = new Set(['runner_capacity', 'minute_capacity', 'day_capacity', 'site_capacity', 'key_capacity', 'billing_scope_unresolved', 'periodic_site_capacity', 'periodic_site_unresolved', 'periodic_key_capacity', 'periodic_key_unresolved', 'runner_no_headroom', 'invalid_interval', 'scheduled_unresolved', 'scheduled_inventory_truncated', 'candy_unresolved', 'candy_inventory_truncated', 'active_unresolved', 'active_inventory_truncated', 'active_quota_unestimated'])

function invalidate() {
  generation++; controller?.abort(); result.value = null; error.value = ''; busy.value = false
}
watch(() => JSON.stringify([props.draft, props.disabled]), invalidate, { flush: 'sync' })
onBeforeUnmount(() => { alive = false; invalidate() })

async function preview() {
  if (busy.value || props.disabled) return
  invalidate()
  const id = generation, draft = JSON.parse(JSON.stringify(props.draft)) as MonitoringProbePlanDraft
  controller = new AbortController()
  const options = { signal: controller.signal }
  busy.value = true
  try {
    const summary = await getMonitoringProbeBudget(options)
    if (!alive || id !== generation) return
    const next = await previewMonitoringProbeCoverage({ ...summary.settings, plan_draft: draft }, options)
    if (!alive || id !== generation) return
    const change = next?.plan_change, count = (v: unknown) => Number.isSafeInteger(v) && Number(v) >= 0
    if (!change || change.version !== 1 || change.plan_id !== draft.plan_id || change.account_id !== draft.account_id || change.enabled !== draft.enabled ||
      ![change.current_minimum_requests_per_day, change.proposed_minimum_requests_per_day, next.minimum_total_requests_per_day, next.regular_day_capacity, next.budget_revision].every(count) ||
      !Number.isSafeInteger(change.delta_minimum_requests_per_day) || change.proposed_minimum_requests_per_day - change.current_minimum_requests_per_day !== change.delta_minimum_requests_per_day ||
      change.proposed_minimum_requests_per_day !== next.minimum_total_requests_per_day || next.budget_revision !== summary.settings.revision ||
      typeof next.can_save !== 'boolean' || typeof next.enforced !== 'boolean' || !Array.isArray(next.blocking_reasons) || !Array.isArray(next.warnings) ||
      [...next.blocking_reasons, ...next.warnings].some(r => typeof r !== 'string') || !Number.isFinite(Date.parse(next.observed_at)) ||
      !next.schedule_window_start || !next.schedule_window_end || Date.parse(next.schedule_window_end) - Date.parse(next.schedule_window_start) !== 86400000 ||
      next.can_save !== (!next.enforced || next.blocking_reasons.length === 0)) throw new Error('unknown preview contract')
    result.value = next
  } catch (e) {
    if (alive && id === generation) {
      const status = (e as { response?: { status?: number } })?.response?.status
      error.value = status === 409 ? 'conflict' : status === 403 ? 'forbidden' : status === 400 ? 'invalid' : 'failed'
    }
  } finally {
    if (alive && id === generation) busy.value = false
  }
}
</script>
