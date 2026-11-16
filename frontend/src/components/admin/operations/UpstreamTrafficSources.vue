<template>
  <div class="space-y-2 rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-800" data-testid="upstream-traffic-sources">
    <p class="font-medium">{{ t('upstreamOps.traffic.title') }}</p>
    <dl class="grid grid-cols-1 gap-2 sm:grid-cols-3">
      <div v-for="source in sources" :key="source" :data-testid="`traffic-${source}`" class="flex flex-wrap items-center justify-between gap-2 rounded border border-gray-200 p-2 dark:border-dark-600">
        <dt>{{ t(`upstreamOps.traffic.${source}`) }}</dt><dd class="font-semibold tabular-nums">{{ counts[source] }}</dd>
      </div>
    </dl>
    <p>{{ t('upstreamOps.traffic.coverage', { known: valid ? traffic?.known_samples : 0, total: samples, coverage }) }}</p>
    <p class="leading-5 text-gray-500">{{ t('upstreamOps.traffic.scope') }}</p>
  </div>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UpstreamTrafficSummary, UpstreamTrafficSource } from '@/api/admin/upstreamOperations'
const props = defineProps<{ traffic?: UpstreamTrafficSummary; samples: number }>()
const { t } = useI18n()
const sources: UpstreamTrafficSource[] = ['gateway_ingress', 'account_connection_test', 'unknown']
const natural = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
const valid = computed(() => {
  const value = props.traffic
  if (!value || value.version !== 1 || value.source !== 'server_recorded_entrypoints' || !natural(props.samples) ||
    value.samples !== props.samples || !natural(value.known_samples) || !natural(value.unknown_samples) ||
    value.known_samples + value.unknown_samples !== value.samples || !Array.isArray(value.rows) || value.rows.length > 3) return false
  const seen = new Set<UpstreamTrafficSource>()
  let sum = 0, unknown = 0
  for (const row of value.rows) {
    if (!row || !sources.includes(row.traffic_source) || seen.has(row.traffic_source) || !natural(row.samples)) return false
    seen.add(row.traffic_source); sum += row.samples
    if (row.traffic_source === 'unknown') unknown = row.samples
  }
  return sum === value.samples && unknown === value.unknown_samples && (value.samples === 0 ? value.coverage === null :
    typeof value.coverage === 'number' && Number.isFinite(value.coverage) && Math.abs(value.coverage - value.known_samples / value.samples) < 1e-9)
})
const counts = computed(() => {
  const result = { gateway_ingress: 0, account_connection_test: 0, unknown: props.samples }
  if (valid.value && props.traffic) {
    result.unknown = 0
    for (const row of props.traffic.rows) result[row.traffic_source] = row.samples
  }
  return result
})
const coverage = computed(() => valid.value && props.traffic?.coverage != null ? `${(props.traffic.coverage * 100).toFixed(1)}%` : t('upstreamOps.unknown'))
</script>
