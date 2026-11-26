<template>
  <section class="space-y-3 rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800" data-testid="tool-call-result" :aria-label="t('admin.accounts.pelicanTest.tool.title')">
    <header class="flex flex-wrap items-center justify-between gap-2">
      <h4 class="text-sm font-medium">{{ t('admin.accounts.pelicanTest.tool.title') }}</h4>
      <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="tone" data-testid="tool-verdict">{{ t(`admin.accounts.pelicanTest.tool.${status}`) }}</span>
    </header>
    <p class="break-all text-xs text-gray-600 dark:text-gray-300">{{ t('admin.accounts.pelicanTest.tool.model') }}: {{ model || '—' }}<template v-if="result?.model && result.model !== model"> → {{ result.model }}</template> · Responses HTTP</p>
    <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
      <div v-for="phase in phases" :key="phase.key" class="rounded-lg border border-gray-200 bg-white p-2 dark:border-dark-600 dark:bg-dark-700">
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t(`admin.accounts.pelicanTest.tool.${phase.key}`) }}</p>
        <p class="mt-1 text-sm font-medium" :class="phase.ok ? 'text-green-700 dark:text-green-400' : 'text-gray-600 dark:text-gray-300'">{{ t(`admin.accounts.pelicanTest.tool.${phase.ok ? 'verified' : 'unconfirmed'}`) }}</p>
      </div>
    </div>
    <p v-if="reason" class="text-xs" data-testid="tool-reason">{{ reason }}</p>
    <p class="text-xs text-gray-600 dark:text-gray-300">{{ t('admin.accounts.pelicanTest.tool.usage', { used: result?.observed_tokens ?? '—', budget: result?.token_budget ?? '—' }) }}</p>
    <ul v-if="result?.rounds?.length" class="space-y-1 text-xs text-gray-500 dark:text-gray-400">
      <li v-for="(round, index) in result.rounds" :key="index" class="break-words">{{ t('admin.accounts.pelicanTest.tool.round', { round: index + 1, input: round.input_tokens ?? '—', output: round.output_tokens ?? '—', cached: round.cached_tokens ?? '—', ms: round.latency_ms }) }}</li>
    </ul>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.pelicanTest.tool.scope') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ToolCallProbeResult } from '@/types'

const props = defineProps<{ result?: ToolCallProbeResult; model?: string }>()
const { t, te } = useI18n()
const status = computed(() => props.result?.status || 'inconclusive')
const tone = computed(() => status.value === 'pass' ? 'bg-green-100 text-green-700 dark:bg-green-500/20 dark:text-green-400' : status.value === 'fail' ? 'bg-red-100 text-red-700 dark:bg-red-500/20 dark:text-red-400' : 'bg-gray-200 text-gray-700 dark:bg-dark-600 dark:text-gray-300')
const reason = computed(() => {
  const key = `admin.accounts.pelicanTest.tool.reasons.${props.result?.reason || 'not_observed'}`
  return t(te(key) ? key : 'admin.accounts.pelicanTest.tool.reasons.not_observed')
})
const phases = computed(() => [
  { key: 'arguments', ok: props.result?.arguments_verified === true },
  { key: 'result', ok: props.result?.result_verified === true }
])
</script>
