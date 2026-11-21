<template>
  <details class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="shared-monitoring" @toggle="toggle">
    <summary class="cursor-pointer font-medium">{{ t(`${p}.title`) }}</summary>
    <p class="text-sm leading-6">{{ t(`${p}.intro`) }}</p>
    <div class="flex flex-wrap items-center gap-2 text-sm">
      <p v-if="savedState" data-testid="shared-saved-state">{{ t(`${p}.savedState`, { revision: savedState.revision, products: savedState.products.length, plans: savedState.plans.length }) }}</p>
      <button type="button" class="btn btn-secondary" :disabled="busy || disabled" data-testid="shared-reload" @click="readSaved">{{ t(`${p}.reload`) }}</button>
    </div>
    <p v-if="savedState?.plans.some(plan => !['managed', 'paused'].includes(plan.state))" role="alert" class="text-sm text-amber-800 dark:text-amber-300">{{ t(`${p}.ownershipConflict`) }}</p>
    <p v-if="success" role="status" data-testid="shared-success" class="text-sm text-emerald-700 dark:text-emerald-300">{{ t(`${p}.saved`) }}</p>
    <form class="min-w-0 space-y-3" @submit.prevent="preview">
      <fieldset :disabled="busy || disabled || !loaded" class="min-w-0 space-y-3">
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
          <button type="button" class="btn btn-secondary" :aria-label="t(`${p}.removeRow`, { n: i + 1 })" data-testid="shared-remove" @click="rows.splice(i, 1)">{{ t(`${p}.remove`) }}</button>
        </div>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary" :disabled="rows.length >= 20" data-testid="shared-add" @click="add">{{ t(`${p}.add`) }}</button>
          <button type="button" class="btn btn-secondary" :disabled="rows.length === 0" data-testid="shared-clear" @click="rows = []">{{ t(`${p}.clear`) }}</button>
          <button type="submit" class="btn btn-primary" :disabled="!loaded || !selections || loading" data-testid="shared-preview">{{ t(`${p}.${busy ? 'loading' : 'preview'}`) }}</button>
        </div>
        <p v-if="rows.length === 0" class="text-sm">{{ t(`${p}.pauseAll`) }}</p>
        <label v-if="rows.length === 0" class="flex items-start gap-2 text-sm"><input v-model="releaseOwnership" type="checkbox" data-testid="shared-release" class="mt-1" />{{ t(`${p}.release`) }}</label>
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
      <section v-if="result.application_review" class="space-y-3 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600" data-testid="shared-application">
        <p class="font-medium">{{ t(`${p}.changesTitle`) }}</p>
        <p>{{ t(`${p}.changeCounts`, changeCounts) }}</p>
        <ul class="max-h-60 space-y-1 overflow-auto" :aria-label="t(`${p}.changesTitle`)">
          <li v-for="change in result.application_review.changes" :key="change.account_id" class="break-words">#{{ change.account_id }} · {{ t(`${p}.operations.${change.operation}`) }}<template v-if="change.plan_id"> · {{ t(`${p}.existingPlan`, { id: change.plan_id }) }}</template><template v-if="change.reason !== 'compatible'"> · {{ t(`${p}.reasons.${change.reason}`) }}</template></li>
        </ul>
        <p>{{ t(`${p}.replaceAll`) }}</p>
        <p class="break-words">{{ t(`${p}.expires`, { at: result.application_review.expires_at }) }}</p>
        <label class="flex items-start gap-2"><input v-model="consent" type="checkbox" :disabled="!canSave || saving" data-testid="shared-consent" class="mt-1" />{{ t(`${p}.consent`) }}</label>
        <button type="button" class="btn btn-primary" :disabled="!canSave || !consent || busy || disabled" data-testid="shared-save" @click="save">{{ t(`${p}.${saving ? 'saving' : 'save'}`) }}</button>
      </section>
    </section>
    <p class="text-xs leading-6 text-gray-500">{{ t(`${p}.boundary`) }}</p>
  </details>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import type { QualityRulePreset } from '@/api/admin/accountQuality'
import { applyProcurementSharedMonitoring, readProcurementSharedMonitoring, previewProcurementSharedMonitoring, type ProcurementMonitoringSelection, type ProcurementSharedMonitoringPreview, type ProcurementSharedMonitoringState } from '@/api/admin/procurementCatalog'
import { validProcurementSharedMonitoringPreview, validProcurementSharedMonitoringState } from '@/utils/procurementProductImpact'

const props = defineProps<{ products: Array<{ id: number; name: string }>; presets: QualityRulePreset[]; loading: boolean; loadError: boolean; disabled: boolean; initialProductId?: number | null; initialPresetId?: number | null }>()
const emit = defineEmits<{ load: []; search: [search: string] }>()
const { t } = useI18n(), p = 'upstreamOps.sharedMonitoring'
let key = 0, generation = 0, controller: AbortController | undefined, alive = true
let readGeneration = 0, readController: AbortController | undefined
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
const previewBusy = ref(false), reading = ref(false), saving = ref(false), loaded = ref(false), success = ref(false)
const busy = computed(() => previewBusy.value || reading.value || saving.value)
const savedState = ref<ProcurementSharedMonitoringState | null>(null), releaseOwnership = ref(false), consent = ref(false)
const error = ref(''), result = ref<ProcurementSharedMonitoringPreview | null>(null), page = ref(1)
const clock = ref(performance.now()), started = ref(0)
const expired = computed(() => clock.value - started.value >= 30000)
watch(expired, value => { if (value) consent.value = false }, { flush: 'sync' })
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
const canSave = computed(() => loaded.value && !expired.value && result.value?.version === 2 && result.value.application_review?.can_confirm === true && result.value.application_review.revision === savedState.value?.revision)
const changeCounts = computed(() => {
  const changes = result.value?.application_review?.changes ?? []
  return { create: changes.filter(c => c.operation === 'create').length, update: changes.filter(c => c.operation === 'update').length, pause: changes.filter(c => ['pause', 'release'].includes(c.operation)).length, retain: changes.filter(c => ['retain', 'retain_paused'].includes(c.operation)).length, conflict: changes.filter(c => c.operation === 'conflict').length }
})
function invalidate() { generation++; controller?.abort(); previewBusy.value = false; consent.value = false; error.value = ''; success.value = false; result.value = null; page.value = 1 }
watch(rows, invalidate, { deep: true, flush: 'sync' })
watch(releaseOwnership, invalidate, { flush: 'sync' })
watch(() => rows.value.length, n => { if (n > 0) releaseOwnership.value = false }, { flush: 'sync' })
watch(() => props.presets, () => { if (loaded.value) invalidate() })
watch(() => props.disabled, value => { if (value) invalidate() })
function add() { if (!busy.value && !props.disabled && rows.value.length < 20) rows.value.push({ key: key++, product_id: null, preset_id: rows.value[0]?.preset_id ?? null }) }
function toggle(event: Event) {
  if (!(event.currentTarget as HTMLDetailsElement).open) { if (!saving.value) invalidate(); return }
  if (!loaded.value && !busy.value) void readSaved()
  emit('load')
}
async function readSaved() {
  if (busy.value || props.disabled) return
  invalidate(); loaded.value = false; reading.value = true
  const current = ++readGeneration; readController = new AbortController()
  try {
    const next = await readProcurementSharedMonitoring(readController.signal)
    if (!alive || current !== readGeneration) return
    if (!validProcurementSharedMonitoringState(next)) throw new Error('invalid saved participation')
    savedState.value = next
    rows.value = next.products.length ? next.products.map(p => ({ key: key++, ...p })) : [
      { key: 0, product_id: props.initialProductId ?? null, preset_id: props.initialPresetId ?? null },
      { key: 1, product_id: null, preset_id: props.initialPresetId ?? null }
    ]
    key = Math.max(key, 2); releaseOwnership.value = false; loaded.value = true
  } catch (e) { if (alive && current === readGeneration) { savedState.value = null; error.value = (e as { status?: number })?.status === 403 ? 'forbidden' : 'readFailed' } }
  finally { if (alive && current === readGeneration) reading.value = false }
}
async function preview() {
  if (!loaded.value || !selections.value || busy.value || props.loading || props.disabled) return
  const selected = selections.value.map(r => ({ ...r }))
  invalidate(); const version = generation; controller = new AbortController(); previewBusy.value = true
  started.value = performance.now(); clock.value = started.value
  try {
    const next = await previewProcurementSharedMonitoring(selected, controller.signal, releaseOwnership.value)
    if (!alive || generation !== version) return
    if (!validProcurementSharedMonitoringPreview(next, selected, releaseOwnership.value) || next.version === 2 && next.application_review?.revision !== savedState.value?.revision) throw new Error('invalid shared monitoring preview')
    clock.value = performance.now(); result.value = next
  } catch (e) { if (alive && generation === version) error.value = (e as { status?: number })?.status === 403 ? 'forbidden' : 'failed' }
  finally { if (alive && generation === version) previewBusy.value = false }
}
async function save() {
  const review = result.value?.application_review, selected = selections.value?.map(p => ({ ...p }))
  if (!canSave.value || !consent.value || !review || !selected || busy.value || props.disabled) return
  saving.value = true; consent.value = false; error.value = ''; success.value = false
  let written = false
  try {
    const receipt = await applyProcurementSharedMonitoring({ version: 2, products: selected, release_ownership: releaseOwnership.value, revision: review.revision, observed_at: review.observed_at, fingerprint: review.fingerprint, confirm: true })
    written = true
    const expected = review.changes.filter(c => c.operation !== 'release')
    if (!validProcurementSharedMonitoringState(receipt) || receipt.revision !== review.revision + 1 || JSON.stringify(receipt.products) !== JSON.stringify([...selected].sort((a, b) => a.product_id - b.product_id)) ||
      receipt.plans.length !== expected.length || expected.some(change => !receipt.plans.some(plan => plan.account_id === change.account_id && (change.operation === 'create' || plan.plan_id === change.plan_id) && plan.enabled === ['create', 'update', 'retain'].includes(change.operation)))) throw new Error('invalid saved receipt')
    const next = await readProcurementSharedMonitoring()
    if (!validProcurementSharedMonitoringState(next) || next.revision !== receipt.revision || JSON.stringify(next.products) !== JSON.stringify(receipt.products) || JSON.stringify(next.plans) !== JSON.stringify(receipt.plans) || next.plans.some(p => !['managed', 'paused'].includes(p.state))) throw new Error('saved participation not verified')
    if (alive) { savedState.value = next; loaded.value = true; result.value = null; success.value = true }
  } catch (e) {
    if (alive) { loaded.value = false; result.value = null; error.value = written ? 'savedReadFailed' : (e as { status?: number })?.status === 409 ? 'conflict' : (e as { status?: number })?.status === 403 ? 'forbidden' : 'saveUnknown' }
  } finally { if (alive) saving.value = false }
}
onBeforeUnmount(() => { alive = false; generation++; readGeneration++; controller?.abort(); readController?.abort(); clearInterval(timer) })
</script>
