<template>
  <div class="space-y-1 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-200" role="status">
    <p class="font-semibold">{{ title }}</p>
    <p>{{ t(reasonKey) }} · {{ t(`modelTrace.budgetScopes.${scope}`) }}</p>
    <p v-if="retryAt">{{ t('modelTrace.retryAfter') }} <time :datetime="retryAt">{{ new Date(retryAt).toLocaleString() }}</time></p>
    <p v-else>{{ t('modelTrace.retryUnknown') }}</p>
    <slot />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { MonitoringProbeBudgetDeferral } from '@/types/monitoringProbeBudget'

const props = defineProps<{ deferral: MonitoringProbeBudgetDeferral; title: string }>()
const { t } = useI18n()
const reasonKey = computed(() => {
  const reason = props.deferral.reason
  return ['request_limit', 'recovery_reserve', 'concurrency_limit', 'budget_unavailable'].includes(reason)
    ? `admin.settings.upstreamBillingProbe.budget.${reason}` : 'modelTrace.budgetReasonUnknown'
})
const scope = computed(() => ['global_minute', 'global_day', 'site_minute', 'key_minute', 'global', 'site', 'key', 'system'].includes(props.deferral.scope) ? props.deferral.scope : 'unknown')
const retryAt = computed(() => {
  const value = props.deferral.retry_at
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) ? value : undefined
})
</script>
