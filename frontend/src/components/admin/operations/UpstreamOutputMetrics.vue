<template>
  <section class="min-w-0 space-y-2 border-t border-gray-200 pt-3 text-sm dark:border-dark-600" data-testid="upstream-attempt-output">
    <h3 class="font-medium">{{ t('upstreamOps.output.title') }}</h3>
    <p class="text-xs leading-5 text-gray-500">{{ t('upstreamOps.output.scope') }}</p>
    <dl class="grid min-w-0 gap-3 sm:grid-cols-2">
      <div v-for="metric in distributions" :key="metric.key" class="min-w-0 space-y-1" :data-testid="`attempt-output-${metric.key}`">
        <dt class="flex flex-wrap items-center gap-2 font-medium">{{ t(`upstreamOps.output.${metric.key}`) }}<span class="rounded bg-gray-100 px-2 py-1 text-xs dark:bg-dark-700">{{ t(`upstreamOps.rawCache.${metric.status}`) }}</span></dt>
        <dd class="break-words tabular-nums" data-testid="output-distribution">{{ t('upstreamOps.output.distribution', { mean: ms(metric.valid ? metric.value?.mean_ms : null), p95: ms(metric.valid ? metric.value?.p95_ms : null), max: ms(metric.valid ? metric.value?.max_ms : null) }) }}</dd>
        <dd class="break-words text-xs text-gray-500">{{ t('upstreamOps.output.samples', { samples: count(metric.valid ? metric.value?.samples : null), unknown: count(metric.valid ? metric.value?.unknown_samples : null), coverage: percent(metric.valid ? metric.value?.coverage : null) }) }}</dd>
        <dd v-if="metric.valid && metric.value?.partial_samples" class="break-words text-xs text-amber-800 dark:text-amber-300" data-testid="output-partial">{{ t('upstreamOps.output.partial', { count: metric.value.partial_samples, max: ms(metric.value.partial_max_ms) }) }}</dd>
        <dd class="break-words text-xs text-gray-500">{{ t('upstreamOps.rawCache.waitLatest', { at: metric.valid ? metric.value?.latest_sample_at ?? t('upstreamOps.unknown') : t('upstreamOps.unknown') }) }}</dd>
      </div>
      <div class="min-w-0 space-y-1 sm:col-span-2" data-testid="attempt-output-rate">
        <dt class="font-medium">{{ t('upstreamOps.output.rate') }}</dt>
        <dd class="text-xs leading-5 text-gray-500">{{ t('upstreamOps.output.rateScope') }}</dd>
        <dd class="break-words font-semibold tabular-nums" data-testid="output-rate-value">{{ rateValid && output?.rate.samples ? `${number(output.rate.tokens_per_second)} Token/s` : t('upstreamOps.unknown') }} <span class="text-xs font-normal text-gray-500">{{ t('upstreamOps.output.estimate') }}</span></dd>
        <dd class="break-words text-xs text-gray-500">{{ t('upstreamOps.output.samples', { samples: count(rateValid ? output?.rate.samples : null), unknown: count(rateValid ? output?.rate.unknown_samples : null), coverage: percent(rateValid ? output?.rate.coverage : null) }) }}</dd>
        <dd class="break-words text-xs text-gray-500">{{ t('upstreamOps.rawCache.waitLatest', { at: rateValid ? output?.rate.latest_sample_at ?? t('upstreamOps.unknown') : t('upstreamOps.unknown') }) }}</dd>
      </div>
    </dl>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UpstreamAttemptOutputMetrics } from '@/api/admin/upstreamOperations'

const props = defineProps<{ output?: UpstreamAttemptOutputMetrics; attempts: number }>()
const { t } = useI18n()
const natural = (n: unknown): n is number => typeof n === 'number' && Number.isSafeInteger(n) && n >= 0
const positive = (n: unknown): n is number => typeof n === 'number' && Number.isFinite(n) && n > 0
const nonnegative = (n: unknown): n is number => n === 0 || positive(n)
const valid = computed(() => props.output?.version === 1 && props.output.source === 'canonical_physical_attempt_output' && natural(props.attempts) && props.output.attempts === props.attempts)
function validSamples(value?: { samples: number; unknown_samples: number; coverage: number | null }) {
  return valid.value && value && natural(value.samples) && natural(value.unknown_samples) && value.samples + value.unknown_samples === props.attempts &&
    (props.attempts === 0 ? value.coverage == null : typeof value.coverage === 'number' && Number.isFinite(value.coverage) && Math.abs(value.coverage - value.samples / props.attempts) < 1e-9)
}
const distributions = computed(() => (['max_gap', 'tail_wait'] as const).map(key => {
  const value = props.output?.[key]
  const ok = Boolean(validSamples(value) && value && natural(value.partial_samples) && value.partial_samples <= value.unknown_samples &&
    (value.samples === 0 ? value.mean_ms == null && value.p95_ms == null && value.max_ms == null :
      nonnegative(value.mean_ms) && nonnegative(value.p95_ms) && nonnegative(value.max_ms) && value.mean_ms <= value.max_ms && value.p95_ms <= value.max_ms) &&
    (value.partial_samples === 0 ? value.partial_max_ms == null : nonnegative(value.partial_max_ms)))
  return { key, value, valid: ok, status: !ok || !value?.samples ? 'unknown' : value.unknown_samples ? 'partial' : 'observed' }
}))
const rateValid = computed(() => {
  const rate = props.output?.rate
  return Boolean(validSamples(rate) && rate && (rate.samples === 0 ? rate.tokens_per_second == null && rate.non_reasoning_tokens == null && rate.window_ms == null :
    positive(rate.tokens_per_second) && natural(rate.non_reasoning_tokens) && rate.non_reasoning_tokens >= 32 * rate.samples && positive(rate.window_ms) && rate.window_ms >= 250 * rate.samples &&
    Math.abs(rate.tokens_per_second - rate.non_reasoning_tokens * 1000 / rate.window_ms) <= Math.max(1e-6, rate.tokens_per_second * 1e-9)))
})
const number = (value: number | null | undefined) => value != null && Number.isFinite(value) ? value.toLocaleString(undefined, { maximumFractionDigits: 1, minimumFractionDigits: 1 }) : t('upstreamOps.unknown')
const count = (value: number | null | undefined) => natural(value) ? value : t('upstreamOps.unknown')
const ms = (value: number | null | undefined) => nonnegative(value) ? `${number(value)} ms` : t('upstreamOps.unknown')
const percent = (value: number | null | undefined) => value != null && Number.isFinite(value) && value >= 0 && value <= 1 ? `${(value * 100).toFixed(1)}%` : t('upstreamOps.unknown')
</script>
