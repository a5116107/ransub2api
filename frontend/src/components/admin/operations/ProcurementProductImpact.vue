<template>
  <section class="card min-w-0 space-y-4 p-4" aria-labelledby="product-impact-title" data-testid="product-impact">
    <header>
      <h2 id="product-impact-title" class="font-semibold">{{ t(`${p}.title`) }}</h2>
      <p class="mt-1 text-sm leading-6 text-gray-500">{{ t(`${p}.intro`) }}</p>
    </header>
    <form class="grid min-w-0 items-end gap-3 sm:grid-cols-[minmax(0,1fr)_auto]" @submit.prevent="preview">
      <div class="min-w-0">
        <label for="impact-product" class="mb-1 block text-sm font-medium">{{ t(`${p}.product`) }}</label>
        <Select id="impact-product" v-model="productID" :options="options" :aria-label="t(`${p}.product`)" :placeholder="t(`${p}.choose`)"
          :searchable="true" :remote="true" :loading="productsBusy" :empty-text="t(`${p}.noProducts`)" aria-describedby="impact-product-help"
          @open="openProducts" @search="loadProducts" />
      </div>
      <button type="submit" class="btn btn-secondary" :disabled="busy || !productID" data-testid="product-impact-preview">{{ t(`${p}.${busy ? 'loading' : 'preview'}`) }}</button>
    </form>
    <p id="impact-product-help" class="text-xs leading-5 text-gray-500">{{ t(`${p}.readOnly`) }}</p>
    <p v-if="productsTotal > products.length" class="text-xs text-gray-500">{{ t(`${p}.searchMore`, { count: products.length, total: productsTotal }) }}</p>
    <div v-if="productsError" class="flex flex-wrap items-center gap-2 text-sm" role="alert">
      <span class="text-red-600 dark:text-red-400">{{ t(`${p}.productsFailed`) }}</span>
      <button type="button" class="btn btn-secondary" :disabled="productsBusy" @click="loadProducts(lastSearch)">{{ t(`${p}.retry`) }}</button>
    </div>
    <p v-if="loadedProducts && !productsBusy && !productsError && !products.length" class="text-sm" role="status">{{ t(`${p}.noProducts`) }}</p>
    <div v-if="error" role="alert" class="space-y-2 text-sm text-red-600 dark:text-red-400" data-testid="product-impact-error">
      <p>{{ t(`${p}.${error}`) }}</p>
      <button type="button" class="btn btn-secondary" :disabled="busy || !productID" @click="preview">{{ t(`${p}.retry`) }}</button>
    </div>
    <section v-if="result" class="min-w-0 space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600" aria-live="polite" data-testid="product-impact-result">
      <div class="flex flex-wrap items-start justify-between gap-2">
        <h3 class="min-w-0 break-words font-medium">{{ result.report.group_name }} <span class="text-sm text-gray-500">#{{ result.report.group_id }} · {{ result.report.platform }}</span></h3>
        <span class="rounded-full bg-gray-100 px-3 py-1 text-sm dark:bg-dark-700" data-testid="product-impact-gate">{{ t(`${p}.${result.gate_enabled ? 'gateOn' : 'gateOff'}`) }}</span>
      </div>
      <p class="break-words text-xs text-gray-500">{{ t(`${p}.snapshot`, { time: result.observed_at }) }}</p>
      <p v-if="result.group_status !== 'active'" class="rounded-lg bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-900/20 dark:text-amber-200">{{ t(`${p}.inactive`) }}</p>
      <p v-if="!result.report.effective_gate" class="text-sm" data-testid="product-impact-unsupported">{{ t(`${p}.unsupported`) }}</p>
      <template v-else>
        <p class="rounded-lg p-3 text-sm font-medium" :class="admitted ? 'bg-primary-50 text-primary-800 dark:bg-primary-900/20 dark:text-primary-200' : 'bg-amber-50 text-amber-900 dark:bg-amber-900/20 dark:text-amber-200'" data-testid="product-impact-conclusion">
          {{ t(`${p}.${!result.considered_accounts ? 'empty' : !admitted ? 'allRejected' : 'priceRemaining'}`, { admitted, total: result.considered_accounts }) }}
        </p>
        <dl class="grid min-w-0 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <div v-for="[key, value] in priceMetrics" :key="key" class="min-w-0 rounded-lg bg-gray-50 p-3 dark:bg-dark-800">
            <dt class="text-xs text-gray-500">{{ t(`${p}.${key}`) }}</dt><dd class="mt-1 break-all text-lg font-semibold tabular-nums">{{ formatRate(value) }}</dd>
          </div>
        </dl>
        <p class="text-sm" data-testid="product-impact-overrides">{{ t(`${p}.overrides`, { count: result.user_override_count, admitted: minAdmitted, total: result.considered_accounts }) }}</p>
        <p v-if="admitted && !minAdmitted" class="text-sm font-medium text-amber-800 dark:text-amber-300">{{ t(`${p}.overrideEmpty`) }}</p>
        <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.candidateScope`, { total: result.bound_accounts, considered: result.considered_accounts }) }}</p>
        <ul class="grid min-w-0 gap-3 lg:grid-cols-2">
          <li v-for="row in pageRows" :key="row.account_id" class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="product-impact-account">
            <p class="break-words text-sm font-medium">{{ row.name }} <span class="text-gray-500">#{{ row.account_id }}</span></p>
            <span class="inline-block max-w-full break-words rounded-full px-2 py-1 text-xs" :class="row.class === 'admitted' ? 'bg-primary-50 text-primary-800 dark:bg-primary-900/20 dark:text-primary-200' : 'bg-amber-50 text-amber-900 dark:bg-amber-900/20 dark:text-amber-200'">{{ t(`${p}.classes.${row.class}`) }}</span>
            <dl class="grid min-w-0 grid-cols-2 gap-3 text-sm">
              <div><dt class="text-xs text-gray-500">{{ t(`${p}.admissionU`) }}</dt><dd class="break-all font-semibold tabular-nums" data-testid="product-impact-u">{{ formatRate(row.effective_upstream_rate) }}</dd><dd class="text-xs text-gray-500">{{ t(`${p}.sources.${row.rate_source}`) }}</dd></div>
              <div><dt class="text-xs text-gray-500">{{ t(`${p}.estimateC`) }}</dt><dd class="break-all font-semibold tabular-nums" data-testid="product-impact-c">{{ formatRate(row.estimated_cost_rate) }}</dd><dd class="text-xs text-gray-500">{{ t(`${p}.estimates.${row.estimated_cost_source}`) }}</dd></div>
            </dl>
            <p v-if="row.rejected_under_min_d" class="text-xs text-amber-800 dark:text-amber-300">{{ t(`${p}.rejectedAtMin`) }}</p>
            <ul v-if="row.warnings?.length" class="space-y-1 text-xs text-amber-800 dark:text-amber-300">
              <li v-for="warning in row.warnings" :key="warning">{{ t(`${p}.warnings.${knownWarning(warning)}`) }}</li>
            </ul>
          </li>
        </ul>
        <nav v-if="result.report.verdicts.length > pageSize" class="flex flex-wrap items-center gap-3 text-sm" :aria-label="t(`${p}.pages`)">
          <button type="button" class="btn btn-secondary" :disabled="page <= 1" @click="page--">{{ t('upstreamOps.previous') }}</button>
          <span>{{ page }} / {{ Math.ceil(result.report.verdicts.length / pageSize) }}</span>
          <button type="button" class="btn btn-secondary" :disabled="page * pageSize >= result.report.verdicts.length" @click="page++">{{ t('upstreamOps.next') }}</button>
        </nav>
      </template>
      <p class="rounded-lg bg-gray-50 p-3 text-sm leading-6 dark:bg-dark-800" data-testid="product-impact-boundary">{{ t(`${p}.boundary`) }}</p>
    </section>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import { list as listGroups } from '@/api/admin/groups'
import { previewProcurementProductImpact, type ProcurementProductImpact } from '@/api/admin/procurementCatalog'
import { validProcurementProductImpact } from '@/utils/procurementProductImpact'
import type { AdminGroup } from '@/types'

const { t } = useI18n(), p = 'upstreamOps.productImpact'
const productID = ref<number | null>(null), products = ref<AdminGroup[]>([]), productsTotal = ref(0)
const loadedProducts = ref(false), productsBusy = ref(false), productsError = ref(false)
const result = ref<ProcurementProductImpact | null>(null), busy = ref(false), error = ref(''), page = ref(1), pageSize = 20
let alive = true, productsGeneration = 0, previewGeneration = 0, lastSearch = ''
let productsController: AbortController | undefined, previewController: AbortController | undefined
const selected = ref<AdminGroup | null>(null)
const options = computed(() => {
  const rows = products.value.slice()
  if (selected.value && !rows.some(row => row.id === selected.value?.id)) rows.unshift(selected.value)
  return rows.map(row => ({ value: row.id, label: `${row.name} · ${row.platform} #${row.id}${row.status === 'inactive' ? ' · ' + t(`${p}.disabledProduct`) : ''}` }))
})
const admitted = computed(() => result.value?.report.verdicts.filter(row => row.class === 'admitted').length ?? 0)
const minAdmitted = computed(() => result.value?.report.verdicts.filter(row => row.class === 'admitted' && !row.rejected_under_min_d).length ?? 0)
const pageRows = computed(() => result.value?.report.verdicts.slice((page.value - 1) * pageSize, page.value * pageSize) ?? [])
const priceMetrics = computed<Array<[string, number]>>(() => !result.value ? [] : [
  ['saleD', result.value.report.default_d], ['threshold', result.value.report.threshold_default],
  ['minimumD', result.value.report.min_effective_d], ['minimumThreshold', result.value.report.threshold_min_d]
])
const warnings = new Set(['probe_snapshot_missing', 'probe_snapshot_stale', 'probe_failed', 'probe_unsupported', 'probe_rate_evidence_invalid', 'probe_request_scope_not_evaluated', 'manual_rate_1_suspected_unmaintained'])
const knownWarning = (warning: string) => warnings.has(warning) ? warning : 'unknown'
const formatRate = (value: number | undefined) => value == null ? t('upstreamOps.unknown') : new Intl.NumberFormat(undefined, { maximumFractionDigits: 8 }).format(value)
watch(productID, id => {
  selected.value = products.value.find(row => row.id === id) ?? (selected.value?.id === id ? selected.value : null)
  previewController?.abort(); previewGeneration++; busy.value = false; result.value = null; error.value = ''; page.value = 1
}, { flush: 'sync' })
function openProducts() { if (!loadedProducts.value && !productsBusy.value) void loadProducts('') }
async function loadProducts(search: string) {
  lastSearch = search
  productsController?.abort(); productsController = new AbortController()
  const generation = ++productsGeneration
  productsBusy.value = true; productsError.value = false
  try {
    const next = await listGroups(1, 100, { search, sort_by: 'name', sort_order: 'asc' }, { signal: productsController.signal })
    if (!next || !Array.isArray(next.items) || !Number.isSafeInteger(next.total) || next.total < next.items.length ||
      next.items.some(row => !Number.isSafeInteger(row.id) || row.id < 1 || typeof row.name !== 'string' || typeof row.platform !== 'string')) throw new Error('invalid product list')
    if (alive && generation === productsGeneration) { products.value = next.items; productsTotal.value = next.total; loadedProducts.value = true }
  } catch { if (alive && generation === productsGeneration) productsError.value = true }
  finally { if (alive && generation === productsGeneration) productsBusy.value = false }
}
async function preview() {
  if (busy.value || !productID.value) return
  const id = productID.value, generation = ++previewGeneration
  previewController?.abort(); previewController = new AbortController()
  busy.value = true; error.value = ''; result.value = null; page.value = 1
  try {
    const next = await previewProcurementProductImpact(id, previewController.signal)
    if (!validProcurementProductImpact(next, id)) throw new Error('invalid product impact')
    if (alive && generation === previewGeneration) result.value = next
  } catch (e) {
    if (alive && generation === previewGeneration) {
      const status = (e as { status?: number })?.status, reason = (e as { reason?: string })?.reason
      error.value = status === 403 ? 'forbidden' : status === 404 ? 'notFound' : reason === 'PROCUREMENT_PRODUCT_IMPACT_TOO_LARGE' ? 'tooLarge' : 'failed'
    }
  } finally { if (alive && generation === previewGeneration) busy.value = false }
}
onBeforeUnmount(() => { alive = false; productsController?.abort(); previewController?.abort() })
</script>
