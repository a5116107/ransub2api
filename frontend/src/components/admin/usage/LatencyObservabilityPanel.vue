<template>
  <section :class="flat ? 'space-y-3 p-4' : 'card space-y-3 p-4'" data-testid="latency-observability">
    <div v-if="collapsible" class="space-y-1">
      <h3 class="font-semibold">
        <button
          type="button"
          class="flex w-full items-center justify-between gap-3 text-left"
          data-testid="latency-panel-toggle"
          :aria-expanded="expanded"
          :aria-controls="contentId"
          @click="toggleExpanded"
        >
          <span>{{ t('latencyObservability.title') }}</span>
          <span class="inline-flex items-center gap-1 text-sm font-normal text-gray-500 dark:text-gray-400">
            {{ t(expanded ? 'latencyObservability.collapse' : 'latencyObservability.expand') }}
            <Icon name="chevronDown" size="sm" :class="expanded && 'rotate-180'" />
          </span>
        </button>
      </h3>
      <p v-if="!expanded" class="text-xs text-gray-500 dark:text-gray-400">
        {{ t('latencyObservability.collapsedHint') }}
      </p>
    </div>
    <h3 v-else-if="!hideTitle" class="font-semibold">{{ t('latencyObservability.title') }}</h3>

    <div v-if="contentVisible" :id="contentId" class="space-y-3" data-testid="latency-panel-content">
      <details class="rounded-lg border border-gray-200 px-3 py-2 text-xs text-gray-500 dark:border-dark-700 dark:text-gray-400">
        <summary class="cursor-pointer font-medium">{{ t('latencyObservability.definitions') }}</summary>
        <div class="mt-2 space-y-2">
          <p>{{ t('latencyObservability.scope') }}</p>
          <p>{{ t('latencyObservability.typedScope') }}</p>
<p>{{ t('latencyObservability.cacheScope') }}</p>
<p>{{ t('latencyObservability.coldScope') }}</p>
          <p v-if="ops" data-testid="socket-evidence">{{ t('latencyObservability.socketScope') }}</p>
          <p v-if="ops">{{ t('latencyObservability.opsPopulation') }}</p>
        </div>
      </details>
      <p v-if="loading" role="status">{{ t('common.loading') }}</p>
      <p v-else-if="error" role="alert">{{ t('latencyObservability.error') }}</p>
      <p v-else-if="!report?.groups.length">{{ t('latencyObservability.empty') }}</p>
      <template v-else>
        <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
          <div v-for="control in filterControls" :key="control.key" class="flex flex-col justify-end gap-1">
            <label :for="`${contentId}-${control.key}`" class="text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ control.label }}
            </label>
            <Select
              v-model="groupFilters[control.key]"
              :id="`${contentId}-${control.key}`"
              :aria-label="control.label"
              :options="control.options"
              :data-testid="`latency-filter-${control.key}`"
            />
          </div>
        </div>
        <div class="flex flex-wrap items-center justify-between gap-2 text-xs text-gray-500 dark:text-gray-400">
          <p role="status">{{ t('latencyObservability.filteredGroups', { count: filteredGroups.length, total: report.groups.length }) }}</p>
          <button
            v-if="hasGroupFilters"
            type="button"
            class="btn btn-secondary btn-sm"
            data-testid="latency-clear-filters"
            @click="clearGroupFilters"
          >
            {{ t('latencyObservability.clearFilters') }}
          </button>
        </div>
        <p v-if="!filteredGroups.length" data-testid="latency-no-matches">{{ t('latencyObservability.noMatches') }}</p>
        <div v-else class="max-h-[32rem] overflow-auto rounded-lg border border-gray-200 dark:border-dark-700" role="region" tabindex="0" :aria-label="t('latencyObservability.title')">
          <table class="w-full text-left text-xs tabular-nums">
            <thead class="sticky top-0 bg-gray-50 dark:bg-dark-800">
              <tr>
                <th v-for="key in columns" :key="key" scope="col" class="p-2 font-medium">{{ t(`latencyObservability.${key}`) }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="g in pagedGroups"
                :key="cohortKey(g)"
                class="border-t border-gray-100 dark:border-dark-700"
                data-testid="latency-group"
              >
                <td class="whitespace-nowrap p-2">#{{ g.account_id ?? '—' }} · {{ g.model ?? '—' }}</td>
          <td class="p-2" data-testid="cache-cohort">
            <div>{{ g.upstream_model || '—' }} · {{ g.service_tier || '—' }}</div>
            <div>{{ g.upstream_endpoint || '—' }} · {{ g.stream == null ? '—' : t(`latencyObservability.${g.stream ? 'stream' : 'nonStream'}`) }}</div>
            <div data-testid="transport-endpoint">{{ t('latencyObservability.transportEndpoint', { endpoint: transportEndpoint(g.transport_endpoint) }) }}</div>
            <div :title="g.route_fingerprint">{{ t('latencyObservability.routeIdentity', { route: g.route_fingerprint?.slice(0, 12) || '—', proxy: g.proxy_id != null && g.proxy_id >= 0 ? g.proxy_id : '—' }) }}</div>
          </td>
          <td class="p-2">{{ g.body_bucket }}</td>
          <td class="p-2" data-testid="cache-evidence">{{ g.cache_bucket }}<div>{{ cacheSource(g.cache_source) }}</div></td>
          <td class="p-2">{{ g.reasoning_effort }}</td>
          <td class="p-2" data-testid="cache-cold-start">
            <div>{{ coldStatus(g.cold_start?.status) }}</div>
            <template v-if="g.cold_start">
              <div>{{ t('latencyObservability.coldSamples', { cold: g.cold_start.cold_samples, warm: g.cold_start.warm_samples }) }}</div>
              <div>{{ t('latencyObservability.coldRequirements', { min: g.cold_start.min_samples, seconds: g.cold_start.max_age_seconds }) }}</div>
              <div v-if="hasColdComparison(g.cold_start)" data-testid="cache-cold-comparison">
                {{ t('latencyObservability.coldComparison', { cold: formatMS(g.cold_start.cold_p90_ms), warm: formatMS(g.cold_start.warm_p90_ms), delta: formatMS(g.cold_start.delta_ms) }) }}
              </div>
            </template>
          </td>
                <td class="whitespace-nowrap p-2">{{ measured(g.reasoning_first_avg_ms, g.reasoning_samples) }}</td>
                <td class="whitespace-nowrap p-2">{{ measured(g.visible_text_first_avg_ms, g.text_samples) }}</td>
                <td class="whitespace-nowrap p-2">{{ measured(g.response_byte_first_avg_ms, g.byte_samples) }}</td>
                <td class="whitespace-nowrap p-2">{{ [g.visible_text_p50_ms, g.visible_text_p90_ms, g.visible_text_p99_ms].map(v => v == null ? '—' : `${v.toFixed(1)} ms`).join(' / ') }}</td>
                <td class="whitespace-nowrap p-2">{{ g.requests }} / {{ g.traces }} / {{ g.attempts }}</td>
                <td class="whitespace-nowrap p-2">{{ measured(g.body_upload_avg_ms, g.upload_samples) }}</td>
                <td class="whitespace-nowrap p-2">{{ measured(g.post_upload_first_content_wait_avg_ms, g.wait_samples) }}</td>
                <td class="whitespace-nowrap p-2">{{ measured(g.post_upload_first_visible_wait_avg_ms, g.visible_samples) }}</td>
                <td class="whitespace-nowrap p-2">{{ g.failover_samples ? `${g.large_body_switches} / ${g.large_body_same_account_retries} (n=${g.failover_samples})` : t('latencyObservability.notCollected') }}</td>
                <td class="whitespace-nowrap p-2">{{ measured(g.extra_upload_component_ms, g.cost_samples) }} / {{ measured(g.extra_wait_component_ms, g.cost_samples) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination
          v-if="filteredGroups.length > PAGE_SIZE"
          v-model:page="page"
          :total="filteredGroups.length"
          :page-size="PAGE_SIZE"
          :show-page-size-selector="false"
        />
      </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getLatencyReport, type CacheColdStartEvidence, type LatencyGroup, type LatencyReport } from '@/api/admin/latencyObservability'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'

const props = withDefaults(defineProps<{
  ops?: boolean
  startDate?: string
  endDate?: string
  model?: string
  platform?: string
  userId?: number
  apiKeyId?: number
  accountId?: number
  groupId?: number
  refreshToken?: number
  flat?: boolean
  hideTitle?: boolean
  collapsible?: boolean
}>(), {
  flat: false,
  hideTitle: false,
  collapsible: false,
})

const { t } = useI18n()
const STORAGE_KEY = 'latency-observability-expanded'
const PAGE_SIZE = 10
const contentId = useId()

function readExpansionState(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY) === 'true'
  } catch {
    return false
  }
}

const expanded = ref(readExpansionState())
const contentVisible = computed(() => !props.collapsible || expanded.value)

function toggleExpanded() {
  expanded.value = !expanded.value
  try {
    localStorage.setItem(STORAGE_KEY, String(expanded.value))
  } catch {
    // The toggle still works when browser storage is unavailable.
  }
}

const report = ref<LatencyReport | null>(null)
const loading = ref(false)
const error = ref(false)
const page = ref(1)
const columns = ['accountModel', 'routeScope', 'body', 'cache', 'effort', 'coldStart', 'reasoning_first_ms', 'visible_text_first_ms', 'response_byte_first_ms', 'textQuantiles', 'samples', 'upload', 'semantic', 'visible', 'switches', 'extra']
const measured = (value: number | null | undefined, count: number | undefined) => `${value == null ? t('latencyObservability.notCollected') : `${value.toFixed(1)} ms`} (n=${count ?? 0})`

const cohortKey = (g: LatencyGroup) => JSON.stringify([g.account_id, g.model, g.upstream_model, g.service_tier, g.upstream_endpoint, g.transport_endpoint, g.route_fingerprint, g.proxy_id, g.stream, g.body_bucket, g.cache_bucket, g.reasoning_effort, g.cache_source])
const transportEndpoint = (endpoint?: string) => {
  switch (endpoint) {
    case 'responses': case 'responses_compact': case 'chat_completions': case 'messages':
    case 'gemini_generate_content': case 'gemini_stream_generate_content': return endpoint
    default: return t('latencyObservability.notCollected')
  }
}
const cacheSource = (source?: string) => {
  const keys: Record<string, string> = { canonical_responses_terminal_usage: 'cacheCanonical', canonical_messages_cumulative_usage: 'cacheMessages', canonical_gemini_usage_metadata: 'cacheGemini', canonical_chat_completions_usage: 'cacheChat', billed_usage_positive_read: 'cacheLegacy', unverified_terminal_usage: 'cacheUnverified' }
  return t(`latencyObservability.${keys[source ?? ''] ?? 'notCollected'}`)
}
const coldStatus = (status?: string) => {
  const keys: Record<string, string> = { historical_association: 'coldHistorical', unknown_scope: 'coldUnknownScope', insufficient_samples: 'coldInsufficient', ambiguous_cohort: 'coldAmbiguous', invalid_window: 'coldInvalidWindow', stale_samples: 'coldStale', invalid_measurement: 'coldInvalidMeasurement' }
  return t(`latencyObservability.${keys[status ?? ''] ?? 'notCollected'}`)
}
const formatMS = (value: number | null) => value != null && Number.isFinite(value) ? `${value.toFixed(1)} ms` : t('latencyObservability.notCollected')
const hasColdComparison = (e: CacheColdStartEvidence) => e.status === 'historical_association' && [e.cold_p90_ms, e.warm_p90_ms, e.delta_ms].every(v => v != null && Number.isFinite(v))

const dimensions = [
  { key: 'account_id', label: 'account' },
  { key: 'model', label: 'model' },
  { key: 'body_bucket', label: 'body' },
  { key: 'cache_bucket', label: 'cache' },
  { key: 'reasoning_effort', label: 'effort' },
] as const
type Dimension = (typeof dimensions)[number]['key']
const groupFilters = reactive<Record<Dimension, string | null>>({
  account_id: null,
  model: null,
  body_bucket: null,
  cache_bucket: null,
  reasoning_effort: null,
})
const dimensionValue = (group: LatencyGroup, key: Dimension) => String(group[key] ?? '')
const filterControls = computed(() => dimensions.map(({ key, label }) => {
  const values = new Set((report.value?.groups ?? []).map(group => dimensionValue(group, key)))
  const selected = groupFilters[key]
  if (selected !== null) values.add(selected)
  return {
    key,
    label: t(`latencyObservability.${label}`),
    options: [
      { value: null, label: t('latencyObservability.all') },
      ...[...values].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }))
        .map(value => ({ value, label: key === 'account_id' && value ? `#${value}` : value || '—' })),
    ],
  }
}))
const hasGroupFilters = computed(() => dimensions.some(({ key }) => groupFilters[key] !== null))
const filteredGroups = computed(() => (report.value?.groups ?? []).filter(group => dimensions.every(({ key }) =>
  groupFilters[key] === null || dimensionValue(group, key) === groupFilters[key],
)))
const pagedGroups = computed(() => filteredGroups.value.slice((page.value - 1) * PAGE_SIZE, page.value * PAGE_SIZE))

function clearGroupFilters() {
  for (const { key } of dimensions) groupFilters[key] = null
}

watch(filteredGroups, () => { page.value = 1 })

let controller: AbortController | undefined
watch(() => [contentVisible.value, props.ops, props.startDate, props.endDate, props.model, props.platform, props.userId, props.apiKeyId, props.accountId, props.groupId, props.refreshToken], async () => {
  controller?.abort()
  controller = undefined
  loading.value = false
  error.value = false
  report.value = null
  if (!contentVisible.value) return

  const current = new AbortController()
  controller = current
  loading.value = true
  try {
    const result = await getLatencyReport(!!props.ops, { start_date: props.startDate, end_date: props.endDate, model: props.model || undefined, platform: props.platform || undefined, user_id: props.userId || undefined, api_key_id: props.apiKeyId || undefined, account_id: props.accountId || undefined, group_id: props.groupId || undefined }, current.signal)
    if (controller === current && !current.signal.aborted) report.value = result
  } catch {
    if (controller === current && !current.signal.aborted) error.value = true
  } finally {
    if (controller === current) loading.value = false
  }
}, { immediate: true })

onUnmounted(() => { controller?.abort(); controller = undefined })
</script>
