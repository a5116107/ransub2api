<template>
  <details class="card min-w-0 p-4" data-testid="procurement-manager" @toggle="toggle">
    <summary class="cursor-pointer font-semibold">{{ t(`${p}.title`) }}</summary>
    <div class="mt-4 min-w-0 space-y-4">
      <p class="text-sm leading-6 text-gray-600 dark:text-gray-300">{{ t(`${p}.scope`) }}</p>
      <p v-if="error" role="alert" class="break-words text-sm text-red-600 dark:text-red-400" data-testid="procurement-error">{{ t(`${p}.${error}`) }}</p>
      <p v-if="busy" role="status" class="text-sm">{{ t('upstreamOps.loading') }}</p>
      <button v-if="!saved" type="button" class="btn btn-secondary" :disabled="busy" @click="load">{{ t(`${p}.load`) }}</button>
      <template v-else>
        <p class="text-xs text-gray-500" data-testid="procurement-revision">{{ t(`${p}.revision`, { revision: saved.catalog.revision }) }}</p>
        <button type="button" class="btn btn-secondary text-sm" :disabled="busy" data-testid="catalog-reload" @click="load">{{ t(`${p}.reload`) }}</button>
        <p v-if="!saved.catalog.buckets.length" role="status" class="text-sm">{{ t(`${p}.empty`) }}</p>
        <div v-if="saved.gaps.length" class="space-y-1 rounded-lg bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-900/20 dark:text-amber-200" data-testid="procurement-gaps">
          <p class="font-medium">{{ t(`${p}.gaps`) }}</p>
          <ul class="flex flex-wrap gap-x-4 gap-y-1"><li v-for="(gap, index) in saved.gaps" :key="index">[{{ gap.min_rate ?? '0' }}, {{ gap.max_rate ?? '∞' }})</li></ul>
        </div>
        <form class="space-y-3" @submit.prevent="save">
          <fieldset :disabled="busy" class="min-w-0 space-y-3">
            <legend class="mb-2 text-sm font-medium">{{ t(`${p}.edit`) }}</legend>
            <div v-for="(row, index) in rows" :key="row.key" class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="procurement-bucket">
              <div class="grid min-w-0 gap-3 sm:grid-cols-2">
                <label class="min-w-0 text-sm">{{ t(`${p}.code`) }}<input v-model="row.code" class="input mt-1 w-full" maxlength="64" required data-testid="bucket-code" /></label>
                <label class="min-w-0 text-sm">{{ t(`${p}.name`) }}<input v-model="row.name" class="input mt-1 w-full" maxlength="100" required data-testid="bucket-name" /></label>
                <label class="min-w-0 text-sm">{{ t(`${p}.min`) }}<input v-model="row.min" class="input mt-1 w-full" inputmode="decimal" maxlength="17" placeholder="0" data-testid="bucket-min" :aria-invalid="Boolean(validation)" /></label>
                <label class="min-w-0 text-sm">{{ t(`${p}.max`) }}<input v-model="row.max" class="input mt-1 w-full" inputmode="decimal" maxlength="17" :disabled="row.unbounded" :required="!row.unbounded" data-testid="bucket-max" :aria-invalid="Boolean(validation)" /></label>
              </div>
              <div class="flex flex-wrap items-center justify-between gap-3">
                <label class="flex items-start gap-2 text-sm"><input v-model="row.unbounded" type="checkbox" class="mt-1" data-testid="bucket-unbounded" />{{ t(`${p}.unbounded`) }}</label>
                <button type="button" class="btn btn-secondary text-sm" :aria-label="t(`${p}.removeNamed`, { name: row.name || index + 1 })" @click="rows.splice(index, 1)">{{ t(`${p}.remove`) }}</button>
              </div>
            </div>
            <p v-if="validation" role="alert" class="text-sm text-amber-800 dark:text-amber-300" data-testid="procurement-validation">{{ t(`${p}.${validation}`) }}</p>
            <div class="flex flex-wrap gap-2">
              <button type="button" class="btn btn-secondary" :disabled="rows.length >= 64" data-testid="bucket-add" @click="add">{{ t(`${p}.add`) }}</button>
              <button type="submit" class="btn btn-primary" :disabled="!dirty || Boolean(validation) || conflict" data-testid="catalog-save">{{ t(`${p}.save`) }}</button>
            </div>
          </fieldset>
        </form>
        <div v-if="conflict" role="alert" class="space-y-3 rounded-lg bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-900/20 dark:text-amber-200" data-testid="catalog-conflict">
          <p>{{ t(`${p}.conflict`) }}</p>
          <p v-if="latest">{{ t(`${p}.revision`, { revision: latest.catalog.revision }) }}</p>
          <ul v-if="latest" class="space-y-1"><li v-for="bucket in latest.catalog.buckets" :key="bucket.code" class="break-words">{{ bucket.code }} · {{ bucket.name }} [{{ bucket.min_rate ?? '0' }}, {{ bucket.max_rate ?? '∞' }})</li></ul>
          <p v-if="latest && !latest.catalog.buckets.length">{{ t(`${p}.empty`) }}</p>
          <div class="flex flex-wrap gap-2">
            <button type="button" class="btn btn-secondary" :disabled="busy" data-testid="catalog-read-latest" @click="readLatest">{{ t(`${p}.loadLatest`) }}</button>
            <button v-if="latest" type="button" class="btn btn-secondary" :disabled="busy" data-testid="catalog-review" @click="review">{{ t(`${p}.review`) }}</button>
          </div>
        </div>
        <p v-if="success" role="status" class="text-sm text-green-700 dark:text-green-400">{{ t(`${p}.saved`) }}</p>
        <form class="min-w-0 space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600" @submit.prevent="preview">
          <fieldset :disabled="busy || dirty || conflict" class="min-w-0 space-y-3">
            <legend class="mb-2 text-sm font-medium">{{ t(`${p}.preview`) }}</legend>
            <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.previewHint`) }}</p>
            <p v-if="dirty || conflict" class="text-sm text-amber-800 dark:text-amber-300">{{ t(`${p}.saveBeforePreview`) }}</p>
            <div class="grid min-w-0 gap-3 sm:grid-cols-2 lg:grid-cols-3">
              <label class="min-w-0 text-sm">{{ t(`${p}.product`) }}<input v-model="probe.product" inputmode="numeric" maxlength="16" class="input mt-1 w-full" data-testid="procurement-product" aria-describedby="procurement-product-hint" :aria-invalid="Number.isNaN(productID)" /></label>
              <label class="min-w-0 text-sm">{{ t('upstreamOps.account') }}<input v-model.number="probe.account" type="number" min="1" step="1" class="input mt-1 w-full" required data-testid="procurement-account" /></label>
              <label class="min-w-0 text-sm">{{ t('upstreamOps.public') }}<input v-model="probe.publicModel" class="input mt-1 w-full" maxlength="200" required data-testid="procurement-public-model" /></label>
              <label class="min-w-0 text-sm">{{ t(`${p}.wireModel`) }}<input v-model="probe.model" class="input mt-1 w-full" maxlength="200" required data-testid="procurement-wire-model" /></label>
              <label class="min-w-0 text-sm">{{ t(`${p}.protocol`) }}<select v-model="probe.protocol" class="input mt-1 w-full" data-testid="procurement-protocol"><option value="responses_http">Responses HTTP</option><option value="responses_ws_v2">Responses WS v2</option><option value="chat_http">Chat Completions</option></select></label>
              <label class="min-w-0 text-sm">{{ t(`${p}.tier`) }}<input v-model="probe.tier" class="input mt-1 w-full" maxlength="64" required data-testid="procurement-tier" /></label>
              <label class="min-w-0 text-sm">{{ t(`${p}.effort`) }}<input v-model="probe.effort" class="input mt-1 w-full" maxlength="64" required data-testid="procurement-effort" /></label>
              <label v-if="responses" class="min-w-0 text-sm">{{ t(`${p}.summary`) }}<select v-model="probe.summary" class="input mt-1 w-full"><option value="">{{ t(`${p}.unspecified`) }}</option><option v-for="value in ['none','auto','concise','detailed']" :key="value" :value="value">{{ value }}</option></select></label>
              <label v-if="responses" class="min-w-0 text-sm">{{ t(`${p}.verbosity`) }}<select v-model="probe.verbosity" class="input mt-1 w-full" data-testid="procurement-verbosity"><option value="">{{ t(`${p}.unspecified`) }}</option><option v-for="value in ['low','medium','high']" :key="value" :value="value">{{ value }}</option></select></label>
            </div>
            <p id="procurement-product-hint" class="text-xs leading-5 text-gray-500">{{ t(`${p}.productHint`) }}</p>
            <div class="flex flex-wrap gap-4 text-sm">
              <label class="flex items-start gap-2"><input v-model="probe.stream" type="checkbox" class="mt-1" :disabled="probe.protocol === 'responses_ws_v2'" />{{ t(`${p}.stream`) }}</label>
              <label v-if="responses" class="flex items-start gap-2"><input v-model="probe.encrypted" type="checkbox" class="mt-1" />{{ t(`${p}.encrypted`) }}</label>
            </div>
            <button type="submit" class="btn btn-secondary" :disabled="!validProbe" data-testid="catalog-preview">{{ t(`${p}.previewAction`) }}</button>
          </fieldset>
        </form>
        <section v-if="result" aria-live="polite" class="min-w-0 space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-800" data-testid="catalog-preview-result">
          <p class="break-words text-xs text-gray-500">{{ t(`${p}.revision`, { revision: result.catalog.revision }) }} · {{ result.observed_at }}</p>
          <div v-for="(item, index) in result.results" :key="index" class="min-w-0 space-y-2">
            <span class="inline-block max-w-full break-words rounded-full px-3 py-1 text-sm" :class="item.state === 'classified' ? 'bg-green-100 text-green-900 dark:bg-green-900/30 dark:text-green-200' : 'bg-amber-100 text-amber-900 dark:bg-amber-900/30 dark:text-amber-200'" data-testid="catalog-preview-state">{{ t(`${p}.states.${knownState(item.state)}`) }}</span>
            <p class="break-words text-sm">#{{ item.account_id }} · {{ item.public_model }} → {{ item.scope.model }} · {{ item.scope.protocol }} · {{ item.scope.service_tier }} · {{ item.scope.reasoning_effort }}</p>
            <dl class="grid gap-3 text-sm sm:grid-cols-2">
              <div><dt class="text-gray-500">{{ t(`${p}.effectiveRate`) }}</dt><dd class="break-all font-medium">{{ item.effective_rate ?? t('upstreamOps.unknown') }}</dd></div>
              <div><dt class="text-gray-500">{{ t(`${p}.rateSource`) }}</dt><dd>{{ t(`${p}.sources.${item.rate_source === 'manual' || item.rate_source === 'upstream_probe_sync' ? item.rate_source : 'unknown'}`) }}</dd></div>
              <div><dt class="text-gray-500">{{ t(`${p}.bucket`) }}</dt><dd class="break-words">{{ item.bucket ? `${item.bucket.code} · ${item.bucket.name} [${item.bucket.min_rate ?? '0'}, ${item.bucket.max_rate ?? '∞'})` : t('upstreamOps.unknown') }}</dd></div>
              <div><dt class="text-gray-500">{{ t(`${p}.applicability`) }}</dt><dd>{{ item.applicability === 'covered' ? t(`${p}.covered`) : t('upstreamOps.unknown') }}</dd></div>
            </dl>
            <p v-if="item.reason" class="break-all font-mono text-xs">{{ item.reason }}</p>
            <div v-if="item.product_procurement" class="min-w-0 space-y-2 border-t border-gray-200 pt-3 dark:border-dark-600" data-testid="product-procurement">
              <p class="break-words text-sm font-medium">{{ t(`${p}.productResult`, { id: item.product_procurement.group_id, revision: item.product_procurement.policy_revision }) }}</p>
              <span class="inline-block max-w-full break-words rounded-full px-3 py-1 text-sm" :class="item.product_procurement.state === 'constraints_passed' ? 'bg-green-100 text-green-900 dark:bg-green-900/30 dark:text-green-200' : 'bg-amber-100 text-amber-900 dark:bg-amber-900/30 dark:text-amber-200'" data-testid="product-procurement-state">{{ t(`${p}.productStates.${item.product_procurement.state}`) }}</span>
              <dl class="grid gap-3 text-sm sm:grid-cols-2">
                <div><dt class="text-gray-500">{{ t(`${p}.productSale`) }}</dt><dd>{{ item.product_procurement.default_d }}</dd></div>
                <div><dt class="text-gray-500">{{ t(`${p}.productThreshold`) }}</dt><dd>{{ item.product_procurement.threshold }}</dd></div>
              </dl>
              <p v-if="item.product_procurement.reason" class="break-all font-mono text-xs">{{ item.product_procurement.reason }}</p>
              <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.productDefaultBasis`) }}</p>
            </div>
          </div>
          <p class="text-sm leading-6 text-amber-800 dark:text-amber-300">{{ t(`${p}.notQualified`) }}</p>
        </section>
      </template>
    </div>
  </details>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { readProcurementCatalog, saveProcurementCatalog, previewProcurementScopes, type ProcurementCatalog, type ProcurementCatalogSnapshot, type ProcurementRequestScope, type ProcurementPreview } from '@/api/admin/procurementCatalog'
import { validProcurementSnapshot, validateProcurementCatalog } from '@/utils/procurementCatalog'

const { t } = useI18n(), p = 'upstreamOps.procurement'
const saved = ref<ProcurementCatalogSnapshot | null>(null), latest = ref<ProcurementCatalogSnapshot | null>(null)
const busy = ref(false), error = ref(''), conflict = ref(false), success = ref(false), result = ref<ProcurementPreview | null>(null)
let rowKey = 0, alive = true, generation = 0
const controller = new AbortController()
const rows = ref<Array<{ key: number; code: string; name: string; min: string; max: string; unbounded: boolean }>>([])
const probe = ref({ product: '', account: 0, publicModel: '', model: '', protocol: 'responses_http', tier: 'unspecified', effort: 'unspecified', stream: false, summary: '', verbosity: '', encrypted: false })
const productID = computed(() => probe.value.product.trim() === '' ? undefined : /^[1-9]\d*$/.test(probe.value.product.trim()) && Number.isSafeInteger(Number(probe.value.product)) ? Number(probe.value.product) : NaN)
const responses = computed(() => probe.value.protocol !== 'chat_http')
const candidate = computed<ProcurementCatalog>(() => ({ version: 1, revision: saved.value?.catalog.revision ?? 0, buckets: rows.value.map(row => ({ code: row.code, name: row.name, min_rate: row.min === '' ? null : row.min, max_rate: row.unbounded ? null : row.max })) }))
const validation = computed(() => validateProcurementCatalog(candidate.value))
const dirty = computed(() => Boolean(saved.value) && JSON.stringify(candidate.value) !== JSON.stringify(saved.value?.catalog))
const validProbe = computed(() => !Number.isNaN(productID.value) && Number.isSafeInteger(probe.value.account) && probe.value.account > 0 && probe.value.publicModel.trim() !== '' && probe.value.model.trim() !== '' && probe.value.tier !== '' && probe.value.effort !== '')
watch([rows, probe], () => { generation++; result.value = null; success.value = false }, { deep: true, flush: 'sync' })
function accept(next: ProcurementCatalogSnapshot) {
  if (!validProcurementSnapshot(next)) throw new Error('unsupported catalog response')
  return next
}
function adopt(next: ProcurementCatalogSnapshot) {
  saved.value = next
  rows.value = next.catalog.buckets.map(b => ({ key: ++rowKey, code: b.code, name: b.name, min: b.min_rate ?? '', max: b.max_rate ?? '', unbounded: b.max_rate === null }))
  conflict.value = false; latest.value = null; result.value = null
}
async function load() {
  if (busy.value) return
  busy.value = true; error.value = ''; result.value = null; success.value = false
  try { const next = accept(await readProcurementCatalog(controller.signal)); if (alive) adopt(next) }
  catch { if (alive) error.value = 'readFailed' }
  finally { if (alive) busy.value = false }
}
function toggle(event: Event) { if ((event.target as HTMLDetailsElement).open && !saved.value) void load() }
function add() { rows.value.push({ key: ++rowKey, code: '', name: '', min: '', max: '', unbounded: false }) }
async function readLatest() {
  if (busy.value) return
  busy.value = true; error.value = ''; latest.value = null; result.value = null
  try { const next = accept(await readProcurementCatalog(controller.signal)); if (alive) latest.value = next }
  catch { if (alive) error.value = 'readFailed' }
  finally { if (alive) busy.value = false }
}
function review() {
  if (!latest.value || busy.value) return
  saved.value = latest.value; latest.value = null; conflict.value = false; result.value = null; error.value = ''
}
async function save() {
  if (busy.value || !dirty.value || validation.value || conflict.value) return
  busy.value = true; error.value = ''; success.value = false; result.value = null
  try {
    const next = accept(await saveProcurementCatalog(candidate.value, controller.signal))
    if (alive) { adopt(next); success.value = true }
  } catch (e) {
    if (!alive) return
    const detail = e as { status?: number; reason?: string }
    if (detail.status === 409 && detail.reason === 'PROCUREMENT_CATALOG_CONFLICT') { conflict.value = true; latest.value = null }
    else error.value = 'saveFailed'
  } finally { if (alive) busy.value = false }
}
async function preview() {
  if (busy.value || dirty.value || conflict.value || !validProbe.value || !saved.value) return
  const options = responses.value ? { reasoning_summary: probe.value.summary || undefined, text_verbosity: probe.value.verbosity || undefined, include_encrypted_content: probe.value.encrypted || undefined } : {}
  const request: ProcurementRequestScope = { account_id: probe.value.account, public_model: probe.value.publicModel, scope: {
    version: options.text_verbosity ? 4 : options.reasoning_summary || options.include_encrypted_content ? 3 : 2,
    model: probe.value.model, protocol: probe.value.protocol, stream: probe.value.protocol === 'responses_ws_v2' || probe.value.stream,
    service_tier: probe.value.tier, reasoning_effort: probe.value.effort, capability: 'text', ...options
  } }
  const id = generation
  const product = productID.value
  busy.value = true; error.value = ''; result.value = null
  try {
    const next = await previewProcurementScopes({ requests: [request], ...(product === undefined ? {} : { product_group_id: product }) }, controller.signal)
    if (!validProcurementSnapshot(next) || next.catalog.revision !== saved.value.catalog.revision || !Number.isFinite(Date.parse(next.observed_at)) || !Array.isArray(next.results) || next.results.length !== 1 ||
      next.results[0]?.account_id !== request.account_id || next.results[0]?.public_model !== request.public_model || !next.results[0]?.scope ||
      !Object.entries(request.scope).every(([key, value]) => next.results[0]!.scope[key as keyof ProcurementRequestScope['scope']] === value) || next.results[0]?.qualification !== 'not_evaluated') throw new Error('unknown preview response')
    const projection = next.results[0]!.product_procurement
    if (product === undefined ? projection !== undefined : !projection || projection.version !== 1 || projection.group_id !== product || projection.price_basis !== 'group_default' ||
      !Number.isSafeInteger(projection.policy_revision) || projection.policy_revision < 0 || !Number.isFinite(projection.default_d) || projection.default_d < 0 || !Number.isFinite(projection.threshold) || projection.threshold < 0 ||
      !['constraints_passed', 'blocked', 'disabled', 'product_inactive', 'not_associated', 'not_evaluated'].includes(projection.state) ||
      projection.state === 'constraints_passed' && projection.price_class !== 'admitted') throw new Error('unknown product projection')
    if (alive && generation === id) result.value = next
  } catch { if (alive && generation === id) error.value = 'previewFailed' }
  finally { if (alive) busy.value = false }
}
function knownState(value: string) { return ['classified','unclassified','scope_unconfirmed','rate_unavailable','account_unavailable','not_configured'].includes(value) ? value : 'unknown' }
onBeforeUnmount(() => { alive = false; controller.abort() })
</script>
