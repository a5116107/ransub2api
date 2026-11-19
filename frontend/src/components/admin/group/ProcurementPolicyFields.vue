<template>
  <fieldset class="mt-3 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
    <legend class="px-1 text-sm font-medium text-gray-700 dark:text-gray-200">{{ t('admin.groups.profitControl.procurement.title') }}</legend>
    <p :id="`${idPrefix}-hint`" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.profitControl.procurement.hint') }}</p>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <div>
        <label :for="`${idPrefix}-min`" class="input-label">{{ t('admin.groups.profitControl.procurement.min') }}</label>
        <input :id="`${idPrefix}-min`" :value="modelValue?.min_rate ?? ''" type="text" inputmode="decimal" maxlength="17"
          class="input" placeholder="0" :disabled="unsupported" :aria-invalid="Boolean(error)" :aria-describedby="`${idPrefix}-hint ${idPrefix}-status`"
          @input="update('min_rate', ($event.target as HTMLInputElement).value)" />
      </div>
      <div>
        <label :for="`${idPrefix}-max`" class="input-label">{{ t('admin.groups.profitControl.procurement.max') }}</label>
        <input :id="`${idPrefix}-max`" :value="modelValue?.max_rate ?? ''" type="text" inputmode="decimal" maxlength="17"
          class="input" :placeholder="t('admin.groups.profitControl.procurement.unbounded')" :disabled="unsupported"
          :aria-invalid="Boolean(error)" :aria-describedby="`${idPrefix}-hint ${idPrefix}-status`"
          @input="update('max_rate', ($event.target as HTMLInputElement).value)" />
      </div>
    </div>
    <div v-if="!unsupported" class="space-y-2">
      <label :for="`${idPrefix}-source-groups-mode`" class="input-label">{{ t(`${p}.sourceGroupsTitle`) }}</label>
      <select :id="`${idPrefix}-source-groups-mode`" class="input" :value="restrictedSourceGroups ? 'selected' : 'original'" @change="setSourceGroupMode(($event.target as HTMLSelectElement).value)">
        <option value="original">{{ t(`${p}.sourceGroupsOriginal`) }}</option>
        <option value="selected">{{ t(`${p}.sourceGroupsSelected`) }}</option>
      </select>
      <p class="break-words text-xs text-gray-500 dark:text-gray-400">{{ t(`${p}.sourceGroupsHint`) }}</p>
      <template v-if="restrictedSourceGroups">
        <p v-if="sourceGroupsLoading" role="status" class="text-xs">{{ t(`${p}.sourceGroupsLoading`) }}</p>
        <p v-if="sourceGroupsError" role="alert" class="text-xs text-amber-800 dark:text-amber-300">{{ t(`${p}.sourceGroupsReadFailed`) }}</p>
        <button type="button" class="btn btn-secondary text-xs" :disabled="sourceGroupsLoading" @click="loadSourceGroups">{{ t(`${p}.sourceGroupsReload`) }}</button>
        <div class="max-h-60 space-y-2 overflow-y-auto">
          <label v-for="group in sourceGroups" :key="group.id" class="flex items-start gap-2 break-words text-xs">
            <input type="checkbox" class="mt-0.5" :data-source-group-id="group.id" :checked="selectedSourceGroups.includes(group.id)" @change="toggleSourceGroup(group.id, ($event.target as HTMLInputElement).checked)" />
            <span class="min-w-0 break-words">{{ group.name }} · #{{ group.id }}</span>
          </label>
        </div>
        <div v-if="missingSourceGroups.length" class="space-y-2">
          <p role="alert" class="break-words text-xs text-amber-800 dark:text-amber-300">{{ t(`${p}.sourceGroupsMissing`, { ids: missingSourceGroups.join(', ') }) }}</p>
          <div class="flex flex-wrap gap-2">
            <button v-for="id in missingSourceGroups" :key="id" type="button" class="btn btn-secondary break-words text-xs" :data-remove-source-group-id="id" @click="toggleSourceGroup(id, false)">{{ t(`${p}.sourceGroupsRemove`, { id }) }}</button>
          </div>
        </div>
        <p v-if="error !== 'sourceGroupFormat'" role="status" class="break-words text-xs" :class="selectedSourceGroups.length === 0 ? 'text-amber-800 dark:text-amber-300' : 'text-gray-600 dark:text-gray-300'">{{ t(`${p}.${selectedSourceGroups.length ? 'sourceGroupsCount' : 'sourceGroupsEmpty'}`, { count: selectedSourceGroups.length }) }}</p>
      </template>
    </div>
    <div v-if="!unsupported" class="space-y-2">
      <label :for="`${idPrefix}-availability`" class="input-label">{{ t(`${p}.availabilityTitle`) }}</label>
      <select :id="`${idPrefix}-availability`" class="input" :value="modelValue?.availability ?? 'not_required'" :aria-describedby="`${idPrefix}-availability-hint`" @change="setAvailability(($event.target as HTMLSelectElement).value)">
        <option value="not_required">{{ t(`${p}.availabilityOff`) }}</option>
        <option value="current_state">{{ t(`${p}.availabilityRequired`) }}</option>
      </select>
      <p :id="`${idPrefix}-availability-hint`" class="break-words text-xs text-gray-500 dark:text-gray-400">{{ t(`${p}.availabilityHint`) }}</p>
    </div>
    <div v-if="!unsupported" class="space-y-2">
      <label :for="`${idPrefix}-quality`" class="input-label">{{ t(`${p}.qualityTitle`) }}</label>
      <select :id="`${idPrefix}-quality`" class="input" :value="modelValue?.quality ?? 'not_required'" :aria-describedby="`${idPrefix}-quality-hint`" @change="setQuality(($event.target as HTMLSelectElement).value)">
        <option value="not_required">{{ t(`${p}.qualityOff`) }}</option>
        <option value="current_answer">{{ t(`${p}.qualityRequired`) }}</option>
      </select>
      <p :id="`${idPrefix}-quality-hint`" class="break-words text-xs text-gray-500 dark:text-gray-400">{{ t(`${p}.qualityHint`) }}</p>
    </div>
    <div v-if="!unsupported" class="space-y-2">
      <label :for="`${idPrefix}-buckets-mode`" class="input-label">{{ t(`${p}.bucketsTitle`) }}</label>
      <select :id="`${idPrefix}-buckets-mode`" class="input" :value="restrictedBuckets ? 'selected' : 'original'" :aria-describedby="`${idPrefix}-buckets-hint`" @change="setBucketMode(($event.target as HTMLSelectElement).value)">
        <option value="original">{{ t(`${p}.bucketsUnrestricted`) }}</option>
        <option value="selected">{{ t(`${p}.bucketsSelected`) }}</option>
      </select>
      <p :id="`${idPrefix}-buckets-hint`" class="text-xs text-gray-500 dark:text-gray-400">{{ t(`${p}.bucketsHint`) }}</p>
      <template v-if="restrictedBuckets">
        <p v-if="catalogLoading" role="status" class="text-xs">{{ t(`${p}.bucketsLoading`) }}</p>
        <p v-if="catalogError" role="alert" class="text-xs text-amber-800 dark:text-amber-300">{{ t(`${p}.bucketsReadFailed`) }}</p>
        <button type="button" class="btn btn-secondary text-xs" :disabled="catalogLoading" @click="loadCatalog">{{ t(`${p}.bucketsReload`) }}</button>
        <p v-if="catalog" class="text-xs text-gray-500">{{ t(`${p}.bucketsRevision`, { revision: catalog.revision }) }}</p>
        <div class="max-h-60 space-y-2 overflow-y-auto">
          <label v-for="bucket in catalog?.buckets ?? []" :key="bucket.code" class="flex items-start gap-2 break-words text-xs">
            <input type="checkbox" class="mt-0.5" :checked="selectedBuckets.includes(bucket.code)" :data-bucket-code="bucket.code" @change="toggleBucket(bucket.code, ($event.target as HTMLInputElement).checked)" />
            <span class="min-w-0 break-words">{{ bucket.name }} · {{ bucket.code }} [{{ bucket.min_rate ?? '0' }}, {{ bucket.max_rate ?? '∞' }})</span>
          </label>
        </div>
        <p v-if="missingBuckets.length" role="alert" class="break-words text-xs text-amber-800 dark:text-amber-300">{{ t(`${p}.bucketsMissing`, { codes: missingBuckets.join(', ') }) }}</p>
        <p v-if="error !== 'bucketFormat'" role="status" class="break-words text-xs" :class="selectedBuckets.length === 0 ? 'text-amber-800 dark:text-amber-300' : 'text-gray-600 dark:text-gray-300'">{{ t(`${p}.${selectedBuckets.length === 0 ? 'bucketsEmpty' : 'bucketsCount'}`, { count: selectedBuckets.length }) }}</p>
      </template>
    </div>
    <div v-if="!unsupported" class="space-y-2">
      <label :for="`${idPrefix}-sources-mode`" class="input-label">{{ t('admin.groups.profitControl.procurement.sourcesTitle') }}</label>
      <select :id="`${idPrefix}-sources-mode`" class="input" :value="restrictedSources ? 'approved' : 'original'" :disabled="unsupported"
        :aria-describedby="`${idPrefix}-sources-hint`" @change="setSourceMode(($event.target as HTMLSelectElement).value)">
        <option value="original">{{ t('admin.groups.profitControl.procurement.originalPool') }}</option>
        <option value="approved">{{ t('admin.groups.profitControl.procurement.approvedOnly') }}</option>
      </select>
      <p :id="`${idPrefix}-sources-hint`" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.profitControl.procurement.sourcesHint') }}</p>
      <template v-if="restrictedSources">
        <label :for="`${idPrefix}-sources`" class="input-label">{{ t('admin.groups.profitControl.procurement.sourceIDs') }}</label>
        <textarea :id="`${idPrefix}-sources`" v-model="sourceDraft" class="input min-h-20" rows="2" :disabled="unsupported"
          :aria-invalid="error === 'sourceFormat'" :aria-describedby="`${idPrefix}-status ${idPrefix}-sources-hint`"
          @input="updateSources(($event.target as HTMLTextAreaElement).value)" />
      </template>
      <p v-if="error !== 'sourceFormat'" role="status" class="break-words rounded-lg px-3 py-2 text-xs"
        :class="restrictedSources && sourceCount === 0 ? 'bg-amber-50 text-amber-900 dark:bg-amber-900/20 dark:text-amber-200' : 'bg-gray-50 text-gray-700 dark:bg-dark-700 dark:text-gray-200'">
        {{ t(`admin.groups.profitControl.procurement.${restrictedSources ? sourceCount === 0 ? 'sourcesEmpty' : 'sourcesCount' : 'sourcesOriginal'}`, { count: sourceCount }) }}
      </p>
    </div>
    <p :id="`${idPrefix}-status`" :role="error ? 'alert' : 'status'" class="text-xs"
      :class="error ? 'text-amber-700 dark:text-amber-300' : 'text-gray-600 dark:text-gray-300'">
      <template v-if="error">{{ t(`admin.groups.profitControl.procurement.${error}`) }}</template>
      <template v-else>{{ t('admin.groups.profitControl.procurement.range') }}: [{{ modelValue?.min_rate ?? '0' }}, {{ modelValue?.max_rate ?? '∞' }})</template>
    </p>
    <div v-if="conflict" role="alert" class="space-y-2 rounded-lg bg-amber-50 p-3 text-xs text-amber-900 dark:bg-amber-900/20 dark:text-amber-200">
      <p>{{ t('admin.groups.profitControl.procurement.conflict') }}</p>
      <p v-if="latestPolicy !== undefined && !latestError">
        {{ t('admin.groups.profitControl.procurement.latestRange') }}: [{{ latestPolicy?.min_rate ?? '0' }}, {{ latestPolicy?.max_rate ?? '∞' }})
      </p>
      <p v-if="latestPolicy !== undefined && !latestError" class="max-h-32 overflow-y-auto break-words">
        {{ t('admin.groups.profitControl.procurement.latestSources') }}:
        {{ latestPolicy?.sources?.account_ids === null || !latestPolicy?.sources ? t('admin.groups.profitControl.procurement.originalPool') : latestPolicy.sources.account_ids.length ? latestPolicy.sources.account_ids.join(', ') : t('admin.groups.profitControl.procurement.sourcesEmpty') }}
      </p>
      <p v-else-if="latestError">{{ t(`admin.groups.profitControl.procurement.${latestError}`) }}</p>
      <p v-if="latestPolicy !== undefined && !latestError" class="break-words">{{ t(`${p}.latestBuckets`) }}: {{ latestPolicy?.buckets?.codes === null || !latestPolicy?.buckets ? t(`${p}.bucketsUnrestricted`) : latestPolicy.buckets.codes.length ? latestPolicy.buckets.codes.join(', ') : t(`${p}.bucketsEmpty`) }}</p>
      <p v-if="latestPolicy !== undefined && !latestError" class="break-words">{{ t(`${p}.latestQuality`) }}: {{ t(`${p}.${latestPolicy?.quality === 'current_answer' ? 'qualityRequired' : 'qualityOff'}`) }}</p>
      <p v-if="latestPolicy !== undefined && !latestError" class="break-words">{{ t(`${p}.latestAvailability`) }}: {{ t(`${p}.${latestPolicy?.availability === 'current_state' ? 'availabilityRequired' : 'availabilityOff'}`) }}</p>
      <p v-if="latestPolicy !== undefined && !latestError" class="break-words">{{ t(`${p}.sourceGroupsLatest`) }}: {{ Array.isArray(latestPolicy?.source_group_ids) ? latestPolicy.source_group_ids.length ? latestPolicy.source_group_ids.join(', ') : t(`${p}.sourceGroupsEmpty`) : t(`${p}.sourceGroupsOriginal`) }}</p>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-secondary text-xs" :disabled="refreshing" @click="emit('refresh')">
          {{ t('admin.groups.profitControl.procurement.reloadLatest') }}
        </button>
        <button v-if="latestPolicy !== undefined" type="button" class="btn btn-secondary text-xs" :disabled="refreshing || Boolean(latestError)" @click="emit('resolve')">
          {{ t('admin.groups.profitControl.procurement.useLatestRevision') }}
        </button>
      </div>
    </div>
  </fieldset>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { GroupProcurementPolicy, GroupPlatform } from '@/types'
import { adminAPI } from '@/api/admin'
import { cloneProcurementPolicy, parseProcurementSourceIDs, validateProcurementPolicy } from '@/utils/procurementPolicy'
import { readProcurementCatalog, type ProcurementCatalog } from '@/api/admin/procurementCatalog'
import { validProcurementSnapshot } from '@/utils/procurementCatalog'

const props = defineProps<{
  modelValue: GroupProcurementPolicy | null
  idPrefix: string
  platform?: GroupPlatform
  productGroupId?: number
  conflict?: boolean
  latestPolicy?: GroupProcurementPolicy | null
  refreshing?: boolean
}>()
const emit = defineEmits<{
  'update:modelValue': [value: GroupProcurementPolicy | null]
  refresh: []
  resolve: []
}>()
const { t } = useI18n()
const p = 'admin.groups.profitControl.procurement'
const error = computed(() => validateProcurementPolicy(props.modelValue))
const unsupported = computed(() => error.value === 'unsupported')
const latestError = computed(() => props.latestPolicy === undefined ? null : validateProcurementPolicy(props.latestPolicy))
const restrictedSources = computed(() => (props.modelValue?.version ?? 0) >= 2 && Array.isArray(props.modelValue?.sources?.account_ids))
const restrictedBuckets = computed(() => (props.modelValue?.version ?? 0) >= 3 && Array.isArray(props.modelValue?.buckets?.codes))
const selectedBuckets = computed(() => props.modelValue?.buckets?.codes ?? [])
const catalog = ref<ProcurementCatalog | null>(null), catalogLoading = ref(false), catalogError = ref(false)
const missingBuckets = computed(() => catalog.value ? selectedBuckets.value.filter(code => !catalog.value!.buckets.some(bucket => bucket.code === code)) : [])
const controller = new AbortController()
let alive = true
async function loadCatalog() {
  if (catalogLoading.value) return
  catalogLoading.value = true; catalogError.value = false; catalog.value = null
  try {
    const result = await readProcurementCatalog(controller.signal)
    if (!validProcurementSnapshot(result)) throw new Error('unknown catalog contract')
    if (alive) catalog.value = result.catalog
  } catch { if (alive) catalogError.value = true }
  finally { if (alive) catalogLoading.value = false }
}
watch(restrictedBuckets, value => { if (value && !catalog.value) void loadCatalog() }, { immediate: true })
onBeforeUnmount(() => { alive = false; controller.abort() })
const restrictedSourceGroups = computed(() => Array.isArray(props.modelValue?.source_group_ids))
const selectedSourceGroups = computed(() => props.modelValue?.source_group_ids ?? [])
const sourceGroups = ref<{ id: number; name: string }[]>([])
const sourceGroupsLoading = ref(false), sourceGroupsError = ref(false), sourceGroupsLoaded = ref(false)
let sourceGeneration = 0
const missingSourceGroups = computed(() => sourceGroupsLoaded.value ? selectedSourceGroups.value.filter(id => !sourceGroups.value.some(group => group.id === id)) : [])
async function loadSourceGroups() {
  const generation = ++sourceGeneration
  sourceGroupsLoading.value = true; sourceGroupsError.value = false; sourceGroupsLoaded.value = false; sourceGroups.value = []
  try {
    const groups = await adminAPI.groups.getAll(props.platform, controller.signal)
    if (!Array.isArray(groups) || groups.some(group => !Number.isSafeInteger(group.id) || group.id <= 0 || typeof group.name !== 'string')) throw new Error('invalid source groups')
    if (alive && generation === sourceGeneration) {
      sourceGroups.value = groups.filter(group => group.id !== props.productGroupId && group.status === 'active' && (!props.platform || group.platform === props.platform)).map(group => ({ id: group.id, name: group.name }))
      sourceGroupsLoaded.value = true
    }
  } catch { if (alive && generation === sourceGeneration) sourceGroupsError.value = true }
  finally { if (alive && generation === sourceGeneration) sourceGroupsLoading.value = false }
}
watch([restrictedSourceGroups, () => props.platform, () => props.productGroupId], ([restricted]) => { if (restricted) void loadSourceGroups() }, { immediate: true })
function setSourceGroupMode(value: string) {
  if (unsupported.value) return
  const next = draft()
  next.version = 6
  next.sources ??= { account_ids: null }; next.buckets ??= { codes: null }
  next.quality ??= 'not_required'; next.availability ??= 'not_required'
  next.source_group_ids = value === 'selected' ? [] : null
  emit('update:modelValue', next)
}
function toggleSourceGroup(id: number, checked: boolean) {
  if (unsupported.value) return
  const next = draft()
  next.source_group_ids = checked ? [...selectedSourceGroups.value, id].sort((a, b) => a - b) : selectedSourceGroups.value.filter(value => value !== id)
  emit('update:modelValue', next)
}

const sourceCount = computed(() => props.modelValue?.sources?.account_ids?.length ?? 0)
const sourceDraft = ref('')
watch(() => props.modelValue?.sources?.account_ids, ids => {
  if (!Array.isArray(ids)) { sourceDraft.value = ''; return }
  const parsed = parseProcurementSourceIDs(sourceDraft.value)
  if (ids.length !== parsed.length || ids.some((id, i) => !Object.is(id, parsed[i]))) sourceDraft.value = ids.join(', ')
}, { immediate: true, deep: true })

function draft(): GroupProcurementPolicy {
  return props.modelValue ? cloneProcurementPolicy(props.modelValue) : { version: 1, revision: 0, min_rate: null, max_rate: null }
}

function setQuality(value: string) {
  if (unsupported.value) return
  const next = draft()
  next.version = Math.max(4, next.version)
  next.sources ??= { account_ids: null }
  next.buckets ??= { codes: null }
  next.quality = value
  emit('update:modelValue', next)
}

function setAvailability(value: string) {
  if (unsupported.value) return
  const next = draft()
  next.version = Math.max(5, next.version)
  next.sources ??= { account_ids: null }
  next.buckets ??= { codes: null }
  next.quality ??= 'not_required'
  next.availability = value
  emit('update:modelValue', next)
}

function setSourceMode(value: string) {
  if (unsupported.value) return
  const next = draft()
  next.version = Math.max(2, next.version)
  next.sources = { account_ids: value === 'approved' ? [] : null }
  emit('update:modelValue', next)
}

function updateSources(value: string) {
  if (unsupported.value) return
  const next = draft()
  next.version = Math.max(2, next.version)
  next.sources = { account_ids: parseProcurementSourceIDs(value) }
  emit('update:modelValue', next)
}

function setBucketMode(value: string) {
  if (unsupported.value) return
  const next = draft()
  next.version = Math.max(3, next.version)
  next.sources ??= { account_ids: null }
  next.buckets = { codes: value === 'selected' ? [] : null }
  emit('update:modelValue', next)
}
function toggleBucket(code: string, checked: boolean) {
  if (unsupported.value) return
  const next = draft()
  next.version = Math.max(3, next.version)
  next.sources ??= { account_ids: null }
  next.buckets = { codes: checked ? [...selectedBuckets.value, code].sort() : selectedBuckets.value.filter(value => value !== code) }
  emit('update:modelValue', next)
}

function update(field: 'min_rate' | 'max_rate', value: string) {
  if (unsupported.value) return
  const next = draft()
  next[field] = value.trim() || null
  emit('update:modelValue', next.version === 1 && next.revision === 0 && next.min_rate === null && next.max_rate === null ? null : next)
}
</script>
