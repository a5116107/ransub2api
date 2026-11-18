<template>
  <div class="space-y-1 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-200" role="status">
    <p class="font-semibold">{{ pluginUnmetered ? t('modelTrace.pluginMeteringDeferred') : title }}</p>
    <p>{{ t(reasonKey) }} · {{ scopeLabel || t(`modelTrace.budgetScopes.${scope}`) }}</p>
    <p v-if="pluginUnmetered">{{ t('modelTrace.pluginMeteringResume') }}</p>
    <p v-else-if="retryAt">{{ t('modelTrace.retryAfter') }} <time :datetime="retryAt">{{ new Date(retryAt).toLocaleString() }}</time></p>
    <p v-else>{{ t('modelTrace.retryUnknown') }}</p>
    <slot />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { MonitoringProbeBudgetDeferral } from '@/types/monitoringProbeBudget'

const props = defineProps<{ deferral: MonitoringProbeBudgetDeferral; title: string; scopeLabel?: string }>()
const { t } = useI18n()
const pluginUnmetered = computed(() => props.deferral.reason === 'plugin_metering_unavailable')
const reasonKey = computed(() => {
  if (pluginUnmetered.value) return 'modelTrace.pluginMeteringReason'
  const reason = props.deferral.reason
  return ['request_limit', 'token_limit', 'recovery_reserve', 'concurrency_limit', 'budget_unavailable'].includes(reason)
    ? `admin.settings.upstreamBillingProbe.budget.${reason}` : 'modelTrace.budgetReasonUnknown'
})
const scope = computed(() => ['global_minute', 'global_day', 'site_minute', 'key_minute', 'global', 'site', 'key', 'system', 'plugin'].includes(props.deferral.scope) ? props.deferral.scope : 'unknown')
const retryAt = computed(() => {
  const value = props.deferral.retry_at
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) ? value : undefined
})
</script>
