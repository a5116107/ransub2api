<template>
  <section class="min-w-0 space-y-4" data-testid="upstream-line-cards" :aria-label="t('upstreamOps.lines.title')">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div><h2 class="font-semibold">{{ t('upstreamOps.lines.title') }}</h2><p class="mt-1 text-xs text-gray-500">{{ t('upstreamOps.lines.scope') }}</p></div>
      <label class="text-sm">{{ t('upstreamOps.lines.participation') }}<select v-model="participation" class="input ml-2" data-testid="line-participation"><option value="all">{{ t('upstreamOps.lines.all') }}</option><option value="on">{{ t('upstreamOps.lines.on') }}</option><option value="off">{{ t('upstreamOps.lines.off') }}</option></select></label>
    </div>
    <p v-if="!members.length" class="card p-4" role="status">{{ t('upstreamOps.lines.empty') }}</p>
    <article v-for="member in members" :key="member.id" class="card min-w-0 overflow-hidden" data-testid="upstream-line-card" :data-account-id="member.id">
      <header class="flex flex-wrap items-start justify-between gap-3 border-b border-gray-100 p-4 dark:border-dark-600">
        <div class="min-w-0"><h3 class="break-words font-semibold">{{ member.name }} <span class="text-xs font-normal text-gray-500">#{{ member.id }}</span></h3><p class="mt-1 break-all text-xs text-gray-500">{{ member.key }}</p></div>
        <div class="text-xs"><span class="rounded-md bg-gray-100 px-2 py-1 dark:bg-dark-700">{{ t(member.deleted ? 'upstreamOps.lines.deleted' : member.schedulable === true && member.status === 'active' ? 'upstreamOps.lines.on' : member.schedulable === false ? 'upstreamOps.lines.off' : 'upstreamOps.unknown') }}</span><p class="mt-2 text-gray-500">{{ t('upstreamOps.lines.notCertified') }}</p></div>
      </header>
      <dl class="grid min-w-0 gap-5 p-4 sm:grid-cols-2 xl:grid-cols-4">
        <div class="min-w-0"><dt class="mb-2 text-xs text-gray-500">{{ t('upstreamOps.lines.purchaseRate') }}</dt><dd><UpstreamBillingRateCell v-if="member.type === 'apikey'" :account="billingAccount(member)" :now="now" read-only :can-configure="false" /><span v-else>{{ t('upstreamOps.unknown') }}</span><p class="mt-2 text-xs text-gray-500">{{ t('upstreamOps.lines.localRates', { admission: number(member.rate_multiplier), estimate: number(member.cost_multiplier) }) }}</p></dd></div>
        <div class="min-w-0"><dt class="mb-2 text-xs text-gray-500">{{ t('upstreamOps.lines.answerAndIdentity') }}</dt><dd><template v-if="quality(member.id)"><code class="break-all text-xs">{{ quality(member.id)?.measured_model || t('upstreamOps.unknown') }}</code><QualityRoundAssessment :round="quality(member.id)!" :queried-at="snapshot.quality.queried_at" /></template><p v-else class="text-sm">{{ t('upstreamOps.lines.noQuality') }}</p><p class="mt-2 text-xs text-gray-500">{{ t('upstreamOps.lines.identityUnknown') }}</p></dd></div>
        <div class="min-w-0"><dt class="mb-2 text-xs text-gray-500">{{ t('upstreamOps.lines.cacheAndWait') }}</dt><dd><p class="text-sm" data-testid="line-cache-unknown">{{ t('upstreamOps.lines.noComparable') }}</p><p class="mt-2 text-xs text-gray-500">{{ t('upstreamOps.lines.cacheScope') }}</p></dd></div>
        <div class="min-w-0"><dt class="mb-2 text-xs text-gray-500">{{ t('upstreamOps.lines.activity') }}</dt><dd class="space-y-2 text-sm"><p>{{ t('upstreamOps.lines.usageRows', { count: member.metrics?.requests ?? t('upstreamOps.unknown') }) }}</p><p>P50 / P90 · {{ number(member.metrics?.p50_ms) }} / {{ number(member.metrics?.p90_ms) }} ms</p><p class="text-xs text-gray-500">{{ t('upstreamOps.lines.completionUnknown') }}</p><p class="text-xs text-gray-500">{{ t('upstreamOps.lines.capacity', { count: member.concurrency ?? t('upstreamOps.unknown') }) }}</p></dd></div>
      </dl>
      <section class="min-w-0 border-t border-gray-100 p-4 dark:border-dark-600" :aria-label="t('upstreamOps.lines.availability.title')" data-testid="line-availability">
        <h4 class="text-sm font-medium">{{ t('upstreamOps.lines.availability.title') }}</h4>
        <p class="mt-1 text-xs text-gray-500">{{ t('upstreamOps.lines.availability.scope') }}</p>
        <p v-if="!availabilityReady" class="mt-2 text-sm" role="status">{{ t(snapshot.availability?.status === 'query_failed' ? 'upstreamOps.queryFailed' : 'upstreamOps.lines.availability.unread') }}</p>
        <p v-else-if="!availabilityReports(member.id).length" class="mt-2 text-sm text-gray-500">{{ t('upstreamOps.lines.availability.empty') }}</p>
        <div v-for="report in availabilityReports(member.id)" :key="report.plan_id" class="mt-3 grid min-w-0 gap-3 sm:grid-cols-2 lg:grid-cols-3">
          <p v-if="!report.models.length" class="text-sm text-gray-500">{{ t('upstreamOps.lines.availability.unread') }}</p>
          <div v-for="model in report.models" :key="model.model_id" class="min-w-0 rounded-lg border p-3 dark:border-dark-600" data-testid="line-availability-model">
            <div class="flex flex-wrap items-start justify-between gap-2"><code class="break-all text-sm">{{ model.model_id }}</code><span class="rounded px-2 py-1 text-xs font-medium" :class="availabilityState(report, model) === 'pass' ? 'bg-emerald-50 text-emerald-800 dark:bg-emerald-900/30 dark:text-emerald-300' : availabilityState(report, model) === 'fail' ? 'bg-red-50 text-red-800 dark:bg-red-900/30 dark:text-red-300' : 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'" data-testid="line-availability-state">{{ t(`upstreamOps.lines.availability.${availabilityState(report, model)}`) }}</span></div>
            <p class="mt-2 text-xs text-gray-500" data-testid="line-availability-reason">{{ availabilityReason(report, model) }}</p>
            <p class="mt-1 break-all text-xs">{{ model.decision?.scope?.model || t('upstreamOps.unknown') }} · {{ model.decision?.scope?.protocol || t('upstreamOps.unknown') }} · {{ model.decision?.scope?.service_tier || t('upstreamOps.unknown') }} / {{ model.decision?.scope?.reasoning_effort || t('upstreamOps.unknown') }}</p>
            <p class="mt-1 text-xs text-gray-500">{{ t('upstreamOps.lines.availability.observed') }} {{ date(model.observation?.finished_at ?? '') }} · {{ t(`upstreamOps.lines.availability.${model.observation?.status === 'pass' ? 'pass' : model.observation?.status === 'fail' ? 'fail' : 'unknown'}`) }}</p>
            <p class="mt-1 text-xs text-gray-500">{{ t('upstreamOps.lines.availability.decision') }} {{ date(model.decision?.finished_at ?? '') }}</p>
            <p class="mt-1 text-xs text-gray-500">{{ t('upstreamOps.lines.availability.until') }} {{ date(model.valid_until ?? '') }}</p>
            <a class="mt-2 inline-block text-xs text-primary-600 underline dark:text-primary-400" :href="`/admin/account-quality?plan_id=${report.plan_id}`">{{ t('upstreamOps.plan') }} #{{ report.plan_id }}</a>
          </div>
        </div>
        <p v-if="availabilityReady && snapshot.availability?.data?.truncated" class="mt-2 text-xs text-amber-800 dark:text-amber-300" role="status">{{ t('upstreamOps.lines.availability.capped') }}</p>
      </section>
      <section class="min-w-0 border-t border-gray-100 p-4 dark:border-dark-600" :aria-label="t('upstreamOps.lines.tests')">
        <div class="mb-3 flex flex-wrap items-center justify-between gap-2"><h4 class="text-sm font-medium">{{ t('upstreamOps.lines.tests') }}</h4><a class="text-xs text-primary-600 underline dark:text-primary-400" href="/admin/pelican-tests">{{ t('upstreamOps.lines.testPlans') }}</a></div>
        <p v-if="!testsReady" class="text-sm" role="status">{{ t(snapshot.model_tests?.status === 'query_failed' ? 'upstreamOps.queryFailed' : 'upstreamOps.lines.testScopeUnknown') }}</p>
        <p v-else-if="!tests(member.id).length" class="text-sm text-gray-500">{{ t('upstreamOps.lines.noTests') }}</p>
        <div class="grid min-w-0 gap-3 sm:grid-cols-2 lg:grid-cols-3">
          <div v-for="result in tests(member.id)" :key="result.id" class="min-w-0 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="line-pelican-result">
            <p class="text-xs font-medium">{{ t('upstreamOps.lines.pelican') }}</p><code class="mt-1 block break-all text-sm" data-testid="line-test-model">{{ result.model || t('upstreamOps.lines.modelNotRecorded') }}</code>
            <p class="mt-1 text-xs text-gray-500">{{ result.reasoning_effort || t('upstreamOps.unknown') }} · G{{ result.group_id }} · #{{ result.id }}</p>
            <p class="mt-2 text-sm" :class="result.status === 'failed' ? 'text-amber-800 dark:text-amber-300' : ''" data-testid="line-test-outcome">{{ t(result.status === 'success' ? 'upstreamOps.lines.generated' : result.status === 'failed' ? 'upstreamOps.lines.executionFailed' : result.status === 'deferred' ? 'upstreamOps.lines.deferred' : 'upstreamOps.unknown') }}</p>
            <p class="mt-1 text-xs text-gray-500">{{ date(result.started_at) }}</p>
            <p class="mt-1 break-words text-xs text-gray-500" data-testid="line-test-attribution">{{ t('upstreamOps.lines.finalAccount', { id: result.account_id }) }}<template v-for="(attempt, index) in result.attempts" :key="index"> · {{ t('upstreamOps.lines.priorAttempt', { id: attempt.account_id }) }}</template></p>
            <div v-if="validArtwork(result)" class="mt-3 max-w-60" data-testid="line-historical-artwork">
              <p class="mb-2 text-xs text-gray-500">{{ t('upstreamOps.lines.historicalArtwork') }}</p>
              <PelicanShowcaseCard :item="showcaseItem(result.artwork!)" :group-name="`G${result.group_id}`" :body="bodies[result.artwork!.id]" @visible="loadArtwork(result.artwork!)" @open="openArtwork(result.artwork!)" />
              <button v-if="bodies[result.artwork!.id]?.status === 'error'" class="mt-2 text-xs text-primary-600 underline" @click="loadArtwork(result.artwork!, true)">{{ t('upstreamOps.lines.retryArtwork') }}</button>
            </div>
          </div>
        </div>
        <p class="mt-3 text-xs text-gray-500">{{ t('upstreamOps.lines.otherTestsUnknown') }}</p>
      </section>
      <footer class="flex flex-wrap items-center justify-between gap-3 border-t border-gray-100 bg-gray-50 px-4 py-3 dark:border-dark-600 dark:bg-dark-800"><p class="text-xs text-gray-500">{{ t('upstreamOps.lines.stickyScope') }}</p><div class="flex flex-wrap gap-2"><button class="btn btn-secondary" data-testid="line-details" @click="emit('select', member.id)">{{ t('upstreamOps.lines.details') }}</button><button class="btn btn-secondary" :disabled="member.deleted" data-testid="line-price-edit" @click="emit('edit', member.id)">{{ t('upstreamOps.lines.priceEdit') }}</button><button v-if="member.type === 'apikey'" class="btn btn-secondary" :disabled="member.deleted" @click="emit('configure', member.id)">{{ t('upstreamOps.lines.priceAuthorization') }}</button></div></footer>
    </article>
    <p v-if="testsReady" class="text-xs text-gray-500">{{ t('upstreamOps.lines.testsCoverage', { shown: snapshot.model_tests?.data?.rows.length ?? 0, total: snapshot.model_tests?.data?.total ?? 0 }) }}</p>
    <BaseDialog :show="!!selectedArtwork" :title="t('upstreamOps.lines.artworkTitle')" width="extra-wide" @close="selectedArtwork = null">
      <template v-if="selectedArtwork"><p class="mb-3 break-words text-sm">{{ selectedArtwork.model || t('upstreamOps.lines.modelNotRecorded') }} · {{ selectedArtwork.reasoning_effort || t('upstreamOps.unknown') }} · {{ date(selectedArtwork.started_at) }} · #{{ selectedArtwork.id }}</p><div v-if="bodies[selectedArtwork.id]?.status === 'ready'" class="h-[65vh] min-w-0"><PelicanArtworkPreview :html="bodies[selectedArtwork.id].html" :title="selectedArtwork.model || t('upstreamOps.lines.modelNotRecorded')" /></div><p v-else role="status">{{ t(bodies[selectedArtwork.id]?.status === 'loading' ? 'common.loading' : 'upstreamOps.queryFailed') }}</p><p class="mt-3 text-xs text-gray-500">{{ t('upstreamOps.lines.historicalArtwork') }}</p></template>
    </BaseDialog>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'
import type { AccountBasis, ModelTestArtwork, ModelTestRow, OperationsSnapshot, AvailabilityLineReport, AvailabilityLineModel } from '@/api/admin/upstreamOperations'
import { getResult } from '@/api/admin/pelicanTests'
import { extractPelicanHtml } from '@/utils/pelicanHtml'
import type { PelicanBody } from '@/components/user/pelican/pelicanShowcaseFormat'
import BaseDialog from '@/components/common/BaseDialog.vue'
import UpstreamBillingRateCell from '@/components/account/UpstreamBillingRateCell.vue'
import QualityRoundAssessment from './QualityRoundAssessment.vue'
import PelicanShowcaseCard from '@/components/user/pelican/PelicanShowcaseCard.vue'
import PelicanArtworkPreview from '@/components/user/pelican/PelicanArtworkPreview.vue'

const props = defineProps<{ snapshot: OperationsSnapshot; readElapsedMs?: number }>()
const emit = defineEmits<{ select: [id: number]; edit: [id: number]; configure: [id: number] }>()
const { t } = useI18n()
const participation = ref('all')
const bodies = ref<Record<number, PelicanBody>>({})
const selectedArtwork = ref<ModelTestArtwork | null>(null)
const controller = new AbortController()
const clock = ref(Date.now())
const elapsedClock = ref(performance.now())
const loadedAt = ref(performance.now())
const timer = window.setInterval(() => { clock.value = Date.now(); elapsedClock.value = performance.now() }, 1_000)
const now = computed(() => Math.max(clock.value, Date.parse(props.snapshot.queried_at) || 0))
watch(() => props.snapshot, () => { selectedArtwork.value = null; loadedAt.value = performance.now(); elapsedClock.value = loadedAt.value })
const members = computed(() => (props.snapshot.usage.members ?? []).filter(a => participation.value === 'all' || participation.value === 'on' && !a.deleted && a.schedulable === true && a.status === 'active' || participation.value === 'off' && (a.deleted || a.schedulable === false)))
const testsReady = computed(() => props.snapshot.model_tests?.status === 'ready' && props.snapshot.model_tests.data?.version === 1 && props.snapshot.model_tests.data.source === 'persisted_pelican_group_test_frozen_model' && Array.isArray(props.snapshot.model_tests.data.rows) && Number.isSafeInteger(props.snapshot.model_tests.data.total) && props.snapshot.model_tests.data.total >= props.snapshot.model_tests.data.rows.length)
const availabilityReady = computed(() => {
  const data = props.snapshot.availability?.data
  return props.snapshot.availability?.status === 'ready' && data?.version === 1 && data.source === 'current_account_availability_authority' && Array.isArray(data.reports) && Number.isSafeInteger(data.plan_total) && data.plan_total >= data.reports.length && Number.isSafeInteger(data.model_total) && data.model_total >= data.models_loaded && Number.isSafeInteger(data.models_loaded) && data.models_loaded === data.reports.reduce((count, report) => count + (Array.isArray(report.models) ? report.models.length : NaN), 0) && typeof data.truncated === 'boolean'
})
const availabilityReports = (id: number) => availabilityReady.value ? props.snapshot.availability!.data!.reports.filter(r => r.account_id === id) : []
function availabilityState(report: AvailabilityLineReport, model: AvailabilityLineModel) {
  if (!report.enabled) return 'paused'
  if (!['pass', 'fail'].includes(model.status)) return model.reason === 'expired' ? 'expired' : 'unknown'
  const evaluated = Date.parse(report.evaluated_at), until = Date.parse(model.valid_until ?? '')
  const decision = model.decision, scope = decision?.scope
  if (!report.configured || !Number.isFinite(evaluated) || !Number.isFinite(until) || decision?.version !== 1 || decision.status !== model.status || !Number.isFinite(Date.parse(decision.started_at)) || Date.parse(decision.finished_at) < Date.parse(decision.started_at) || !(Date.parse(decision.finished_at) <= evaluated) || ![2, 3, 4].includes(scope?.version ?? 0) || !scope?.model || !scope.protocol) return 'unknown'
  return until <= evaluated + Math.max(0, props.readElapsedMs ?? 0) + Math.max(0, elapsedClock.value - loadedAt.value) ? 'expired' : model.status
}
function availabilityReason(report: AvailabilityLineReport, model: AvailabilityLineModel) {
  const state = availabilityState(report, model)
  const known = ['not_observed', 'rule_changed', 'route_changed', 'new_fault_event', 'connection_not_selected', 'connection_not_observed', 'evidence_version_missing', 'invalid_evidence']
  const reason = state === 'paused' ? 'rule_paused' : state === 'expired' ? 'expired' : known.includes(model.reason) ? model.reason : state === 'pass' || state === 'fail' ? 'latest_decision' : 'unknown'
  return t(`upstreamOps.lines.availability.reasons.${reason}`)
}
const number = (v: number | undefined | null) => typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v.toLocaleString(undefined, { maximumFractionDigits: 4 }) : t('upstreamOps.unknown')
const date = (v: string) => Number.isFinite(Date.parse(v)) ? new Date(v).toLocaleString() : t('upstreamOps.unknown')
const billingAccount = (a: AccountBasis) => ({ id: a.id, type: a.type, platform: a.platform, extra: { upstream_billing_probe: a.billing_probe ?? undefined, upstream_billing_probe_enabled: a.billing_probe_enabled === true, new_api_apply_usd_rate: a.new_api_apply_usd_rate ?? undefined } } as Account)
const quality = (id: number) => props.snapshot.quality.status === 'ready' ? props.snapshot.quality.data?.rows.find(r => r.account_id === id) : undefined
const tests = (id: number) => testsReady.value ? props.snapshot.model_tests!.data!.rows.filter(r => r.account_id === id) : []
const validArtwork = (r: ModelTestRow) => r.artwork && r.artwork.account_id === r.account_id && r.artwork.group_id === r.group_id && r.artwork.model === r.model && r.artwork.reasoning_effort === r.reasoning_effort && r.artwork.id <= r.id && Number.isFinite(Date.parse(r.artwork.started_at))
const showcaseItem = (a: ModelTestArtwork) => ({ id: a.id, group_id: a.group_id, model_id: a.model ?? '', reasoning_effort: a.reasoning_effort ?? '', latency_ms: a.latency_ms ?? 0, generated_at: a.started_at })

async function loadArtwork(a: ModelTestArtwork, retry = false) {
  if (bodies.value[a.id] && !retry) return
  bodies.value[a.id] = { status: 'loading', html: '' }
  try {
    const result = await getResult(a.id, controller.signal)
    if (controller.signal.aborted) return
    if (result.id !== a.id || result.account_id !== a.account_id || result.group_id !== a.group_id || result.status !== 'success' || (result.pelican_config?.model_id ?? null) !== a.model || (result.pelican_config?.reasoning_effort ?? null) !== a.reasoning_effort) throw new Error('Result attribution changed')
    const html = extractPelicanHtml(result.response_text ?? '')
    bodies.value[a.id] = { status: html ? 'ready' : 'invalid', html }
  } catch {
    if (!controller.signal.aborted) bodies.value[a.id] = { status: 'error', html: '' }
  }
}
function openArtwork(a: ModelTestArtwork) { selectedArtwork.value = a; void loadArtwork(a) }
onBeforeUnmount(() => { window.clearInterval(timer); controller.abort() })
</script>
