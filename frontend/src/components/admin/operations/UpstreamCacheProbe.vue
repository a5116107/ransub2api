<template>
  <details class="rounded-xl border border-gray-200 bg-white p-5 dark:border-dark-700 dark:bg-dark-800" data-testid="upstream-cache-probe">
    <summary class="cursor-pointer font-semibold">{{ t('admin.groupCacheTest.upstream.title') }}</summary>
    <p class="mt-3 text-sm text-gray-500">{{ t('admin.groupCacheTest.upstream.description') }}</p>
    <form class="mt-4 space-y-4" @submit.prevent="start">
      <fieldset :disabled="busy" class="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
        <label class="text-sm"><span>{{ t('admin.groupCacheTest.upstream.account') }}</span><select v-model="accountId" class="input mt-1" data-testid="upstream-cache-account"><option :value="null">{{ t('admin.groupCacheTest.upstream.selectAccount') }}</option><option v-for="account in accounts" :key="account.id" :value="account.id">{{ account.name }} · #{{ account.id }}</option></select></label>
        <label class="text-sm"><span>{{ t('admin.groupCacheTest.model') }}</span><input v-model="settings.model" class="input mt-1" maxlength="200" required data-testid="upstream-cache-model" /></label>
        <label class="text-sm"><span>{{ t('admin.groupCacheTest.upstream.intervals') }}</span><input v-model="intervalText" class="input mt-1" placeholder="1, 3" required data-testid="upstream-cache-intervals" /></label>
        <label class="text-sm"><span>{{ t('admin.groupCacheTest.upstream.prefix') }}</span><input v-model.number="settings.prefix_blocks" class="input mt-1" type="number" min="16" max="128" required /></label>
        <label class="text-sm"><span>{{ t('admin.groupCacheTest.upstream.timeout') }}</span><input v-model.number="settings.timeout_seconds" class="input mt-1" type="number" min="10" max="120" required /></label>
        <label class="text-sm"><span>{{ t('admin.groupCacheTest.reasoning') }}</span><select v-model="settings.reasoning_effort" class="input mt-1"><option value="">{{ t('admin.groupCacheTest.unspecified') }}</option><option v-for="effort in ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']" :key="effort" :value="effort">{{ effort }}</option></select></label>
        <label class="text-sm"><span>{{ t('admin.groupCacheTest.tier') }}</span><select v-model="settings.service_tier" class="input mt-1"><option value="">{{ t('admin.groupCacheTest.unspecified') }}</option><option value="default">default</option><option value="priority">priority / fast</option></select></label>
        <label class="text-sm"><span>{{ t('admin.groupCacheTest.upstream.search') }}</span><span class="mt-1 flex gap-2"><input v-model="search" class="input min-w-0" maxlength="200" /><button type="button" class="btn btn-secondary shrink-0" :disabled="loading" @click="loadAccounts">{{ t('admin.groupCacheTest.reload') }}</button></span></label>
      </fieldset>
      <p class="text-xs text-gray-500">{{ t('admin.groupCacheTest.upstream.limits') }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ error }}</p>
      <div class="flex flex-wrap gap-2"><button class="btn btn-primary" :disabled="busy || loading || !canStart" data-testid="upstream-cache-start">{{ t('admin.groupCacheTest.start') }}</button><button v-if="busy" type="button" class="btn btn-secondary" @click="stop">{{ t('admin.groupCacheTest.stop') }}</button><button v-if="result && !busy" type="button" class="btn btn-secondary" @click="download">{{ t('admin.groupCacheTest.export') }}</button></div>
    </form>
    <section v-if="result" class="mt-5 space-y-3" aria-live="polite" data-testid="upstream-cache-result">
      <h3 class="text-sm font-medium">{{ t('admin.groupCacheTest.upstream.account') }} #{{ result.account_id }} · {{ result.settings.model }}</h3>
      <div class="flex flex-wrap items-center gap-3 rounded-lg p-4" :class="verdictClass"><strong data-testid="upstream-cache-verdict">{{ t('admin.groupCacheTest.upstream.verdict.' + result.verdict) }}</strong><span class="text-sm">{{ t('admin.groupCacheTest.upstream.coverage', { observed: result.observed_warm_rounds, planned: result.planned_warm_rounds }) }}</span><span class="text-sm">{{ t('admin.groupCacheTest.upstream.requestRate') }} {{ percent(result.request_hit_rate) }} · {{ t('admin.groupCacheTest.upstream.tokenRate') }} {{ percent(result.token_hit_rate) }}</span></div>
      <p class="text-sm">{{ t('admin.groupCacheTest.upstream.cold.' + result.cold_baseline) }} · {{ t('admin.groupCacheTest.upstream.route.' + result.route_continuity) }}</p>
      <p class="text-xs text-gray-500">{{ t('admin.groupCacheTest.upstream.boundary') }}</p>
      <p v-if="result.status !== 'completed'" role="status" class="text-sm text-amber-700 dark:text-amber-300">{{ t('admin.groupCacheTest.upstream.states.' + result.status) }} · {{ reason(result.reason) }}</p>
      <p v-if="result.budget_deferral" class="text-sm">{{ t('admin.groupCacheTest.upstream.budget', { scope: result.budget_deferral.scope, at: result.budget_deferral.retry_at }) }}</p>
      <div class="overflow-x-auto"><table class="w-full whitespace-nowrap text-left text-sm tabular-nums"><thead><tr><th class="p-2">{{ t('admin.groupCacheTest.round') }}</th><th class="p-2">{{ t('admin.groupCacheTest.upstream.interval') }}</th><th class="p-2">{{ t('admin.groupCacheTest.input') }}</th><th class="p-2">{{ t('admin.groupCacheTest.cacheRead') }}</th><th class="p-2">{{ t('admin.groupCacheTest.hitRate') }}</th><th class="p-2">{{ t('admin.groupCacheTest.firstText') }}</th></tr></thead><tbody><tr v-for="round in result.rounds" :key="round.index" class="border-t border-gray-100 dark:border-dark-700"><td class="p-2">{{ round.index }} · {{ t('admin.groupCacheTest.upstream.states.' + round.status) }}</td><td class="p-2">{{ round.interval_ms }} ms</td><td class="p-2">{{ round.cache?.input_tokens ?? '—' }}</td><td class="p-2">{{ round.cache?.read_tokens ?? '—' }}</td><td class="p-2">{{ percent(round.cache?.read_ratio ?? null) }}</td><td class="p-2">{{ round.first_text_ms === null ? '—' : round.first_text_ms.toFixed(0) + ' ms' }}</td></tr></tbody></table></div>
      <details v-for="round in result.rounds" :key="'detail-' + round.index" class="rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-900"><summary class="cursor-pointer">{{ t('admin.groupCacheTest.roundDetails', { index: round.index }) }} · {{ reason(round.reason) }}</summary><dl class="mt-2 grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 break-words"><dt>{{ t('admin.groupCacheTest.upstream.scope') }}</dt><dd>{{ round.scope ? [round.scope.protocol, round.scope.model, round.scope.service_tier, round.scope.reasoning_effort].join(' · ') : '—' }}</dd><dt>{{ t('admin.groupCacheTest.upstream.source') }}</dt><dd>{{ round.cache?.source || '—' }} · {{ round.cache?.status || '—' }}</dd><dt>{{ t('admin.groupCacheTest.upstream.observedAt') }}</dt><dd>{{ round.started_at }} → {{ round.finished_at || '—' }}</dd><dt>{{ t('admin.groupCacheTest.duration') }}</dt><dd>{{ round.duration_ms === null ? '—' : round.duration_ms.toFixed(0) + ' ms' }}</dd></dl></details>
    </section>
  </details>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { saveAs } from 'file-saver'
import { list } from '@/api/admin/accounts'
import { runUpstreamCacheProbe, validUpstreamCacheProbeInput, type UpstreamCacheProbeInput, type UpstreamCacheProbeRun } from '@/api/admin/upstreamCacheProbe'
import { useAuthStore } from '@/stores/auth'

const { t, te } = useI18n(), auth = useAuthStore()
const accounts = ref<{ id: number; name: string }[]>([]), accountId = ref<number | null>(null), search = ref(''), loading = ref(false), busy = ref(false), error = ref('')
const intervalText = ref('1, 3'), result = ref<UpstreamCacheProbeRun | null>(null)
const settings = reactive<UpstreamCacheProbeInput>({ model: '', prefix_blocks: 48, timeout_seconds: 60, intervals_ms: [1000, 3000], reasoning_effort: '', service_tier: '' })
const intervals = computed(() => intervalText.value.split(',').map(s => /^\d+(\.\d{1,3})?$/.test(s.trim()) ? Math.round(Number(s.trim()) * 1000) : NaN))
const input = computed(() => ({ ...settings, model: settings.model.trim(), intervals_ms: intervals.value }))
const canStart = computed(() => accountId.value !== null && validUpstreamCacheProbeInput(input.value))
const verdictClass = computed(() => result.value?.verdict === 'hit_observed' ? 'bg-emerald-50 text-emerald-800 dark:bg-emerald-950/30 dark:text-emerald-300' : result.value?.verdict === 'no_hit_observed' ? 'bg-red-50 text-red-800 dark:bg-red-950/30 dark:text-red-300' : 'bg-amber-50 text-amber-800 dark:bg-amber-950/30 dark:text-amber-300')
const percent = (value: number | null) => value === null ? '—' : (value * 100).toFixed(1) + '%'
const reason = (code: string) => { const key = 'admin.groupCacheTest.upstream.reasons.' + code; return te(key) ? t(key) : t('admin.groupCacheTest.upstream.reasons.unknown') }
let active = true, generation = 0, runGeneration = 0, controller: AbortController | null = null, loadController: AbortController | null = null
async function loadAccounts() {
  loadController?.abort(); loadController = new AbortController()
  const request = ++generation, user = auth.user?.id
  loading.value = true; error.value = ''
  try {
    const data = await list(1, 100, { platform: 'openai', type: 'apikey', status: 'active', search: search.value, lite: 'true' }, { signal: loadController.signal })
    if (!active || request !== generation || user !== auth.user?.id) return
    accounts.value = data.items.map(account => ({ id: account.id, name: account.name }))
    if (!accounts.value.some(account => account.id === accountId.value)) accountId.value = null
  } catch { if (active && request === generation) error.value = t('admin.groupCacheTest.upstream.loadError') }
  finally { if (active && request === generation) loading.value = false }
}
async function start() {
  if (!canStart.value || busy.value || !accountId.value) return
  const user = auth.user?.id, target = accountId.value, request = ++runGeneration
  controller = new AbortController(); const signal = controller.signal
  busy.value = true; result.value = null; error.value = ''
  try {
    await runUpstreamCacheProbe(target, input.value, signal, run => { if (active && request === runGeneration && !signal.aborted && user === auth.user?.id) result.value = run })
  } catch {
    if (active && request === runGeneration && user === auth.user?.id) {
      error.value = t(signal.aborted ? 'admin.groupCacheTest.upstream.stopped' : 'admin.groupCacheTest.upstream.runError')
      invalidateResult(signal.aborted)
    }
  }
  finally { if (active && request === runGeneration) busy.value = false }
}
function invalidateResult(stopped: boolean) {
  const run = result.value
  if (run) {
    run.status = stopped ? 'stopped' : 'unknown'; run.verdict = 'insufficient'; run.reason = stopped ? 'canceled' : 'response_not_confirmed'
    run.finished_at = new Date().toISOString(); run.request_hit_rate = null; run.token_hit_rate = null
    run.cold_baseline = run.rounds[0]?.status !== 'completed' ? 'not_observed' : run.rounds[0]?.cache?.read_tokens === 0 ? 'zero_read_observed' : 'cold_not_confirmed'
    for (const round of run.rounds) if (round.status === 'running') { round.status = run.status; round.reason = run.reason; round.finished_at = run.finished_at }
  }
}
function stop() { controller?.abort() }
function download() { if (result.value) saveAs(new Blob([JSON.stringify(result.value, null, 2)], { type: 'application/json' }), 'upstream-cache-probe.json') }
watch(() => auth.user?.id, () => { generation++; runGeneration++; controller?.abort(); loadController?.abort(); busy.value = false; error.value = ''; accounts.value = []; result.value = null; accountId.value = null; if (active && auth.isAdmin) void loadAccounts() })
onMounted(() => { if (auth.isAdmin) void loadAccounts() })
onBeforeUnmount(() => { active = false; generation++; runGeneration++; controller?.abort(); loadController?.abort() })
</script>
