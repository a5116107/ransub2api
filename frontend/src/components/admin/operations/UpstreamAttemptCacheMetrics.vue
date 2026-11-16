<template>
  <section class="min-w-0 space-y-2 border-t border-gray-200 pt-3 text-sm dark:border-dark-600" data-testid="upstream-attempt-cache">
    <div class="flex flex-wrap items-center gap-2">
      <h3 class="font-medium">{{ t('upstreamOps.attempts.cacheTitle') }}</h3>
      <span class="rounded-full px-2 py-1 text-xs" :class="status === 'observed' ? 'bg-green-50 text-green-700 dark:bg-green-950 dark:text-green-400' : 'bg-amber-50 text-amber-800 dark:bg-amber-950 dark:text-amber-300'">{{ t(`upstreamOps.rawCache.${status}`) }}</span>
    </div>
    <p class="text-xs leading-5 text-gray-500">{{ t('upstreamOps.attempts.cacheScope') }}</p>
    <dl class="grid grid-cols-3 gap-2 tabular-nums">
      <div><dt class="text-xs text-gray-500">{{ t('upstreamOps.rawCache.tokenRate') }}</dt><dd data-testid="attempt-cache-token-rate">{{ percent(valid ? cache?.token_read_rate : null) }}</dd></div>
      <div><dt class="text-xs text-gray-500">{{ t('upstreamOps.attempts.cacheHitRate') }}</dt><dd data-testid="attempt-cache-hit-rate">{{ percent(valid ? cache?.attempt_hit_rate : null) }}</dd></div>
      <div><dt class="text-xs text-gray-500">{{ t('upstreamOps.rawCache.coverage') }}</dt><dd data-testid="attempt-cache-coverage">{{ percent(valid ? cache?.coverage : null) }}</dd></div>
    </dl>
    <p v-if="valid && cache" class="break-words text-xs text-gray-500">{{ t('upstreamOps.attempts.cacheCounts', { observed: cache.observed_attempts, total: cache.attempts, unknown: cache.unknown_attempts, read: cache.read_tokens, input: cache.input_tokens }) }}</p>
    <p v-if="valid && cache?.latest_sample_at" class="break-words text-xs text-gray-500">{{ t('upstreamOps.rawCache.waitLatest', { at: cache.latest_sample_at }) }}</p>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UpstreamAttemptCacheMetrics } from '@/api/admin/upstreamOperations'

const props = defineProps<{ cache?: UpstreamAttemptCacheMetrics; attempts: number }>()
const { t } = useI18n()
const valid = computed(() => {
  const c = props.cache
  return c?.version === 1 && c.source === 'canonical_physical_attempt_cache_reports' &&
    [c.attempts, c.observed_attempts, c.unknown_attempts, c.hit_attempts].every(n => Number.isSafeInteger(n) && n >= 0) &&
    c.attempts === props.attempts && c.observed_attempts + c.unknown_attempts === c.attempts && c.hit_attempts <= c.observed_attempts
})
const status = computed(() => !valid.value || !props.cache?.observed_attempts ? 'unknown' : props.cache.unknown_attempts > 0 ? 'partial' : 'observed')
function percent(value: number | null | undefined) {
  return value != null && Number.isFinite(value) && value >= 0 && value <= 1 ? `${(value * 100).toFixed(1)}%` : t('upstreamOps.unknown')
}
</script>
