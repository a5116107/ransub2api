<template>
  <details class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="shared-monitoring" @toggle="toggle">
    <summary class="cursor-pointer font-medium">{{ t(`${p}.title`) }}</summary>
    <p class="text-sm leading-6">{{ t(`${p}.intro`) }}</p>
    <form class="min-w-0 space-y-3" @submit.prevent="preview">
      <fieldset :disabled="busy || disabled" class="min-w-0 space-y-3">
        <legend class="sr-only">{{ t(`${p}.title`) }}</legend>
        <div v-for="(row, i) in rows" :key="row.key" class="grid min-w-0 items-end gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]" data-testid="shared-selection">
          <div class="min-w-0">
            <label :for="`shared-product-${row.key}`" class="mb-1 block text-sm">{{ t(`${p}.product`, { n: i + 1 }) }}</label>
            <Select :id="`shared-product-${row.key}`" v-model="row.product_id" :options="productOptions" :aria-label="t(`${p}.product`, { n: i + 1 })" :searchable="true" :remote="true" :loading="loading" @open="emit('load')" @search="emit('search', $event)">
              <template v-if="row.product_id" #selected>{{ savedNames.get(row.product_id) ?? `#${row.product_id}` }}</template>
            </Select>
          </div>
          <div class="min-w-0">
            <label :for="`shared-preset-${row.key}`" class="mb-1 block text-sm">{{ t(`${p}.preset`, { n: i + 1 }) }}</label>
            <select :id="`shared-preset-${row.key}`" v-model="row.preset_id" class="input w-full" :aria-label="t(`${p}.preset`, { n: i + 1 })" @focus="emit('load')">
              <option :value="null">{{ t(`${p}.choosePreset`) }}</option>
              <option v-for="preset in presets" :key="preset.id" :value="preset.id">{{ preset.name }} · #{{ preset.id }}</option>
            </select>
          </div>
          <button type="button" class="btn btn-secondary" :disabled="rows.length <= 1" :aria-label="t(`${p}.removeRow`, { n: i + 1 })" data-testid="shared-remove" @click="rows.splice(i, 1)">{{ t(`${p}.remove`) }}</button>
        </div>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary" :disabled="rows.length >= 20" data-testid="shared-add" @click="add">{{ t(`${p}.add`) }}</button>
          <button type="submit" class="btn btn-primary" :disabled="!selections || loading" data-testid="shared-preview">{{ t(`${p}.${busy ? 'loading' : 'preview'}`) }}</button>
        </div>
      </fieldset>
      <p v-if="duplicate" role="alert" class="text-sm text-amber-800 dark:text-amber-300">{{ t(`${p}.duplicate`) }}</p>
      <p v-if="loadError" role="alert" class="text-sm">{{ t(`${p}.loadFailed`) }} <button type="button" class="underline" @click="emit('load')">{{ t(`${p}.retry`) }}</button></p>
    </form>
    <p v-if="error" role="alert" data-testid="shared-error" class="text-sm text-amber-800 dark:text-amber-300">{{ t(`${p}.${error}`) }}</p>
    <section v-if="result" class="min-w-0 space-y-3" aria-live="polite" data-testid="shared-result">
      <p class="rounded-lg bg-gray-100 p-3 text-sm font-medium dark:bg-dark-700" data-testid="shared-conclusion" :data-state="expired ? 'expired' : result.state">{{ t(`${p}.states.${expired ? 'expired' : result.state}`) }}</p>
      <p class="break-words text-xs">{{ t(`${p}.observed`, { at: result.observed_at }) }}</p>
      <dl class="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
        <div v-for="[key, value] in metrics" :key="key"><dt class="text-xs text-gray-500">{{ t(`${p}.${key}`) }}</dt><dd class="font-semibold tabular-nums" :data-testid="`shared-${key}`">{{ value }}</dd></div>
      </dl>
      <ul class="min-w-0 space-y-2 text-sm">
        <li v-for="product in result.products" :key="product.product_id" class="break-words" data-testid="shared-product-result">{{ product.name }} · #{{ product.product_id }} · {{ t(`${p}.sources.${expired ? 'expired' : product.state}`, { count: product.accounts }) }}</li>
      </ul>
      <div class="space-y-1 rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-800" data-testid="shared-budget" :data-state="budgetState">
        <p class="font-medium">{{ t(`${p}.budget.${budgetState}`) }}</p>
        <p v-if="result.coverage" class="break-words tabular-nums">{{ t(`${p}.budget.counts`, { current: result.current_minimum_requests_per_day, delta: result.delta_minimum_requests_per_day, total: result.coverage.minimum_total_requests_per_day, capacity: result.coverage.regular_day_capacity }) }}</p>
        <p class="text-xs leading-5">{{ t(`${p}.budget.boundary`) }}</p>
        <a href="/admin/settings?tab=gateway#settings-section-upstream-billing-probe" class="text-primary-600 underline dark:text-primary-400">{{ t(`${p}.budget.configure`) }}</a>
      </div>
      <ul class="min-w-0 space-y-3 text-sm">
        <li v-for="account in accountPage" :key="account.account_id" class="min-w-0 break-words rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="shared-account-result">
          <p class="font-medium">{{ account.name }} · #{{ account.account_id }} · {{ t(`${p}.reasons.${expired ? 'expired' : account.reason}`) }}</p>
          <p>{{ t(`${p}.accountProducts`, { ids: account.product_ids.join(', ') }) }}</p>
          <p v-if="account.models.length" class="break-all">{{ t(`${p}.models`) }}: {{ account.models.join(', ') }}</p>
          <p v-if="account.cron_expression" class="break-all">{{ t(`${p}.cron`) }}: <code>{{ account.cron_expression }}</code></p>
          <p v-if="account.existing_plan_id">{{ t(`${p}.existingPlan`, { id: account.existing_plan_id }) }} · {{ t(`${p}.${account.existing_enabled ? 'enabled' : 'paused'}`) }}</p>
        </li>
      </ul>
      <nav v-if="result.accounts.length > 20" class="flex flex-wrap items-center gap-3 text-sm" :aria-label="t(`${p}.pages`)">
        <button type="button" class="btn btn-secondary" :disabled="page <= 1" @click="page--">{{ t('upstreamOps.previous') }}</button>
        <span>{{ page }} / {{ Math.ceil(result.accounts.length / 20) }}</span>
        <button type="button" class="btn btn-secondary" :disabled="page * 20 >= result.accounts.length" @click="page++">{{ t('upstreamOps.next') }}</button>
      </nav>
    </section>
    <p class="text-xs leading-6 text-gray-500">{{ t(`${p}.boundary`) }}</p>
  </details>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import type { QualityRulePreset } from '@/api/admin/accountQuality'
import { previewProcurementSharedMonitoring, type ProcurementMonitoringSelection, type ProcurementSharedMonitoringPreview } from '@/api/admin/procurementCatalog'
import { validProcurementSharedMonitoringPreview } from '@/utils/procurementProductImpact'

const props = defineProps<{ products: Array<{ id: number; name: string }>; presets: QualityRulePreset[]; loading: boolean; loadError: boolean; disabled: boolean; initialProductId?: number | null; initialPresetId?: number | null }>()
const emit = defineEmits<{ load: []; search: [search: string] }>()
const { t } = useI18n(), p = 'upstreamOps.sharedMonitoring'
let key = 0, generation = 0, controller: AbortController | undefined, alive = true
const rows = ref<Array<{ key: number; product_id: number | null; preset_id: number | null }>>([{ key: key++, product_id: null, preset_id: null }, { key: key++, product_id: null, preset_id: null }])
const savedNames = ref(new Map<number, string>())
watch(() => props.products, products => {
  const names = new Map<number, string>()
  for (const row of rows.value) if (row.product_id && savedNames.value.has(row.product_id)) names.set(row.product_id, savedNames.value.get(row.product_id)!)
  for (const row of products) names.set(row.id, row.name)
  savedNames.value = names
}, { immediate: true })
const productOptions = computed(() => props.products.map(p => ({ value: p.id, label: p.name })))
const duplicate = computed(() => { const ids = rows.value.flatMap(r => r.product_id ? [r.product_id] : []); return new Set(ids).size !== ids.length })
const selections = computed<ProcurementMonitoringSelection[] | null>(() => duplicate.value || rows.value.some(r => !r.product_id || !r.preset_id) ? null : rows.value.map(r => ({ product_id: r.product_id!, preset_id: r.preset_id! })))
const busy = ref(false), error = ref(''), result = ref<ProcurementSharedMonitoringPreview | null>(null), page = ref(1)
const clock = ref(performance.now()), started = ref(0)
const expired = computed(() => clock.value - started.value >= 30000)
const timer = setInterval(() => { clock.value = performance.now() }, 1000)
const accountPage = computed(() => result.value?.accounts.slice((page.value - 1) * 20, page.value * 20) ?? [])
const metrics = computed(() => result.value ? [['products', result.value.products.length], ['requirements', result.value.requirements], ['accounts', result.value.accounts.length], ['shared', result.value.shared_accounts]] as const : [])
const budgetState = computed(() => {
  if (expired.value) return 'expired'
  const c = result.value?.coverage
  if (!c) return 'unknown'
  if (!c.enforced) return 'off'
  if (!c.can_save) return 'blocked'
  return c.inventory_complete !== true || c.plan_inventory_complete !== true || c.unestimated_demand_sources?.length || c.warnings.length ? 'partial' : 'fits'
})
function invalidate() { generation++; controller?.abort(); busy.value = false; error.value = ''; result.value = null; page.value = 1 }
watch(rows, invalidate, { deep: true, flush: 'sync' })
watch(() => props.presets, invalidate)
watch(() => props.disabled, value => { if (value) invalidate() })
function add() { if (!busy.value && !props.disabled && rows.value.length < 20) rows.value.push({ key: key++, product_id: null, preset_id: rows.value[0]?.preset_id ?? null }) }
function toggle(event: Event) {
  if (!(event.currentTarget as HTMLDetailsElement).open) { invalidate(); return }
  if (rows.value.every(r => r.product_id === null && r.preset_id === null)) {
    rows.value[0]!.product_id = props.initialProductId ?? null
    for (const row of rows.value) row.preset_id = props.initialPresetId ?? null
  }
  emit('load')
}
async function preview() {
  if (!selections.value || busy.value || props.loading || props.disabled) return
  const selected = selections.value.map(r => ({ ...r }))
  invalidate(); const version = generation; controller = new AbortController(); busy.value = true
  started.value = performance.now(); clock.value = started.value
  try {
    const next = await previewProcurementSharedMonitoring(selected, controller.signal)
    if (!alive || generation !== version) return
    if (!validProcurementSharedMonitoringPreview(next, selected)) throw new Error('invalid shared monitoring preview')
    clock.value = performance.now(); result.value = next
  } catch (e) { if (alive && generation === version) error.value = (e as { status?: number })?.status === 403 ? 'forbidden' : 'failed' }
  finally { if (alive && generation === version) busy.value = false }
}
onBeforeUnmount(() => { alive = false; generation++; controller?.abort(); clearInterval(timer) })
</script>
