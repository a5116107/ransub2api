<template>
  <section class="mt-4 rounded-lg border border-amber-300 p-3 dark:border-amber-800" data-testid="probe-budget-group-queues">
    <h5 class="font-semibold">{{ t(`${prefix}.title`) }}</h5>
    <p class="mt-2 text-xs">{{ t(`${prefix}.hint`) }}</p>
    <p class="mt-1 text-xs">{{ t(`${prefix}.priorityHint`) }}</p>
    <div class="mt-3 grid gap-3 lg:grid-cols-2">
      <article v-for="row in rows" :key="row.mode" class="min-w-0 rounded-lg border border-amber-200 p-3 dark:border-amber-900" :data-group-probe-queue="row.mode">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h6 class="font-semibold">{{ t(`${prefix}.modes.${row.mode}`) }}</h6>
          <span class="rounded-md bg-amber-100 px-2 py-0.5 text-xs font-medium dark:bg-amber-900/50" :data-testid="`group-queue-state-${row.mode}`">{{ t(`${prefix}.states.${row.runtime?.state || 'unknown'}`) }}</span>
        </div>
        <template v-if="row.scan">
          <p class="mt-3 text-xs" :data-testid="`group-queue-scan-${row.mode}`">{{ t(`${prefix}.inventory`, row.scan) }}</p>
          <p v-if="row.priority" class="mt-1 text-xs" :data-testid="`group-queue-priority-${row.mode}`">{{ t(`${prefix}.priority`, row.priority) }}</p>
          <p v-else class="mt-1 text-xs" :data-testid="`group-queue-priority-unknown-${row.mode}`">{{ t(`${prefix}.priorityUnknown`) }}</p>
          <p class="mt-1 break-words text-xs">{{ t(`${prefix}.scanAt`) }} <time :datetime="row.scan.observed_at">{{ displayTime(row.scan.observed_at) }}</time></p>
        </template>
        <p v-else class="mt-3 text-xs">{{ t(`${prefix}.unobserved`) }}</p>
        <template v-if="row.cycle">
          <dl class="mt-3 grid grid-cols-2 gap-2 text-xs" :data-testid="`group-queue-cycle-${row.mode}`">
            <div v-for="metric in metrics" :key="metric"><dt>{{ t(`${prefix}.metrics.${metric}`) }}</dt><dd class="font-mono">{{ row.cycle[metric] }}</dd></div>
          </dl>
          <p class="mt-3 break-words text-xs" :data-testid="`group-queue-oldest-${row.mode}`">
            {{ t(`${prefix}.oldest`) }}
            <time v-if="row.cycle.oldest_pending_at" :datetime="row.cycle.oldest_pending_at">{{ displayTime(row.cycle.oldest_pending_at) }}</time>
            <span v-else>{{ t(`${prefix}.noWaiting`) }}</span>
          </p>
          <p class="mt-1 break-words text-xs">{{ t(`${prefix}.cycleStarted`) }} <time :datetime="row.cycle.started_at">{{ displayTime(row.cycle.started_at) }}</time></p>
          <p v-if="row.cycle.finished_at" class="mt-1 break-words text-xs">{{ t(`${prefix}.cycleFinished`) }} <time :datetime="row.cycle.finished_at">{{ displayTime(row.cycle.finished_at) }}</time></p>
          <MonitoringProbeBudgetNotice v-if="row.cycle.budget_deferral" class="mt-3" :deferral="row.cycle.budget_deferral" :title="t(`${prefix}.deferredTitle`)" />
        </template>
        <p v-else class="mt-3 text-xs">{{ t(`${prefix}.countsUnknown`) }}</p>
        <details v-if="row.runtime" class="mt-3 text-xs">
          <summary class="cursor-pointer">{{ t(`${prefix}.details`) }}</summary>
          <p class="mt-2 break-all font-mono">{{ row.runtime.instance_id || t(`${prefix}.states.unknown`) }}</p>
          <p class="mt-1 break-words">{{ t(`${prefix}.readAt`) }} <time :datetime="row.runtime.observed_at">{{ displayTime(row.runtime.observed_at) }}</time></p>
        </details>
      </article>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import MonitoringProbeBudgetNotice from '@/components/admin/account/MonitoringProbeBudgetNotice.vue'
import type { MonitoringProbeGroupQueues, MonitoringProbeQueueRuntime as QueueRuntime } from '@/types/monitoringProbeBudget'

const props = defineProps<{ queues?: MonitoringProbeGroupQueues | null }>()
const { t, locale } = useI18n()
const prefix = 'admin.settings.upstreamBillingProbe.budget.groupQueues'
const metrics = ['queued', 'active', 'finished', 'deferred'] as const
const states = new Set(['unavailable', 'unobserved', 'scanning', 'queued', 'running', 'idle', 'budget_paused', 'deferred', 'canceled', 'inactive', 'not_due', 'inventory_unavailable', 'coordination_unavailable', 'claim_changed'])
const timestamp = (value: unknown): value is string => typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(value) && Number.isFinite(Date.parse(value))
const counts = (...values: unknown[]) => values.every(value => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0)
const displayTime = (value: string) => new Date(value).toLocaleString(locale.value === 'zh' ? 'zh-CN' : 'en-US')

function runtime(value: QueueRuntime | null | undefined): QueueRuntime | null {
  if (!value || value.version !== 1 || value.scope !== 'responding_process' || !timestamp(value.observed_at) || !states.has(value.state)) return null
  const id = value.instance_id
  if (typeof id !== 'string' || !(/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(id) || value.state === 'unavailable' && id === '' && value.last_scan === null && value.cycle === null)) return null
  if (['unavailable', 'unobserved'].includes(value.state) && (value.last_scan !== null || value.cycle !== null)) return null
  return value
}

function inventory(value: QueueRuntime | null) {
  const scan = value?.last_scan
  return scan && timestamp(scan.observed_at) && Date.parse(scan.observed_at) <= Date.parse(value!.observed_at) &&
    counts(scan.targets, scan.due) && scan.due <= scan.targets ? scan : null
}

function cycle(value: QueueRuntime | null, scan: ReturnType<typeof inventory>) {
  const run = value?.cycle
  if (!run || !scan || !timestamp(run.started_at) || Date.parse(run.started_at) !== Date.parse(scan.observed_at) ||
    !counts(...metrics.map(key => run[key])) || run.active > 4 || run.deferred > run.finished ||
    !Number.isSafeInteger(run.queued + run.active + run.finished) || run.queued + run.active + run.finished !== scan.due) return null
  if (run.finished_at !== null && (!timestamp(run.finished_at) || Date.parse(run.finished_at) < Date.parse(run.started_at) ||
    Date.parse(run.finished_at) > Date.parse(value!.observed_at) || run.active !== 0)) return null
  if (['idle', 'budget_paused', 'deferred', 'canceled'].includes(value!.state) && run.finished_at === null ||
    ['running', 'queued'].includes(value!.state) && run.finished_at !== null) return null
  if (run.oldest_pending_at !== null && (!timestamp(run.oldest_pending_at) || Date.parse(run.oldest_pending_at) > Date.parse(run.started_at))) return null
  if (run.queued + run.active > 0 && run.oldest_pending_at === null) return null
  const denial = run.budget_deferral
  if (denial !== null && (!denial || typeof denial.reason !== 'string' || typeof denial.scope !== 'string' || typeof denial.retry_at !== 'string')) return null
  if (denial !== null && run.deferred === 0 && (!['running', 'coordination_unavailable'].includes(value!.state) || run.active === 0 ||
    ['key', 'key_minute', 'site', 'site_minute', 'plugin'].includes(denial.scope))) return null
  if (run.deferred > 0 && denial === null) return null
  return run
}

function priority(scan: ReturnType<typeof inventory>) {
  if (!scan || scan.priority_version !== 1 || !counts(scan.review_targets, scan.routine_targets) ||
    !Number.isSafeInteger(scan.review_targets! + scan.routine_targets!) || scan.review_targets! + scan.routine_targets! !== scan.due) return null
  return { review: scan.review_targets!, routine: scan.routine_targets! }
}

const rows = computed(() => (['v2', 'v3'] as const).map(mode => {
  const view = runtime(props.queues?.[mode])
  const scan = inventory(view)
  return { mode, runtime: view, scan, priority: priority(scan), cycle: cycle(view, scan) }
}))
</script>
