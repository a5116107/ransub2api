<template>
  <div class="min-w-0 space-y-2 py-2" data-testid="quality-round-evidence">
    <StatusBadge :status="status" :label="t(`channelOps.${outcome}`)" data-testid="quality-round-outcome" />
    <p class="text-xs text-gray-500">{{ t('channelOps.evidence.historical') }}</p>
    <p data-testid="quality-round-coverage" class="text-xs tabular-nums">
      {{ t('channelOps.evidence.roundCoverage') }} {{ count(round.round_probes) }} / {{ count(round.round_expected) }}
      · {{ coverage == null ? t('channelOps.evidence.unknown') : `${Math.floor(coverage * 10) / 10}%` }}
    </p>
    <progress v-if="coverage != null" :value="coverage" max="100" class="block h-2 w-full max-w-40 accent-primary-600" :aria-label="t('channelOps.evidence.roundCoverage')" />
    <p v-if="!round.round_complete" class="text-xs text-amber-700 dark:text-amber-400">{{ t('channelOps.incompleteRound') }} {{ count(round.round_probes) }} / {{ count(round.round_expected) }}</p>
    <StatusBadge status="unknown" :label="t('channelOps.evidence.identityUnknown')" data-testid="quality-round-identity" />
    <details class="max-w-xs text-xs">
      <summary class="cursor-pointer text-primary-600 dark:text-primary-400">{{ t('channelOps.evidence.details') }}</summary>
      <dl class="mt-2 space-y-2 break-words">
        <div><dt class="text-gray-500">{{ t('channelOps.evidence.probes') }}</dt><dd>{{ count(round.passed_probes) }} / {{ count(round.probes) }} ({{ count(round.expected) }})</dd></div>
        <div><dt class="text-gray-500">{{ t('channelOps.evidence.observedAt') }}</dt><dd><time v-if="observedAt" :datetime="round.observed_at">{{ observedAt }}</time><span v-else>{{ t('channelOps.evidence.unknown') }}</span></dd></div>
        <div><dt class="text-gray-500">{{ t('channelOps.evidence.ageAtRead') }}</dt><dd>{{ ageSeconds == null ? t('channelOps.evidence.unknown') : t('channelOps.evidence.seconds', { count: ageSeconds }) }}</dd></div>
      </dl>
      <p class="mt-2 text-gray-600 dark:text-gray-300">{{ t('channelOps.evidence.validityUnknown') }}</p>
      <p class="mt-2 text-gray-600 dark:text-gray-300">{{ t('channelOps.evidence.identityNote') }}</p>
    </details>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import StatusBadge from '@/components/common/StatusBadge.vue'
import type { QualityRound } from '@/api/admin/channelOperations'

const props = defineProps<{ round: QualityRound; queriedAt?: string }>()
const { t, locale } = useI18n()
const validCount = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
const count = (value: unknown) => validCount(value) ? value : t('channelOps.evidence.unknown')
const coverage = computed(() => {
  const { round_probes: actual, round_expected: expected } = props.round
  return validCount(actual) && validCount(expected) && expected > 0 && actual <= expected ? actual / expected * 100 : null
})
const outcome = computed(() => {
  const r = props.round
  // Only display the persisted verdict; never derive quality or identity from a
  // success ratio. Contradictory/partial counts cannot acquire a green verdict.
  const complete = r.round_complete === true && coverage.value === 100 && validCount(r.probes) &&
    r.probes > 0 && r.probes === r.expected && validCount(r.passed_probes) && r.passed_probes <= r.probes && r.round_probes >= r.probes
  if (!complete || !r.measured_model?.trim()) return 'inconclusive'
  if (r.outcome === 'passed' && r.passed_probes === r.probes) return 'passed'
  return r.outcome === 'degraded' ? 'degraded' : 'inconclusive'
})
const status = computed(() => ({ passed: 'success', degraded: 'error', inconclusive: 'warning' })[outcome.value])
const observedTime = computed(() => Date.parse(props.round.observed_at))
const observedAt = computed(() => Number.isFinite(observedTime.value) ? new Date(observedTime.value).toLocaleString(locale?.value) : null)
const ageSeconds = computed(() => {
  const read = Date.parse(props.queriedAt || '')
  const elapsed = read - observedTime.value
  return Number.isFinite(elapsed) && elapsed >= 0 ? Math.floor(elapsed / 1000) : null
})
</script>
