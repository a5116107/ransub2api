<template>
  <section v-if="metrics.length" class="space-y-2 border-t border-gray-100 pt-2 dark:border-dark-700" :data-testid="`${testPrefix}s`">
    <h3 class="text-sm font-medium">{{ t('upstreamOps.rawCache.waitTitle') }}</h3>
    <p class="text-xs leading-5 text-gray-500">{{ t(noteKey) }}</p>
    <dl class="grid min-w-0 gap-3 sm:grid-cols-2">
      <div v-for="metric in metrics" :key="metric.key" :data-testid="`${testPrefix}-${metric.key}`" class="min-w-0 space-y-1">
        <dt class="text-xs text-gray-500">{{ t(`upstreamOps.rawCache.${isWS && metric.key === 'first_event' ? 'wsFirstEvent' : metric.key}`) }}</dt>
        <dd class="break-words font-medium tabular-nums">{{ metric.mean == null ? t('upstreamOps.unknown') : `${number(metric.mean, 1)} ms` }} <span class="text-xs font-normal text-gray-500">{{ t(`upstreamOps.rawCache.${metric.status}`) }}</span></dd>
        <dd class="break-words text-xs text-gray-500">{{ t('upstreamOps.rawCache.waitSamples', { samples: number(metric.samples, 0), unknown: number(metric.unknown, 0), coverage: percent(metric.coverage) }) }}</dd>
        <dd class="break-words text-xs text-gray-500">{{ t('upstreamOps.rawCache.waitLatest', { at: metric.latest ?? t('upstreamOps.unknown') }) }}</dd>
      </div>
    </dl>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UpstreamWaitSet } from '@/api/admin/upstreamOperations'

const props = defineProps<{ transport?: string | null; waits?: UpstreamWaitSet; scope: 'cache' | 'attempts' }>()
const { t } = useI18n()
const isWS = computed(() => props.transport === 'ws_turn')
const testPrefix = computed(() => props.scope === 'attempts' ? 'upstream-attempt-wait' : 'upstream-wait')
const noteKey = computed(() => props.scope === 'attempts'
  ? isWS.value ? 'upstreamOps.attempts.wsWaitScope' : 'upstreamOps.attempts.waitScope'
  : isWS.value ? 'upstreamOps.rawCache.wsWaitScope' : 'upstreamOps.rawCache.waitScope')
const metrics = computed(() => {
  const waits = props.waits
  const source = isWS.value ? 'canonical_physical_ws_turn_generation_start'
    : props.scope === 'attempts' ? 'canonical_physical_http_attempt' : 'canonical_final_physical_http_attempt'
  if (!['http', 'ws_turn'].includes(props.transport ?? '') || waits?.version !== 1 || waits.source !== source) return []
  const keys = isWS.value ? ['first_event', 'first_reasoning', 'first_text'] as const : ['first_byte', 'first_event', 'first_reasoning', 'first_text'] as const
  return keys.map(key => {
    const metric = waits[key]
    const mean = metric && metric.samples > 0 && typeof metric.mean_ms === 'number' && Number.isFinite(metric.mean_ms) && metric.mean_ms >= 0 ? metric.mean_ms : null
    return { key, mean, samples: metric?.samples, unknown: metric?.unknown_samples, coverage: metric?.coverage,
      latest: mean == null ? null : metric?.latest_sample_at,
      status: mean == null ? 'unknown' : metric.unknown_samples > 0 ? 'partial' : 'waitObserved' }
  })
})
function number(value: number | null | undefined, decimals: number) {
  return value == null || !Number.isFinite(value) ? t('upstreamOps.unknown') : value.toLocaleString(undefined, { minimumFractionDigits: decimals, maximumFractionDigits: decimals })
}
function percent(value: number | null | undefined) {
  return value != null && Number.isFinite(value) && value >= 0 && value <= 1 ? `${(value * 100).toFixed(1)}%` : t('upstreamOps.unknown')
}
</script>
