<template>
  <BaseDialog :show="!!record" :title="t('requestTiming.title')" width="extra-wide" :placement="viewportWidth < 768 ? 'center' : 'right'" close-on-click-outside @close="$emit('close')">
    <div v-if="record" class="space-y-5">
      <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
        <div class="flex flex-wrap justify-between gap-2"><strong>{{ record.model }}</strong><span>{{ formatDateTime(record.created_at) }}</span></div>
        <div class="mt-2 break-all font-mono text-xs text-gray-500">{{ record.request_id }}</div>
      </div>
      <div class="grid grid-cols-2 gap-3 md:grid-cols-3">
        <div v-for="item in summaries" :key="item.label" class="rounded-xl border p-4" :class="TIMING_SURFACE[item.health]">
          <div class="text-sm text-gray-600 dark:text-gray-400">{{ item.label }}</div>
          <div class="mt-2 flex flex-wrap items-baseline justify-between gap-2" :class="TIMING_TEXT[item.health]">
            <strong class="text-xl tabular-nums">{{ ms(item.value) }}</strong>
            <span v-if="item.health !== 'neutral'" class="text-xs font-semibold">{{ healthLabel(item.health) }}</span>
          </div>
        </div>
      </div>
      <p v-if="loading" role="status">{{ t('requestTiming.loading') }}</p>
      <div v-else-if="error" role="alert"><p>{{ t('requestTiming.loadError') }}</p><button class="btn btn-secondary mt-2" @click="load">{{ t('requestTiming.retry') }}</button></div>
      <p v-else-if="!trace" class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800">{{ t('requestTiming.empty') }}</p>
      <template v-if="trace">
        <label v-if="traces.length > 1" class="block text-sm">{{ t('requestTiming.traceSelection') }}
          <select v-model="selected" class="input mt-2 w-full"><option v-for="(item, i) in traces" :key="item.trace_id" :value="i">{{ formatDateTime(item.started_at) }} · {{ ms(item.total_ms) }}</option></select>
        </label>
        <div class="rounded-xl border border-sky-200 bg-sky-50 p-4 dark:border-sky-900 dark:bg-sky-950/30">
          <h4 class="font-semibold text-sky-900 dark:text-sky-200">{{ t('requestTiming.health.guideTitle') }}</h4>
          <p class="mt-1 text-sm text-sky-800 dark:text-sky-300">{{ diagnosis }}</p>
          <p v-if="largestStage" class="mt-2 text-sm text-sky-800 dark:text-sky-300">{{ t('requestTiming.health.largest', { stage: label(largestStage.name), time: ms(largestStage.end_ms - largestStage.start_ms) }) }}</p>
          <div class="mt-3 flex flex-wrap gap-x-4 gap-y-2 text-xs">
            <span v-for="health in healthLevels" :key="health" class="inline-flex items-center gap-1.5 font-medium" :class="TIMING_TEXT[health]"><span class="h-2 w-2 rounded-full" :class="TIMING_BAR[health]" />{{ healthLabel(health) }}</span>
          </div>
          <details class="mt-3 text-xs text-sky-800 dark:text-sky-300"><summary class="cursor-pointer">{{ t('requestTiming.health.thresholdTitle') }}</summary><p class="mt-2 leading-relaxed">{{ t('requestTiming.health.thresholds') }}</p></details>
        </div>
        <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('requestTiming.scope') }}</p>
        <p class="text-sm"><strong>{{ t('usage.latencyTps') }} {{ formatUsageOutputTps(record) ?? '—' }}</strong> · {{ t('requestTiming.tpsNote') }}</p>
        <section class="rounded-xl border border-gray-200 p-4 dark:border-dark-600" aria-labelledby="output-rate-title">
          <div class="flex flex-wrap items-baseline justify-between gap-2">
            <h4 id="output-rate-title" class="font-semibold">{{ t('requestTiming.outputRate.title') }}</h4>
            <strong data-testid="output-phase-rate" class="text-lg tabular-nums">{{ phaseRate }}</strong>
          </div>
          <p class="mt-1 text-sm text-gray-600 dark:text-gray-400" data-testid="output-phase-status">{{ phaseRateStatus }}</p>
          <dl class="mt-3 grid grid-cols-2 gap-3 text-sm md:grid-cols-4">
            <div><dt class="text-gray-500">{{ t('requestTiming.outputRate.tokens') }}</dt><dd class="mt-1 tabular-nums">{{ phaseRateTokens }}</dd></div>
            <div><dt class="text-gray-500">{{ t('requestTiming.outputRate.reasoning') }}</dt><dd class="mt-1 tabular-nums">{{ phaseReasoningTokens }}</dd></div>
            <div><dt class="text-gray-500">{{ t('requestTiming.outputRate.window') }}</dt><dd class="mt-1 tabular-nums">{{ ms(outputRate?.window_ms) }}</dd></div>
            <div><dt class="text-gray-500">{{ t('requestTiming.outputRate.scopeLabel') }}</dt><dd class="mt-1" data-testid="output-phase-scope">{{ phaseRateScopeKey ? t(phaseRateScopeKey) : t('requestTiming.missing') }}</dd></div>
          </dl>
          <p class="mt-3 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('requestTiming.outputRate.note') }}</p>
        </section>
        <section class="rounded-xl border border-gray-200 p-4 dark:border-dark-600" aria-labelledby="cache-migration-title">
          <h4 id="cache-migration-title" class="font-semibold">{{ t('requestTiming.cacheMigration.title') }}</h4>
          <p class="mt-1 text-sm text-gray-600 dark:text-gray-400" data-testid="cache-migration-status">{{ migrationStatus }}</p>
          <dl class="mt-3 grid grid-cols-2 gap-3 text-sm md:grid-cols-4">
            <div><dt class="text-gray-500">{{ t('requestTiming.cacheMigration.input') }}</dt><dd class="mt-1 tabular-nums">{{ migrationCounts?.input ?? t('requestTiming.missing') }}</dd></div>
            <div><dt class="text-gray-500">{{ t('requestTiming.cacheMigration.read') }}</dt><dd class="mt-1 tabular-nums">{{ migrationCounts?.read ?? t('requestTiming.missing') }}</dd></div>
            <div><dt class="text-gray-500">{{ t('requestTiming.cacheMigration.nonRead') }}</dt><dd class="mt-1 tabular-nums" data-testid="cache-migration-non-read">{{ migrationCounts?.nonRead ?? t('requestTiming.missing') }}</dd></div>
            <div><dt class="text-gray-500">{{ t('requestTiming.cacheMigration.bound') }}</dt><dd class="mt-1 tabular-nums" data-testid="cache-migration-bound">{{ migrationBound == null ? t('requestTiming.missing') : `0–${migrationBound}` }}</dd></div>
          </dl>
          <p v-if="migrationTransitions" class="mt-3 text-xs text-gray-500 dark:text-gray-400">{{ migrationTransitions }}</p>
          <p class="mt-3 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('requestTiming.cacheMigration.note') }}</p>
        </section>
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('requestTiming.health.attemptGuide') }}</p>
        <p v-if="trace.truncated" class="text-amber-600">{{ t('requestTiming.truncated') }}</p>
        <div class="grid gap-4 lg:grid-cols-3">
          <section v-for="group in groups" :key="group.title" class="rounded-xl border border-gray-200 p-4 dark:border-dark-600">
            <h4 class="mb-4 font-semibold">{{ group.title }}</h4>
            <dl class="space-y-3"><div v-for="row in group.rows" :key="row.name" class="flex justify-between gap-4 text-sm"><dt class="text-gray-500">{{ row.name }}</dt><dd class="shrink-0 text-right font-medium tabular-nums" :class="TIMING_TEXT[row.health]">{{ row.value }}<span v-if="row.health !== 'neutral'" class="mt-0.5 block text-[10px] font-normal">{{ healthLabel(row.health) }}</span></dd></div></dl>
          </section>
        </div>
        <section class="rounded-xl border border-gray-200 p-4 dark:border-dark-600">
          <h4 class="mb-3 font-semibold">{{ t('requestTiming.timeline') }}</h4>
          <p class="mb-3 text-xs text-gray-500">{{ t('requestTiming.overlap') }}</p>
          <div v-for="(span, i) in orderedSpans" :key="i" class="mb-3">
            <div class="flex justify-between gap-2 text-xs"><span>{{ label(span.name) }}</span><span :class="TIMING_TEXT[spanHealth(span)]">{{ ms(span.start_ms) }} → {{ ms(span.end_ms) }} · <strong>{{ ms(span.end_ms - span.start_ms) }}</strong></span></div>
            <div class="mt-1 h-2 overflow-hidden rounded bg-gray-100 dark:bg-dark-700"><div class="h-full rounded" :class="TIMING_BAR[spanHealth(span)]" :style="bar(span)" /></div>
          </div>
        </section>
        <section v-for="attempt in trace.attempts" :key="attempt.number" class="rounded-xl border border-gray-200 p-4 dark:border-dark-600">
          <h4 class="font-semibold">{{ attemptTitle(attempt) }} · {{ t('requestTiming.account') }} #{{ attempt.account_id }} · {{ attempt.proxy_id > 0 ? `${t('requestTiming.proxy')} #${attempt.proxy_id}` : t('requestTiming.direct') }}</h4>
          <div class="mt-3 grid gap-x-8 gap-y-3 text-sm md:grid-cols-2">
            <div v-for="row in attemptRows(attempt)" :key="row.name" class="flex justify-between gap-4"><span class="text-gray-500">{{ row.name }}</span><span class="text-right font-medium tabular-nums" :class="TIMING_TEXT[row.health]">{{ row.value }}<span v-if="row.health !== 'neutral'" class="mt-0.5 block text-[10px] font-normal">{{ healthLabel(row.health) }}</span></span></div>
          </div>
        </section>
        <div class="break-all text-xs text-gray-500">Trace: {{ trace.trace_id }} · {{ t('requestTiming.retention', { days: retention }) }}</div>
      </template>
    </div>
  </BaseDialog>
</template>
<script setup lang="ts">
import { inject, computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useWindowSize } from '@vueuse/core'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { AdminUsageLog } from '@/types'
import { formatDateTime } from '@/utils/format'
import { formatUsageOutputTps } from '@/utils/usageTps'
import { timingHealth, stageScale, TIMING_TEXT, TIMING_SURFACE, TIMING_BAR, type TimingHealth, type TimingScale } from '@/utils/requestTimingHealth'
import { getUsageTiming, type RequestTiming, type TimingAttempt, type TimingSpan } from '@/api/admin/usageTiming'
import { observerUsageAPI } from '@/api/observerUsage'
import { observerUsageContext } from './observerUsageContext'
const observerMode = inject(observerUsageContext, false)
const props = defineProps<{ record: AdminUsageLog | null }>()
defineEmits<{ close: [] }>()
const { t } = useI18n()
const { width: viewportWidth } = useWindowSize()
const traces = ref<RequestTiming[]>([]), selected = ref(0), loading = ref(false), error = ref(false), retention = ref(30)
let controller: AbortController | undefined
const trace = computed(() => traces.value[selected.value])
const outputRate = computed(() => trace.value?.output_rate)
const migration = computed(() => trace.value?.latency?.cache_migration)
const migrationScope = computed(() => trace.value?.version === 1 && trace.value.latency?.version === 1 && trace.value.ttft_mode !== 'ws_turn' && migration.value?.scope === 'final_physical_http_input_after_request_route_transitions' && migration.value.method === 'zero_to_final_non_read_input_not_causal')
const safeCount = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
const migrationCounts = computed(() => {
  const v = migration.value
  if (!migrationScope.value || !v || v.cache_status !== 'observed' || !['canonical_responses_terminal_usage', 'canonical_chat_completions_usage', 'canonical_messages_cumulative_usage', 'canonical_gemini_usage_metadata'].includes(v.source)) return undefined
  const { input_tokens: input, read_tokens: read, non_read_input_tokens: nonRead } = v
  if (!safeCount(input) || !safeCount(read) || !safeCount(nonRead) || read > input || nonRead !== input - read) return undefined
  return { input, read, nonRead }
})
const migrationBound = computed(() => {
  const v = migration.value, counts = migrationCounts.value
  if (!v || !counts || v.status !== 'bounded_attribution_unknown' || !safeCount(v.route_switches) || v.route_switches < 1 || v.unknown_transitions !== 0 || v.extra_cold_input_min_tokens !== 0 || v.extra_cold_input_max_tokens !== counts.nonRead) return undefined
  return counts.nonRead
})
const migrationStatus = computed(() => {
  const v = migration.value
  const states = ['not_collected', 'partial', 'unsupported_transport', 'cache_usage_unavailable', 'invalid_boundary', 'unknown_route', 'no_observed_route_change', 'bounded_attribution_unknown']
  const status = migrationScope.value && v && states.includes(v.status) && (v.status !== 'bounded_attribution_unknown' || migrationBound.value != null) ? v.status : 'not_collected'
  return t(`requestTiming.cacheMigration.states.${status}`)
})
const migrationTransitions = computed(() => {
  const v = migration.value
  if (!migrationScope.value || !v || !['unknown_route', 'no_observed_route_change', 'bounded_attribution_unknown'].includes(v.status) || !safeCount(v.route_switches) || !safeCount(v.unknown_transitions)) return undefined
  return t('requestTiming.cacheMigration.transitions', { changed: v.route_switches, unknown: v.unknown_transitions })
})
const phaseRateScopeKey = computed(() => {
  const rate = outputRate.value
  if (!rate) return undefined
  if (trace.value?.ttft_mode === 'ws_turn') {
    if (rate.scope === 'physical_websocket_turn_responses_text_at_parser' && rate.source === 'canonical_responses_terminal_usage_and_text_events') return 'requestTiming.outputRate.wsScope'
    return undefined
  }
  const scopes: Record<string, [string, string]> = {
    final_physical_http_responses_text_at_parser: ['canonical_responses_terminal_usage_and_text_events', 'scope'],
    final_physical_http_chat_completions_text_at_parser: ['canonical_chat_completions_usage_and_text_events', 'chatScope'],
    final_physical_http_messages_text_at_parser: ['canonical_messages_usage_and_text_events', 'messagesScope'],
    final_physical_http_gemini_text_at_parser: ['canonical_gemini_usage_and_text_events', 'geminiScope']
  }
  const scope = Object.prototype.hasOwnProperty.call(scopes, rate.scope) ? scopes[rate.scope] : undefined
  if (scope && rate.source === scope[0]) return `requestTiming.outputRate.${scope[1]}`
  return undefined
})
const phaseRate = computed(() => {
  const rate = outputRate.value
  const value = rate?.tokens_per_second
  if (!phaseRateScopeKey.value || rate?.status !== 'estimated' || value == null || !Number.isFinite(value) || value <= 0 ||
    rate.window_ms == null || !Number.isFinite(rate.window_ms) || rate.window_ms < 250 ||
    rate.non_reasoning_tokens == null || !Number.isSafeInteger(rate.non_reasoning_tokens) || rate.non_reasoning_tokens < 32) return t('requestTiming.missing')
  return `${value >= 100 ? Math.round(value) : value.toFixed(1)} t/s`
})
const phaseRateStatus = computed(() => {
  const status = phaseRateScopeKey.value ? outputRate.value?.status ?? 'not_collected' : 'not_collected'
  const states = ['estimated', 'not_collected', 'not_reported', 'invalid_counters', 'diagnostic_limit', 'unsupported_usage',
    'invalid_terminal', 'unsupported_output', 'no_text_output', 'partial', 'insufficient_stream', 'incomplete_stream',
    'short_output', 'insufficient_window', 'invalid_boundary', 'incomplete_capture', 'invalid_protocol']
  return t(`requestTiming.outputRate.states.${states.includes(status) ? status : 'not_collected'}`)
})
const phaseRateTokens = computed(() => {
  const value = outputRate.value?.non_reasoning_tokens
  return value != null && Number.isSafeInteger(value) && value >= 0 ? value.toLocaleString() : t('requestTiming.missing')
})
const phaseReasoningTokens = computed(() => {
  const value = outputRate.value?.reasoning_tokens
  return value != null && Number.isSafeInteger(value) && value >= 0 ? value.toLocaleString() : t('requestTiming.missing')
})
const label = (name: string) => t(`requestTiming.fields.${name}`)
const ms = (value: number | null | undefined) => value == null ? t('requestTiming.missing') : value < 1 ? '<1ms' : value < 1000 ? `${value.toFixed(0)}ms` : `${(value / 1000).toFixed(2)}s`
const bytes = (value: number) => value < 0 ? t('requestTiming.missing') : `${(value / 1048576).toFixed(3)} MiB`
const yes = (value: boolean | null) => value == null ? t('requestTiming.missing') : t(value ? 'requestTiming.yes' : 'requestTiming.no')
const delta = (events: Record<string, number>, start: string, end: string) => events[start] == null || events[end] == null || events[end] < events[start] ? undefined : events[end] - events[start]
const duration = (name: string) => { const items = trace.value?.spans.filter(s => s.name === name); return items?.length ? items.reduce((n, s) => n + s.end_ms - s.start_ms, 0) : undefined }
const healthLevels: TimingHealth[] = ['good', 'warn', 'slow', 'critical', 'neutral']
const healthLabel = (health: TimingHealth) => t(`requestTiming.health.${health}`)
interface DetailRow { name: string; value: string; health: TimingHealth }
const row = (name: string, value: string, health: TimingHealth = 'neutral'): DetailRow => ({ name: label(name), value, health })
const timed = (name: string, value: number | null | undefined, scale: TimingScale = 'stage') => row(name, ms(value), timingHealth(value, scale))
const flag = (name: string, value: boolean | null | undefined, goodValue: boolean, negative: TimingHealth = 'warn') => row(name, yes(value ?? null), value == null ? 'neutral' : value === goodValue ? 'good' : negative)
const statusHealth = (status: number): TimingHealth => status >= 500 ? 'critical' : status >= 400 ? 'warn' : status >= 200 && status < 300 ? 'good' : 'neutral'
const queueWaitMs = computed(() => props.record?.queue_timing?.status === 'measured' ? props.record.queue_wait_ms ?? null : null)
const serverFirstMs = computed(() => props.record?.queue_timing?.status === 'measured' ? props.record.queue_timing.server_first_token_ms : null)
const serverTotalMs = computed(() => props.record?.queue_timing?.status === 'measured' ? props.record.queue_timing.server_duration_ms : null)
const summaries = computed(() => [
  { label: t('requestTiming.legacyFirst'), value: props.record?.first_token_ms, scale: 'first' as const },
  { label: t('requestTiming.queueWait'), value: queueWaitMs.value, scale: 'first' as const },
  { label: t('requestTiming.serverFirst'), value: serverFirstMs.value, scale: 'first' as const },
  { label: t('requestTiming.legacyTotal'), value: props.record?.duration_ms, scale: 'total' as const },
  { label: t('requestTiming.serverTotal'), value: serverTotalMs.value, scale: 'total' as const },
  { label: t('requestTiming.requestTotal'), value: trace.value?.total_ms, scale: 'total' as const },
  { label: label('first_semantic'), value: trace.value?.events.first_semantic, scale: 'first' as const },
  { label: label('first_visible'), value: trace.value?.events.first_visible, scale: 'first' as const },
  { label: label('first_output_flush'), value: trace.value?.events.first_output_flush, scale: 'first' as const }
].map(item => ({ ...item, health: timingHealth(item.value, item.scale) })))
const diagnosis = computed(() => {
  const d = trace.value
  if (!d) return ''
  if (d.outcome === 'failed' || d.terminal === 'failed' || d.downstream_error || d.status >= 400) return t('requestTiming.health.failed')
  if (d.client_disconnect || d.canceled || d.terminal === 'incomplete') return t('requestTiming.health.interrupted')
  const first = d.events.first_visible ?? d.events.first_semantic
  if (first == null) return t('requestTiming.health.insufficient')
  return t(timingHealth(first, 'first') === 'good' ? 'requestTiming.health.firstGood' : 'requestTiming.health.firstSlow', { time: ms(first) })
})
const largestStage = computed(() => {
  const candidates = (trace.value?.spans ?? []).filter(s => stageScale(s.name) === 'stage')
  return candidates.reduce<TimingSpan | undefined>((best, s) => !best || s.end_ms - s.start_ms > best.end_ms - best.start_ms ? s : best, undefined)
})
const spanHealth = (span: TimingSpan) => timingHealth(span.end_ms - span.start_ms, stageScale(span.name))
const groups = computed(() => {
  const d = trace.value
  if (!d) return []
  const e = d.events
  return [
    { title: t('requestTiming.inbound'), rows: [
      timed('body_read_start', e.body_read_start, 'neutral'), timed('body_first_wait', delta(e, 'body_read_start', 'body_first_byte')),
      timed('body_receive', delta(e, 'body_first_byte', 'body_received')), timed('body_read_ms', e.body_read_start == null ? undefined : d.body_read_ms),
      row('body_bytes', bytes(d.body_bytes)), row('body_rate', e.body_read_start == null ? t('requestTiming.missing') : d.body_read_ms < 1 ? t('requestTiming.health.readTooBrief') : `${(d.body_bytes / 1048576 / (d.body_read_ms / 1000)).toFixed(2)} MiB/s`),
      flag('body_complete', e.body_read_start == null ? null : d.body_complete, true)
    ] },
    { title: t('requestTiming.internal'), rows: ['api_key_auth', 'model_allowlist', 'composite_routing', 'handler_body_read', 'security_audit', 'billing_check', 'user_queue', 'account_selection', 'account_queue', 'upstream_credentials', 'build_upstream_request', 'handler'].map(name => timed(name, duration(name), stageScale(name))) },
    { title: t('requestTiming.result'), rows: [
      row('status', String(d.status), statusHealth(d.status)), row('attempts', String(d.attempts.filter(a => a.kind !== 'http').length)),
      row('terminal', d.terminal ? t(`requestTiming.${d.terminal}`) : t('requestTiming.missing'), d.terminal === 'completed' ? 'good' : d.terminal === 'failed' ? 'critical' : d.terminal === 'incomplete' ? 'warn' : 'neutral'),
      row('outcome', d.outcome ? t(`requestTiming.${d.outcome}`) : t('requestTiming.missing'), d.outcome === 'success' ? 'good' : d.outcome === 'failed' ? 'critical' : d.outcome === 'client_disconnected' ? 'warn' : 'neutral'),
      flag('client_disconnect', d.client_disconnect, false), flag('canceled', d.canceled, false), flag('downstream_error', d.downstream_error, false, 'critical'), row('downstream_bytes', bytes(d.downstream_bytes)),
      timed('downstream_write_ms', d.downstream_write_ms, 'neutral'), row('ttft_mode', d.ttft_mode || t('requestTiming.missing'))
    ] }
  ]
})
const orderedSpans = computed(() => [...(trace.value?.spans ?? [])].sort((a, b) => a.start_ms - b.start_ms))
const bar = (span: TimingSpan) => { const total = Math.max(trace.value?.total_ms ?? 1, 1); return { marginLeft: `${Math.min(100, span.start_ms / total * 100)}%`, width: `${Math.max(0.2, (span.end_ms - span.start_ms) / total * 100)}%`, maxWidth: '100%' } }
function attemptTitle(a: TimingAttempt): string {
  const attempts = trace.value?.attempts ?? []
  const egress = attempts.filter(item => item.kind !== 'http')
  if (a.kind !== 'http') return `${t('requestTiming.attempt')} #${egress.findIndex(item => item.number === a.number) + 1}`
  const parent = egress.findIndex(item => item.number === a.parent) + 1
  const child = attempts.filter(item => item.kind === 'http' && item.parent === a.parent).findIndex(item => item.number === a.number) + 1
  return `${t('requestTiming.httpAttempt')} #${parent}.${child}`
}
function attemptRows(a: TimingAttempt): DetailRow[] {
  const e = a.events
  return [
    timed('attempt_total', a.end_ms == null ? undefined : a.end_ms - a.start_ms, 'neutral'), row('status', a.status > 0 ? String(a.status) : t('requestTiming.missing'), statusHealth(a.status)),
    row('transport_result', a.cleanup_canceled ? t('requestTiming.health.normalClose') : a.error === 'canceled' && !a.terminal ? t('requestTiming.health.legacyCancel') : a.error ? t(`requestTiming.${a.error}`) : t('requestTiming.health.noTransportError'), a.error ? (a.error === 'canceled' ? 'warn' : 'critical') : a.cleanup_canceled ? 'good' : 'neutral'),
    // A new connection and closing after the terminal event without EOF are normal.
    row('reused', yes(a.reused), a.reused === true ? 'good' : 'neutral'), timed('connection', delta(e, 'connection_start', 'connection_ready')),
    ...['dns', 'tcp', 'tls'].map(name => ({ name: name.toUpperCase(), value: ms(delta(e, `${name}_start`, `${name}_end`)), health: timingHealth(delta(e, `${name}_start`, `${name}_end`), 'stage') })),
    timed('request_write', delta(e, 'connection_ready', 'request_written')), timed('wait_first_byte', delta(e, 'request_written', 'first_byte'), 'first'),
    row('content_encoding', a.content_encoding || t('requestTiming.missing')),
    row('auto_decompressed', a.content_encoding ? yes(a.auto_decompressed ?? false) : t('requestTiming.missing')),
    ...['raw_body_first', 'decoded_body_first', 'sse_parsed_first', 'sse_visible_first', 'tool_converted_first', 'tool_visible_first'].map(name => timed(name, e[name], 'neutral')),
    timed('decode_wait', delta(e, 'raw_body_first', 'decoded_body_first'), 'neutral'),
    timed('sse_wait', delta(e, 'decoded_body_first', 'sse_parsed_first'), 'neutral'),
    timed('tool_wait', delta(e, 'sse_visible_first', 'tool_visible_first'), 'neutral'),
    timed('first_semantic', e.first_semantic, 'neutral'), timed('first_visible', e.first_visible, 'neutral'),
    timed('response_headers', e.response_headers == null ? undefined : e.response_headers - a.start_ms, 'first'),
    timed('stream_transfer', a.end_ms == null || e.response_headers == null ? undefined : a.end_ms - e.response_headers, 'neutral'),
    row('request_bytes', bytes(a.request_bytes)), row('response_bytes', bytes(a.response_bytes)), row('body_eof', yes(a.body_eof))
  ]
}
async function load() {
  controller?.abort(); const current = new AbortController(); controller = current
  traces.value = []; selected.value = 0; error.value = false
  if (!props.record) { loading.value = false; return }
  loading.value = true
  try { const data = await (observerMode ? observerUsageAPI.getTiming : getUsageTiming)(props.record.id, current.signal); if (controller === current) { traces.value = data.traces; retention.value = data.retention_days } }
  catch { if (!current.signal.aborted && controller === current) error.value = true }
  finally { if (controller === current) loading.value = false }
}
watch(() => props.record?.id, load, { immediate: true })
onUnmounted(() => controller?.abort())
</script>
