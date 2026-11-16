<template>
  <AppLayout>
    <div class="min-w-0 space-y-5">
      <header class="flex flex-wrap items-start justify-between gap-3"><div><h1 class="text-2xl font-semibold">{{ t('upstreamOps.title') }}</h1><p class="mt-2 text-sm text-gray-500">{{ t('upstreamOps.description') }}</p></div><button class="btn btn-secondary" data-testid="refresh-operations" :disabled="loading" @click="load">{{ t('upstreamOps.refresh') }}</button></header>
      <SmartOpsNav />
      <p class="rounded-xl bg-primary-50 p-4 text-sm leading-6 dark:bg-dark-800">{{ t('upstreamOps.attribution') }}</p>
      <form data-testid="upstream-filters" class="card grid gap-3 p-4 sm:grid-cols-2 lg:grid-cols-4" @submit.prevent="apply">
        <label>{{ t('upstreamOps.from') }}<input v-model="from" type="datetime-local" class="input w-full" required /></label>
        <label>{{ t('upstreamOps.until') }}<input v-model="until" type="datetime-local" class="input w-full" required /></label>
        <label>{{ t('upstreamOps.account') }}<input v-model.number="draft.account_id" type="number" min="0" class="input w-full" /></label>
        <label>{{ t('upstreamOps.group') }}<input v-model.number="draft.group_id" type="number" min="0" class="input w-full" /></label>
        <label>{{ t('upstreamOps.upstream') }}<input v-model="draft.upstream" maxlength="2048" class="input w-full" /></label>
        <label>{{ t('upstreamOps.model') }}<input v-model="draft.model" data-testid="model-filter" maxlength="200" class="input w-full" /></label>
        <label>{{ t('upstreamOps.modelScope') }}<select v-model="draft.model_scope" data-testid="model-scope" class="input w-full"><option value="public">{{ t('upstreamOps.public') }}</option><option value="recorded">{{ t('upstreamOps.recorded') }}</option><option value="upstream">{{ t('upstreamOps.upstreamModel') }}</option></select></label>
        <label>{{ t('upstreamOps.dimension') }}<select v-model="draft.dimension" class="input w-full"><option value="upstream">{{ t('upstreamOps.upstream') }}</option><option value="account">{{ t('upstreamOps.account') }}</option><option value="model">{{ t('upstreamOps.model') }}</option></select></label>
        <label>{{ t('upstreamOps.sort') }}<select v-model="draft.sort" data-testid="sort-filter" class="input w-full"><option v-for="key in sortKeys" :key="key" :value="key">{{ t(`upstreamOps.${key}`) }}</option></select></label>
        <label>{{ t('upstreamOps.direction') }}<select v-model="draft.direction" class="input w-full"><option value="desc">{{ t('upstreamOps.desc') }}</option><option value="asc">{{ t('upstreamOps.asc') }}</option></select></label>
        <button class="btn btn-primary self-end" :disabled="loading">{{ t('upstreamOps.apply') }}</button><button type="button" class="btn btn-secondary self-end" :disabled="loading" @click="last24h">{{ t('upstreamOps.last24h') }}</button>
      </form>
      <p v-if="error" role="alert" class="text-red-600">{{ error }}</p><p v-if="loading" role="status">{{ t('upstreamOps.loading') }}</p>
      <template v-if="snapshot && !error">
        <p class="break-words text-xs text-gray-500" data-testid="applied-scope">{{ snapshot.scope.from }} ≤ t &lt; {{ snapshot.scope.until }} · {{ t('upstreamOps.queried') }} {{ snapshot.queried_at }}</p>
        <section class="card space-y-3 p-4"><h2 class="font-semibold">{{ t('upstreamOps.discovery') }}</h2><p class="text-xs text-gray-500">{{ t('upstreamOps.discoveryNote') }}</p><div class="flex flex-wrap gap-2"><button v-for="c in snapshot.usage.catalog" :key="c.key" class="btn btn-secondary max-w-full break-all" @click="selectUpstream(c.key)">{{ c.name }} · {{ c.accounts }}</button></div><p class="text-xs">{{ applied.page }} · {{ snapshot.usage.catalog_total }}</p></section>
        <section v-if="applied.upstream || applied.account_id" class="card space-y-3 p-4"><h2 class="font-semibold">{{ t('upstreamOps.currentMembers') }}</h2><p class="text-xs text-gray-500">{{ t('upstreamOps.memberNote') }}</p><div class="flex flex-wrap gap-2"><button v-for="a in snapshot.usage.members" :key="a.id" class="btn btn-secondary" data-testid="member-drill" @click="selectAccount(a.id)">{{ a.name }} #{{ a.id }}</button></div><p class="text-xs">{{ applied.page }} · {{ snapshot.usage.member_total }}</p></section>
        <section class="card p-4" data-testid="upstream-summary"><h2>{{ t('upstreamOps.summary') }}</h2><p>{{ t('upstreamOps.requests') }} {{ snapshot.usage.summary.requests }} · {{ t('upstreamOps.unknownUpstream') }} {{ snapshot.usage.summary.unknown_upstream_requests }}</p></section>
        <section class="card space-y-4 p-4">
          <div class="flex flex-wrap items-center justify-between gap-3"><h2 class="font-semibold">{{ t('upstreamOps.compare') }} · {{ t(`upstreamOps.${applied.dimension}`) }}</h2><button class="btn btn-secondary" @click="overview">{{ t('upstreamOps.overview') }}</button></div>
          <p v-if="!snapshot.usage.rows.length">{{ t('upstreamOps.empty') }}</p>
          <div v-else class="overflow-x-auto"><table class="w-full min-w-[1000px] text-left text-sm"><thead><tr><th class="p-2">{{ t(`upstreamOps.${applied.dimension}`) }}</th><th v-for="key in amountKeys" :key="key" class="p-2">{{ t(`upstreamOps.${key}`) }}</th><th>P50 / P90 / P95 (ms)</th><th>{{ t('upstreamOps.duration') }}</th></tr></thead><tbody><tr v-for="row in snapshot.usage.rows" :key="row.key" class="border-t dark:border-dark-700"><td class="max-w-80 break-all p-2"><button v-if="applied.dimension !== 'model'" class="text-primary-600 underline" data-testid="drill-row" @click="drill(row)">{{ row.name }}</button><span v-else>{{ row.name }}</span></td><td v-for="key in amountKeys" :key="key" class="p-2">{{ key === 'requests' ? row.requests : formatMetric(row,key) }}</td><td>{{ number(row.p50_ms, 0) }} / {{ number(row.p90_ms, 0) }} / {{ number(row.p95_ms, 0) }}<span class="block text-xs">{{ t('upstreamOps.samples') }} {{ row.samples }} / {{ t('upstreamOps.unknown') }} {{ row.unknown_samples }}</span></td><td>{{ number(row.duration_p90_ms,0) }}</td></tr></tbody></table></div>
          <p class="text-xs text-gray-500">{{ t('upstreamOps.latencyNote') }}</p>
          <div class="flex flex-wrap items-center gap-3"><button class="btn btn-secondary" :disabled="loading || applied.page <= 1" @click="page(-1)">{{ t('upstreamOps.previous') }}</button><span>{{ applied.page }} · {{ snapshot.usage.total }}</span><button class="btn btn-secondary" data-testid="operations-next" :disabled="loading || applied.page * applied.page_size >= Math.max(snapshot.usage.total, snapshot.usage.catalog_total || 0, snapshot.quality.data?.total || 0, snapshot.raw_cache?.data?.total || 0, applied.upstream || applied.account_id ? snapshot.usage.member_total || 0 : 0)" @click="page(1)">{{ t('upstreamOps.next') }}</button></div>
        </section>
        <section v-if="snapshot.raw_cache" class="card min-w-0 space-y-4 p-4" data-testid="upstream-raw-cache" aria-labelledby="upstream-raw-cache-title">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h2 id="upstream-raw-cache-title" class="font-semibold">{{ t('upstreamOps.rawCache.title') }}</h2>
            <span v-if="snapshot.raw_cache.data" class="rounded-full bg-primary-50 px-3 py-1 text-xs dark:bg-dark-800" data-testid="upstream-cache-status">{{ t(`upstreamOps.rawCache.${cacheStatus(snapshot.raw_cache.data.summary)}`) }}</span>
          </div>
          <p class="text-xs leading-5 text-gray-500">{{ t('upstreamOps.rawCache.scope') }}</p>
          <p v-if="snapshot.raw_cache.status !== 'ready'" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ t('upstreamOps.queryFailed') }}</p>
          <template v-else-if="snapshot.raw_cache.data">
            <dl class="grid gap-3 sm:grid-cols-3">
              <div><dt class="text-sm text-gray-500">{{ t('upstreamOps.rawCache.tokenRate') }}</dt><dd class="text-2xl font-semibold tabular-nums" data-testid="upstream-cache-token-rate">{{ percent(snapshot.raw_cache.data.summary.token_read_rate) }}</dd></div>
              <div><dt class="text-sm text-gray-500">{{ t('upstreamOps.rawCache.requestRate') }}</dt><dd class="text-2xl font-semibold tabular-nums" data-testid="upstream-cache-request-rate">{{ percent(snapshot.raw_cache.data.summary.request_hit_rate) }}</dd></div>
              <div><dt class="text-sm text-gray-500">{{ t('upstreamOps.rawCache.coverage') }}</dt><dd class="text-2xl font-semibold tabular-nums" data-testid="upstream-cache-coverage">{{ percent(snapshot.raw_cache.data.summary.coverage) }}</dd></div>
            </dl>
            <p class="text-sm">{{ t('upstreamOps.rawCache.counts', { observed: number(snapshot.raw_cache.data.summary.observed_requests, 0), total: number(snapshot.raw_cache.data.summary.requests, 0), unknown: number(snapshot.raw_cache.data.summary.unknown_requests, 0), unattributed: number(snapshot.raw_cache.data.summary.unattributed_requests, 0) }) }}</p>
            <p v-if="!snapshot.raw_cache.data.summary.requests" class="text-sm text-gray-500">{{ t('upstreamOps.empty') }}</p>
            <ul v-else class="grid min-w-0 gap-3 lg:grid-cols-2">
              <li v-for="(row, index) in snapshot.raw_cache.data.rows" :key="index" class="min-w-0 space-y-2 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600" data-testid="upstream-cache-row">
                <p class="break-all font-medium">#{{ row.account_id }} · {{ t('upstreamOps.rawCache.credential') }} {{ row.credential_fingerprint?.slice(0, 12) ?? t('upstreamOps.unknown') }}</p>
                <p class="break-words">{{ row.public_model }} → {{ row.upstream_model ?? t('upstreamOps.unknown') }}</p>
                <p class="break-words text-xs text-gray-500">{{ row.protocol ?? t('upstreamOps.unknown') }} / {{ row.transport ?? t('upstreamOps.unknown') }} · {{ row.service_tier || t('upstreamOps.unknown') }} · {{ row.reasoning_effort || t('upstreamOps.unknown') }}</p>
                <p class="text-xs text-gray-500">{{ row.stream === true ? t('upstreamOps.rawCache.streaming') : row.stream === false ? t('upstreamOps.rawCache.nonStreaming') : t('upstreamOps.unknown') }}</p>
                <dl class="grid grid-cols-3 gap-2 tabular-nums">
                  <div><dt class="text-xs text-gray-500">{{ t('upstreamOps.rawCache.tokenRate') }}</dt><dd>{{ percent(row.token_read_rate) }}</dd></div>
                  <div><dt class="text-xs text-gray-500">{{ t('upstreamOps.rawCache.requestRate') }}</dt><dd>{{ percent(row.request_hit_rate) }}</dd></div>
                  <div><dt class="text-xs text-gray-500">{{ t('upstreamOps.rawCache.coverage') }}</dt><dd>{{ percent(row.coverage) }}</dd></div>
                </dl>
                <p class="text-xs text-gray-500">{{ t('upstreamOps.rawCache.rowCounts', { observed: number(row.observed_requests, 0), total: number(row.requests, 0), read: number(row.read_tokens, 0), input: number(row.input_tokens, 0) }) }}</p>
                <section v-if="waitRows(row).length" class="space-y-2 border-t border-gray-100 pt-2 dark:border-dark-700" data-testid="upstream-waits">
                  <h3 class="text-sm font-medium">{{ t('upstreamOps.rawCache.waitTitle') }}</h3>
                  <p class="text-xs leading-5 text-gray-500">{{ t(row.transport === 'ws_turn' ? 'upstreamOps.rawCache.wsWaitScope' : 'upstreamOps.rawCache.waitScope') }}</p>
                  <dl class="grid min-w-0 gap-3 sm:grid-cols-2">
                    <div v-for="metric in waitRows(row)" :key="metric.key" :data-testid="`upstream-wait-${metric.key}`" class="min-w-0 space-y-1">
                      <dt class="text-xs text-gray-500">{{ t(`upstreamOps.rawCache.${row.transport === 'ws_turn' && metric.key === 'first_event' ? 'wsFirstEvent' : metric.key}`) }}</dt>
                      <dd class="break-words font-medium tabular-nums">{{ metric.mean == null ? t('upstreamOps.unknown') : `${number(metric.mean, 1)} ms` }} <span class="text-xs font-normal text-gray-500">{{ t(`upstreamOps.rawCache.${metric.status}`) }}</span></dd>
                      <dd class="break-words text-xs text-gray-500">{{ t('upstreamOps.rawCache.waitSamples', { samples: number(metric.samples, 0), unknown: number(metric.unknown, 0), coverage: percent(metric.coverage) }) }}</dd>
                      <dd class="break-words text-xs text-gray-500">{{ t('upstreamOps.rawCache.waitLatest', { at: metric.latest ?? t('upstreamOps.unknown') }) }}</dd>
                    </div>
                  </dl>
                </section>
                <p class="text-xs text-amber-700 dark:text-amber-400">{{ t('upstreamOps.rawCache.groupUnknown') }}</p>
                <details class="text-xs"><summary class="cursor-pointer text-primary-600 dark:text-primary-400">{{ t('upstreamOps.rawCache.route') }}</summary><p class="mt-2 break-all">{{ row.route_fingerprint ?? t('upstreamOps.unknown') }}</p><p class="mt-1 break-words">{{ row.latest_sample_at ?? t('upstreamOps.unknown') }}</p></details>
              </li>
            </ul>
            <p class="text-xs text-gray-500">{{ t('upstreamOps.rawCache.page', { page: applied.page, total: snapshot.raw_cache.data.total }) }}</p>
          </template>
        </section>
        <section v-if="snapshot.usage.capacity" class="card space-y-2 p-4"><h2 class="font-semibold">{{ t('upstreamOps.capacity') }}</h2><p>{{ t('upstreamOps.account') }} {{ snapshot.usage.capacity.accounts }} · {{ t('upstreamOps.schedulable') }} {{ snapshot.usage.capacity.schedulable }} · {{ t('upstreamOps.concurrency') }} {{ snapshot.usage.capacity.configured_concurrency }}</p><p class="text-xs text-gray-500">{{ t('upstreamOps.capacityNote') }}</p></section>
        <section class="card space-y-3 p-4"><h2 class="font-semibold">{{ t('upstreamOps.quality') }}</h2><p class="text-sm text-gray-500">{{ t('upstreamOps.qualityNote') }}</p><p v-if="snapshot.quality.status !== 'ready'" role="alert">{{ snapshot.quality.error }}</p><template v-else-if="snapshot.quality.data"><p>{{ t('upstreamOps.rounds') }} {{ snapshot.quality.data.summary.rounds }} · {{ t('upstreamOps.passed') }} {{ snapshot.quality.data.summary.passed }} · {{ t('upstreamOps.degraded') }} {{ snapshot.quality.data.summary.degraded }} · {{ t('upstreamOps.inconclusive') }} {{ snapshot.quality.data.summary.inconclusive }}</p><div class="overflow-x-auto"><table class="w-full min-w-[650px] text-left text-sm"><tbody><tr v-for="round in snapshot.quality.data.rows" :key="round.id"><td class="p-2">#{{ round.account_id }}</td><td>{{ round.measured_model }}</td><td>{{ round.observed_at }}</td><td><QualityRoundAssessment :round="round" :queried-at="snapshot.quality.queried_at" /></td><td><a class="underline text-primary-600" :href="`/admin/account-quality?plan_id=${round.plan_id}`">{{ t('upstreamOps.plan') }} #{{ round.plan_id }}</a></td></tr></tbody></table></div><p class="text-xs">{{ t('upstreamOps.pageScope') }} {{ applied.page }} · {{ snapshot.quality.data.total }}</p></template><a class="text-primary-600 underline" href="/admin/account-quality">{{ t('upstreamOps.quality') }}</a></section>
        <section class="card space-y-3 p-4"><h2 class="font-semibold">{{ t('upstreamOps.errors') }}</h2><p class="text-sm text-gray-500">{{ t('upstreamOps.errorNote') }}</p><p v-if="snapshot.errors.status !== 'ready'" role="alert">{{ snapshot.errors.error }}</p><template v-else-if="snapshot.errors.data"><p>{{ t('upstreamOps.errorRecords') }} {{ snapshot.errors.data.records }} · 5xx {{ snapshot.errors.data.upstream_5xx }}</p><p v-for="(row, i) in snapshot.errors.data.rows" :key="i" class="text-sm">{{ row.error_type }} / {{ row.error_phase }} / {{ row.status_code }}: {{ row.records }}</p></template><a class="text-primary-600 underline" href="/admin/ops">{{ t('upstreamOps.errors') }}</a></section>
      </template>
    </div>
  </AppLayout>
</template>
<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import SmartOpsNav from './SmartOpsNav.vue'
import QualityRoundAssessment from './QualityRoundAssessment.vue'
import { readOperations, type OperationsQuery, type OperationsSnapshot, type OperationsRow, type UpstreamMetrics, type UpstreamRawCacheMetrics, type UpstreamRawCacheRow } from '@/api/admin/upstreamOperations'
const { t } = useI18n()
const amountKeys = ['requests'] as const
const sortKeys = [...amountKeys, 'name']
const at = new Date(), start = new Date(at.getTime()-86400000)
const local = (d: Date) => new Date(d.getTime()-d.getTimezoneOffset()*60000).toISOString().slice(0,16)
const from=ref(local(start)), until=ref(local(at))
const initial: OperationsQuery = { from:start.toISOString(),until:at.toISOString(),upstream:'',account_id:0,group_id:0,model:'',model_scope:'public',dimension:'upstream',sort:'requests',direction:'desc',page:1,page_size:20 }
const draft=ref<OperationsQuery>({...initial}), applied=ref<OperationsQuery>({...initial})
const snapshot=ref<OperationsSnapshot|null>(null), loading=ref(false), error=ref('')
let request=0, alive=true, controller: AbortController|undefined
function number(v: number|null|undefined, decimals=6) { return v==null || !Number.isFinite(v) ? t('upstreamOps.unknown') : v.toLocaleString(undefined,{minimumFractionDigits:decimals,maximumFractionDigits:decimals}) }
function percent(value: number | null | undefined) { return value != null && Number.isFinite(value) && value >= 0 && value <= 1 ? `${(value * 100).toFixed(1)}%` : t('upstreamOps.unknown') }
function cacheStatus(value: UpstreamRawCacheMetrics) { return value.requests === 0 ? 'empty' : value.observed_requests === 0 ? 'unknown' : value.observed_requests < value.requests ? 'partial' : 'observed' }
function waitRows(row: UpstreamRawCacheRow) {
 const waits = row.waits
 const ws = row.transport === 'ws_turn'
 if (waits?.version !== 1 || waits.source !== (ws ? 'canonical_physical_ws_turn_generation_start' : 'canonical_final_physical_http_attempt')) return []
 const keys = ws ? ['first_event', 'first_reasoning', 'first_text'] as const : ['first_byte', 'first_event', 'first_reasoning', 'first_text'] as const
 return keys.map(key => {
  const metric = waits[key]
  const mean = metric?.samples > 0 && typeof metric.mean_ms === 'number' && Number.isFinite(metric.mean_ms) && metric.mean_ms >= 0 ? metric.mean_ms : null
  return { key, mean, samples: metric?.samples, unknown: metric?.unknown_samples, coverage: metric?.coverage, latest: mean == null ? null : metric.latest_sample_at,
   status: mean == null ? 'unknown' : metric.unknown_samples > 0 ? 'partial' : 'waitObserved' }
 })
}
function formatMetric(m:UpstreamMetrics,key:typeof amountKeys[number]) {return number(m[key],0)}
async function load() {
 const id=++request; controller?.abort(); controller=new AbortController(); loading.value=true; error.value=''; snapshot.value=null
 try { const next=await readOperations({...applied.value},controller.signal); if(alive && request===id) snapshot.value=next }
 catch { if(alive && request===id) error.value=t('upstreamOps.queryFailed') }
 finally { if(alive && request===id) loading.value=false }
}
function apply() { const a=new Date(from.value), b=new Date(until.value); if(!Number.isFinite(a.getTime())||!Number.isFinite(b.getTime())||b<=a||b.getTime()-a.getTime()>31*86400000){error.value=t('upstreamOps.invalidRange');return} applied.value={...draft.value,from:a.toISOString(),until:b.toISOString(),page:1};void load() }
function last24h(){const end=new Date();applied.value={...draft.value,from:new Date(end.getTime()-86400000).toISOString(),until:end.toISOString(),page:1};from.value=local(new Date(applied.value.from));until.value=local(end);void load()}
function page(delta:number){applied.value={...applied.value,page:applied.value.page+delta};void load()}
function drill(row:OperationsRow){if(applied.value.dimension==='upstream'){applied.value={...applied.value,upstream:row.key,account_id:0,dimension:'account',page:1}}else if(applied.value.dimension==='account'){applied.value={...applied.value,account_id:Number(row.key),dimension:'model',page:1}}draft.value={...applied.value};void load()}
function selectUpstream(key:string){applied.value={...applied.value,upstream:key,account_id:0,dimension:'account',page:1};draft.value={...applied.value};void load()}
function selectAccount(id:number){applied.value={...applied.value,account_id:id,dimension:'model',page:1};draft.value={...applied.value};void load()}
function overview(){applied.value={...applied.value,upstream:'',account_id:0,dimension:'upstream',page:1};draft.value={...applied.value};void load()}
onMounted(load)
onBeforeUnmount(()=>{alive=false;request++;controller?.abort()})
</script>
