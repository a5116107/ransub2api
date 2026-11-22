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
              <label class="flex items-start gap-2"><input v-model="probe.candidate" type="checkbox" class="mt-1" :disabled="productID === undefined || Number.isNaN(productID)" data-testid="procurement-candidate" />{{ t(`${p}.candidateMode`) }}</label>
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
            <section class="min-w-0 space-y-2 border-t border-gray-200 pt-3 dark:border-dark-600" data-testid="quality-scope-evidence">
              <h4 class="text-sm font-medium">{{ t(`${p}.qualityTitle`) }}</h4>
              <span class="inline-block max-w-full break-words rounded-full px-3 py-1 text-sm" :class="evidenceState(item.quality_evidence) === 'pass' ? 'bg-green-100 text-green-900 dark:bg-green-900/30 dark:text-green-200' : evidenceState(item.quality_evidence) === 'fail' ? 'bg-red-100 text-red-900 dark:bg-red-900/30 dark:text-red-200' : 'bg-amber-100 text-amber-900 dark:bg-amber-900/30 dark:text-amber-200'" data-testid="quality-scope-status">{{ t(`${p}.qualityStates.${evidenceState(item.quality_evidence)}`) }}</span>
              <p class="text-sm font-medium text-amber-800 dark:text-amber-300">{{ t(`${p}.modelIdentityUnknown`) }}</p>
              <template v-if="item.quality_evidence">
                <p class="break-words text-sm">{{ t(`${p}.qualityReasons.${evidenceExpired(item.quality_evidence) ? 'expired' : item.quality_evidence.reason}`) }}</p>
                <dl class="grid gap-3 text-sm sm:grid-cols-2">
                  <div><dt class="text-gray-500">{{ t(`${p}.qualityRuleCoverage`) }}</dt><dd>{{ item.quality_evidence.confirmed_rules }} / {{ item.quality_evidence.rule_count }}</dd></div>
                  <div><dt class="text-gray-500">{{ t(`${p}.qualitySampleCoverage`) }}</dt><dd>{{ item.quality_evidence.valid_samples }} / {{ item.quality_evidence.samples }}</dd></div>
                  <div><dt class="text-gray-500">{{ t(`${p}.qualityEvaluatedAt`) }}</dt><dd class="break-all">{{ item.quality_evidence.evaluated_at }}</dd></div>
                  <div><dt class="text-gray-500">{{ t(`${p}.qualityValidUntil`) }}</dt><dd class="break-all">{{ item.quality_evidence.valid_until ?? t('upstreamOps.unknown') }}</dd></div>
                </dl>
                <p v-if="item.quality_evidence.other_scope_rules" class="text-xs text-gray-500">{{ t(`${p}.qualityOtherScopes`, { count: item.quality_evidence.other_scope_rules }) }}</p>
                <details v-if="item.quality_evidence.rules.length" class="min-w-0 text-sm" data-testid="quality-scope-rules">
                  <summary class="cursor-pointer">{{ t(`${p}.qualityRules`) }}</summary>
                  <ul class="mt-2 space-y-2">
                    <li v-for="rule in item.quality_evidence.rules" :key="rule.plan_id" class="break-words rounded border border-gray-200 p-2 dark:border-dark-600">
                      #{{ rule.plan_id }} · {{ t(`${p}.qualityStates.${evidenceState(rule)}`) }} · {{ t(`${p}.qualityReasons.${evidenceExpired(rule) ? 'expired' : rule.reason}`) }}
                      <p>{{ t(`${p}.qualitySampleCoverage`) }}: {{ rule.valid_samples }} / {{ rule.samples }}</p>
                      <p class="break-all">{{ t(`${p}.qualityValidUntil`) }}: {{ rule.valid_until ?? t('upstreamOps.unknown') }}</p>
                    </li>
                  </ul>
                </details>
              </template>
              <p v-else class="text-sm">{{ t(`${p}.qualityLegacy`) }}</p>
              <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.qualityBoundary`) }}</p>
            </section>
            <section class="min-w-0 space-y-2 border-t border-gray-200 pt-3 dark:border-dark-600" data-testid="availability-scope-evidence">
              <h4 class="text-sm font-medium">{{ t(`${p}.availabilityTitle`) }}</h4>
              <span class="inline-block max-w-full break-words rounded-full px-3 py-1 text-sm" :class="evidenceState(item.availability_evidence) === 'pass' ? 'bg-green-100 text-green-900 dark:bg-green-900/30 dark:text-green-200' : evidenceState(item.availability_evidence) === 'fail' ? 'bg-red-100 text-red-900 dark:bg-red-900/30 dark:text-red-200' : 'bg-amber-100 text-amber-900 dark:bg-amber-900/30 dark:text-amber-200'" data-testid="availability-scope-status">{{ t(`${p}.availabilityStates.${evidenceState(item.availability_evidence)}`) }}</span>
              <template v-if="item.availability_evidence">
                <p class="text-sm">{{ t(`${p}.availabilityCoverage`, { confirmed: item.availability_evidence.confirmed_rules, total: item.availability_evidence.rule_count }) }}</p>
                <p class="break-words text-sm">{{ t(`${p}.availabilityReasons.${evidenceExpired(item.availability_evidence) ? 'expired' : item.availability_evidence.reason}`) }}</p>
                <p class="break-all text-xs">{{ t(`${p}.qualityEvaluatedAt`) }}: {{ item.availability_evidence.evaluated_at }}</p>
                <p class="break-all text-xs">{{ t(`${p}.qualityValidUntil`) }}: {{ item.availability_evidence.valid_until ?? t('upstreamOps.unknown') }}</p>
              </template>
              <p v-else class="text-sm">{{ t(`${p}.availabilityLegacy`) }}</p>
              <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.availabilityBoundary`) }}</p>
            </section>
            <div v-if="item.product_procurement" class="min-w-0 space-y-2 border-t border-gray-200 pt-3 dark:border-dark-600" data-testid="product-procurement">
              <p class="break-words text-sm font-medium">{{ t(`${p}.productResult`, { id: item.product_procurement.group_id, revision: item.product_procurement.policy_revision }) }}</p>
              <div v-if="item.product_procurement.candidate" class="space-y-1 rounded-lg border border-amber-200 p-2 text-sm dark:border-amber-800" data-testid="product-candidate">
                <p>{{ t(`${p}.${item.product_procurement.candidate.in_product ? 'candidateAssociated' : 'candidateNotAssociated'}`) }}</p>
                <p>{{ t(`${p}.candidateSourceStates.${candidateSourceState(item.product_procurement.candidate)}`) }}</p>
                <p class="text-xs leading-5">{{ t(`${p}.candidateBoundary`) }}</p>
              </div>
              <p v-if="item.product_procurement.catalog_revision !== undefined" class="text-xs text-gray-500">{{ t(`${p}.revision`, { revision: item.product_procurement.catalog_revision }) }}</p>
              <span class="inline-block max-w-full break-words rounded-full px-3 py-1 text-sm" :class="productPriceState(item) === 'constraints_passed' ? 'bg-green-100 text-green-900 dark:bg-green-900/30 dark:text-green-200' : 'bg-amber-100 text-amber-900 dark:bg-amber-900/30 dark:text-amber-200'" data-testid="product-procurement-state">{{ t(`${p}.productStates.${productPriceState(item)}`) }}</span>
              <p class="break-all text-xs" data-testid="product-price-validity">{{ t(`${p}.priceSnapshotBoundary`) }} {{ item.product_procurement.price_valid_until ?? t('upstreamOps.unknown') }}</p>
              <section class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="product-evaluated-gates">
                <h4 class="text-sm font-medium">{{ t(`${p}.evaluatedGatesTitle`) }}</h4>
                <span class="inline-block max-w-full break-words rounded-full px-3 py-1 text-sm" :class="gateBadgeClass(evaluatedGatesState(item))" data-testid="product-evaluated-gates-status">{{ t(`${p}.evaluatedGatesStates.${evaluatedGatesState(item)}`) }}</span>
                <template v-if="item.product_procurement.evaluated_gates && [3, 4].includes(item.product_procurement.evaluated_gates.version)">
                  <p class="text-sm">{{ t(`${p}.evaluatedGatesCoverage`, { passed: item.product_procurement.evaluated_gates.passed_gates, required: item.product_procurement.evaluated_gates.required_gates }) }}</p>
                  <p class="break-words text-sm">{{ t(`${p}.evaluatedGatesReasons.${evidenceExpired(item.product_procurement.evaluated_gates) ? 'expired' : item.product_procurement.evaluated_gates.reason}`) }}</p>
                  <p class="break-all text-xs">{{ t(`${p}.qualityEvaluatedAt`) }}: {{ item.product_procurement.evaluated_gates.evaluated_at }}</p>
                  <p v-if="item.product_procurement.evaluated_gates.valid_until" class="break-all text-xs">{{ t(`${p}.qualityValidUntil`) }}: {{ item.product_procurement.evaluated_gates.valid_until }}</p>
                </template>
                <p v-else class="text-sm">{{ t(`${p}.evaluatedGatesLegacy`) }}</p>
                <ul v-if="item.product_procurement.evaluated_gates?.version === 4" class="grid min-w-0 gap-2 sm:grid-cols-2" data-testid="product-evaluated-gate-list" :aria-label="t(`${p}.gateDetailsTitle`)">
                  <li v-for="gate in item.product_procurement.evaluated_gates.gates" :key="gate.key" class="min-w-0 space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="product-evaluated-gate">
                    <div class="flex flex-wrap items-center justify-between gap-2">
                      <span class="text-sm font-medium">{{ t(`${p}.gateNames.${gate.key}`) }}</span>
                      <span class="max-w-full break-words rounded-full px-2 py-1 text-xs" :class="gateBadgeClass(evidenceExpired(gate) ? 'unknown' : gate.state)">{{ t(`${p}.gateStates.${evidenceExpired(gate) ? 'unknown' : gate.state}`) }}</span>
                    </div>
                    <p class="break-words text-sm">{{ t(`${p}.evaluatedGatesReasons.${evidenceExpired(gate) ? 'expired' : gate.reason}`) }}</p>
                    <p class="break-all text-xs text-gray-500">{{ t(`${p}.qualityValidUntil`) }}: {{ gate.valid_until ?? t('upstreamOps.unknown') }}</p>
                  </li>
                </ul>
                <p v-else-if="item.product_procurement.evaluated_gates?.version === 3" class="text-xs text-gray-500" data-testid="product-gate-details-legacy">{{ t(`${p}.gateDetailsLegacy`) }}</p>
                <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.evaluatedGatesBoundary`) }}</p>
              </section>
              <p class="break-words text-sm" data-testid="product-account-controls">{{ t(`${p}.accountControlsTitle`) }}: {{ t(`${p}.accountControlsStates.${accountControlsState(item)}`) }}</p>
              <p class="break-words text-sm" data-testid="product-account-controls-reason">{{ t(`${p}.accountControlsReasons.${accountControlsReason(item)}`) }}</p>
              <p v-if="item.product_procurement.account_controls?.valid_until" class="break-all text-xs">{{ t(`${p}.qualityValidUntil`) }}: {{ item.product_procurement.account_controls.valid_until }}</p>
              <p class="break-words text-sm" data-testid="product-answer-quality">{{ t(`${p}.answerQualityTitle`) }}: {{ t(`${p}.answerQualityStates.${productEvidenceState(item.product_procurement.answer_quality, item.quality_evidence)}`) }}</p>
              <p class="break-words text-sm" data-testid="product-availability">{{ t(`${p}.availabilityGateTitle`) }}: {{ t(`${p}.availabilityGateStates.${productEvidenceState(item.product_procurement.availability, item.availability_evidence)}`) }}</p>
              <dl class="grid gap-3 text-sm sm:grid-cols-2">
                <div><dt class="text-gray-500">{{ t(`${p}.productSale`) }}</dt><dd>{{ item.product_procurement.default_d }}</dd></div>
                <div><dt class="text-gray-500">{{ t(`${p}.productThreshold`) }}</dt><dd>{{ item.product_procurement.threshold }}</dd></div>
              </dl>
              <p v-if="item.product_procurement.reason && !['candidate_sources_unconfirmed', 'candidate_source_not_approved'].includes(item.product_procurement.reason)" class="break-all font-mono text-xs">{{ item.product_procurement.reason === 'rejected_procurement_bucket' ? t(`${p}.bucketRejected`) : item.product_procurement.reason === 'rejected_procurement_catalog_unavailable' ? t(`${p}.catalogUnavailable`) : item.product_procurement.reason }}</p>
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
import { useNow } from '@vueuse/core'
import { readProcurementCatalog, saveProcurementCatalog, previewProcurementScopes, type ProcurementCatalog, type ProcurementCatalogSnapshot, type ProcurementRequestScope, type ProcurementPreview, type ProcurementEvaluatedGate, type QualityScopeEvidence, type AvailabilityScopeEvidence } from '@/api/admin/procurementCatalog'
import { validProcurementSnapshot, validateProcurementCatalog } from '@/utils/procurementCatalog'
import qualityMessages from '@/i18n/locales/en/upstreamOps'

const { t } = useI18n(), p = 'upstreamOps.procurement'
const now = useNow({ interval: 1000 })
const saved = ref<ProcurementCatalogSnapshot | null>(null), latest = ref<ProcurementCatalogSnapshot | null>(null)
const busy = ref(false), error = ref(''), conflict = ref(false), success = ref(false), result = ref<ProcurementPreview | null>(null)
let rowKey = 0, alive = true, generation = 0
const controller = new AbortController()
const rows = ref<Array<{ key: number; code: string; name: string; min: string; max: string; unbounded: boolean }>>([])
const probe = ref({ product: '', candidate: false, account: 0, publicModel: '', model: '', protocol: 'responses_http', tier: 'unspecified', effort: 'unspecified', stream: false, summary: '', verbosity: '', encrypted: false })
const productID = computed(() => probe.value.product.trim() === '' ? undefined : /^[1-9]\d*$/.test(probe.value.product.trim()) && Number.isSafeInteger(Number(probe.value.product)) ? Number(probe.value.product) : NaN)
watch(productID, id => { if (id === undefined || Number.isNaN(id)) probe.value.candidate = false }, { flush: 'sync' })
const responses = computed(() => probe.value.protocol !== 'chat_http')
const candidate = computed<ProcurementCatalog>(() => ({ version: 1, revision: saved.value?.catalog.revision ?? 0, buckets: rows.value.map(row => ({ code: row.code, name: row.name, min_rate: row.min === '' ? null : row.min, max_rate: row.unbounded ? null : row.max })) }))
const validation = computed(() => validateProcurementCatalog(candidate.value))
const dirty = computed(() => Boolean(saved.value) && JSON.stringify(candidate.value) !== JSON.stringify(saved.value?.catalog))
const validProbe = computed(() => (!probe.value.candidate || productID.value !== undefined) && !Number.isNaN(productID.value) && Number.isSafeInteger(probe.value.account) && probe.value.account > 0 && probe.value.publicModel.trim() !== '' && probe.value.model.trim() !== '' && probe.value.tier !== '' && probe.value.effort !== '')
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
  const product = productID.value, evaluateCandidate = probe.value.candidate
  busy.value = true; error.value = ''; result.value = null
  try {
    const next = await previewProcurementScopes({ requests: [request], ...(product === undefined ? {} : { product_group_id: product }), ...(evaluateCandidate ? { evaluate_candidate: true } : {}) }, controller.signal)
    if (!validProcurementSnapshot(next) || next.catalog.revision !== saved.value.catalog.revision || !Number.isFinite(Date.parse(next.observed_at)) || !Array.isArray(next.results) || next.results.length !== 1 ||
      next.results[0]?.account_id !== request.account_id || next.results[0]?.public_model !== request.public_model || !next.results[0]?.scope ||
      !Object.entries(request.scope).every(([key, value]) => next.results[0]!.scope[key as keyof ProcurementRequestScope['scope']] === value) || next.results[0]?.qualification !== 'not_evaluated') throw new Error('unknown preview response')
    const projection = next.results[0]!.product_procurement
    const quality = next.results[0]!.quality_evidence
    const availability = next.results[0]!.availability_evidence
    if (projection?.account_controls !== undefined && !validAccountControls(projection.account_controls, next.observed_at)) throw new Error('unknown account controls')
    if (quality !== undefined && !validQualityEvidence(quality, next.observed_at)) throw new Error('unknown quality evidence')
    if (availability !== undefined && !validAvailabilityEvidence(availability, next.observed_at)) throw new Error('unknown availability evidence')
    if (product === undefined ? projection !== undefined : !projection || projection.version !== (evaluateCandidate ? 2 : 1) || !validCandidate(projection, evaluateCandidate) || projection.group_id !== product || projection.price_basis !== 'group_default' ||
      projection.catalog_revision !== undefined && (projection.catalog_revision !== next.catalog.revision || !Number.isSafeInteger(projection.catalog_revision)) ||
      !Number.isSafeInteger(projection.policy_revision) || projection.policy_revision < 0 || !Number.isFinite(projection.default_d) || projection.default_d < 0 || !Number.isFinite(projection.threshold) || projection.threshold < 0 ||
      !['constraints_passed', 'blocked', 'disabled', 'product_inactive', 'not_associated', 'not_evaluated'].includes(projection.state) ||
      projection.state === 'constraints_passed' && projection.price_class !== 'admitted') throw new Error('unknown product projection')
    const answerGate = projection?.answer_quality
    if (answerGate !== undefined && (!answerGate || answerGate.version !== 1 || typeof answerGate.required !== 'boolean' ||
      !['not_required', 'pass', 'fail', 'unknown'].includes(answerGate.state) || answerGate.required === (answerGate.state === 'not_required') ||
      answerGate.required && answerGate.state !== 'unknown' && (!quality || quality.status !== answerGate.state))) throw new Error('unknown answer quality gate')
    const availabilityGate = projection?.availability
    if (availabilityGate !== undefined && (!availabilityGate || availabilityGate.version !== 1 || typeof availabilityGate.required !== 'boolean' ||
      !['not_required', 'pass', 'fail', 'unknown'].includes(availabilityGate.state) || availabilityGate.required === (availabilityGate.state === 'not_required') ||
      availabilityGate.required && availabilityGate.state !== 'unknown' && (!availability || availability.status !== availabilityGate.state))) throw new Error('unknown availability gate')
    if (projection && (projection.price_valid_until !== undefined || [3, 4].includes(projection.evaluated_gates?.version ?? 0)) && !validPriceSnapshot(projection, next.observed_at)) throw new Error('unknown price deadline')
    if (!validEvaluatedGates(next.results[0]!, next.observed_at)) throw new Error('inconsistent evaluated product gates')
    if (alive && generation === id) result.value = next
  } catch { if (alive && generation === id) error.value = 'previewFailed' }
  finally { if (alive) busy.value = false }
}
function validCandidate(product: NonNullable<ProcurementPreview['results'][number]['product_procurement']>, requested: boolean): boolean {
  const candidate = product.candidate
  if (!requested) return candidate === undefined
  if (!candidate || Object.keys(candidate).sort().join(',') !== 'in_product,source_state' || typeof candidate.in_product !== 'boolean' ||
    !['pass', 'fail', 'unknown'].includes(candidate.source_state) || product.state === 'not_associated') return false
  if (candidate.source_state === 'unknown') return ['not_evaluated', 'disabled', 'product_inactive'].includes(product.state)
  return candidate.source_state !== 'fail' || product.state !== 'constraints_passed'
}
function candidateSourceState(candidate: { source_state: string }): string {
  return result.value && now.value.getTime() - Date.parse(result.value.observed_at) < 30000 ? candidate.source_state : 'unknown'
}
function knownState(value: string) { return ['classified','unclassified','scope_unconfirmed','rate_unavailable','account_unavailable','not_configured'].includes(value) ? value : 'unknown' }
function evidenceExpired(evidence?: { valid_until: string | null }) {
  return Boolean(evidence?.valid_until && Date.parse(evidence.valid_until) <= now.value.getTime())
}
function evidenceState(evidence?: { status: string; valid_until: string | null }) {
  return evidence && !evidenceExpired(evidence) ? evidence.status : 'unknown'
}
function productEvidenceState(gate?: { required: boolean; state: string }, evidence?: { valid_until: string | null }) {
  return gate?.required && evidenceExpired(evidence) ? 'unknown' : gate?.state ?? 'unknown'
}
function evaluatedGatesState(item: ProcurementPreview['results'][number]) {
  const gates = item.product_procurement?.evaluated_gates
  return gates && [3, 4].includes(gates.version) && !evidenceExpired(gates) ? gates.state : 'unknown'
}
function gateBadgeClass(state: string) {
  return state === 'pass' ? 'bg-green-100 text-green-900 dark:bg-green-900/30 dark:text-green-200' : state === 'fail' ? 'bg-red-100 text-red-900 dark:bg-red-900/30 dark:text-red-200' : 'bg-amber-100 text-amber-900 dark:bg-amber-900/30 dark:text-amber-200'
}
type ProductPreview = NonNullable<ProcurementPreview['results'][number]['product_procurement']>
function priceGateState(product: ProductPreview) {
  return product.state === 'constraints_passed' ? 'pass' : ['product_inactive', 'not_associated'].includes(product.state) || product.state === 'blocked' && ['rejected_threshold', 'rejected_procurement_range', 'rejected_procurement_source', 'rejected_procurement_bucket', 'rejected_account_sale_rate_mismatch'].includes(product.price_class) ? 'fail' : 'unknown'
}
function validPriceSnapshot(product: ProductPreview, observedAt: string): boolean {
  const until = product.price_valid_until
  if (priceGateState(product) === 'unknown') return until === null
  return typeof until === 'string' && Number.isFinite(Date.parse(until)) &&
    Date.parse(until) > Date.parse(observedAt) && Date.parse(until) <= Date.parse(observedAt) + 30000
}
function productPriceState(item: ProcurementPreview['results'][number]) {
  const product = item.product_procurement
  if (!product || ![3, 4].includes(product.evaluated_gates?.version ?? 0)) return 'unknown'
  if (['disabled', 'not_evaluated'].includes(product.state)) return product.state
  if (!product.price_valid_until) return 'unknown'
  return evidenceExpired({ valid_until: product.price_valid_until }) ? 'expired' : product.state
}
function accountControlsState(item: ProcurementPreview['results'][number]) {
  const controls = item.product_procurement?.account_controls
  return controls && !evidenceExpired(controls) ? controls.state : 'unknown'
}
function accountControlsReason(item: ProcurementPreview['results'][number]) {
  const controls = item.product_procurement?.account_controls
  return controls ? evidenceExpired(controls) ? 'expired' : controls.reason : 'not_checked'
}
function validAccountControls(controls: NonNullable<ProcurementPreview['results'][number]['product_procurement']>['account_controls'], observedAt: string): boolean {
  if (!controls || controls.version !== 1 || !['pass', 'fail', 'unknown'].includes(controls.state) ||
    !Object.prototype.hasOwnProperty.call(qualityMessages.procurement.accountControlsReasons, controls.reason) ||
    !Number.isFinite(Date.parse(controls.evaluated_at)) || Date.parse(controls.evaluated_at) !== Date.parse(observedAt) || Date.parse(observedAt) > Date.now() + 60000) return false
  if (controls.state === 'unknown') return ['account_unavailable', 'account_state_unconfirmed'].includes(controls.reason) && controls.valid_until === null
  if (typeof controls.valid_until !== 'string' || !Number.isFinite(Date.parse(controls.valid_until)) ||
    Date.parse(controls.valid_until) <= Date.parse(observedAt) || Date.parse(controls.valid_until) > Date.parse(observedAt) + 30000) return false
  return controls.state === 'pass' ? controls.reason === 'account_controls_passed' :
    ['account_inactive', 'scheduling_disabled', 'performance_paused', 'account_expired', 'account_overloaded', 'account_rate_limited', 'account_cooldown', 'account_quota_exceeded'].includes(controls.reason)
}
function validEvaluatedGates(item: ProcurementPreview['results'][number], observedAt: string): boolean {
  const product = item.product_procurement, gates = product?.evaluated_gates
  if (gates === undefined) return true // Legacy responses grant no aggregate pass.
  if (!gates || ![1, 2, 3, 4].includes(gates.version) || !product?.answer_quality || !product.availability ||
    !['pass', 'fail', 'unknown'].includes(gates.state) || Date.parse(gates.evaluated_at) !== Date.parse(observedAt)) return false
  const price = priceGateState(product)
  const checks: ProcurementEvaluatedGate[] = [{ key: 'price', state: price, reason: `price_constraints_${price === 'pass' ? 'passed' : price === 'fail' ? 'failed' : 'unavailable'}`, valid_until: gates.version >= 3 ? product.price_valid_until ?? null : null }]
  if (gates.version >= 3 ? !validPriceSnapshot(product, observedAt) : product.price_valid_until !== undefined) return false
  const controls = product.account_controls
  if (gates.version >= 2) {
    if (!validAccountControls(controls, observedAt)) return false
    checks.push({ key: 'account_controls', state: controls!.state, reason: `account_controls_${controls!.state === 'pass' ? 'passed' : controls!.state === 'fail' ? 'failed' : 'unavailable'}`, valid_until: controls!.valid_until })
  } else if (controls !== undefined) return false // Do not mix contracts or infer the missing check.
  for (const [gate, evidence, name] of [
    [product.answer_quality, item.quality_evidence, 'answer_quality'], [product.availability, item.availability_evidence, 'availability']
  ] as const) {
    if (gate.required) checks.push({ key: name, state: gate.state, reason: `${name}_${gate.state === 'pass' ? 'passed' : gate.state === 'fail' ? 'failed' : 'unavailable'}`, valid_until: ['pass', 'fail'].includes(gate.state) ? evidence?.valid_until ?? null : null })
  }
  if (gates.version === 4) {
    if (!Array.isArray(gates.gates) || gates.gates.length !== checks.length || gates.gates.some((gate, i) => {
      const expected = checks[i]!
      return !gate || Object.keys(gate).sort().join(',') !== 'key,reason,state,valid_until' || gate.key !== expected.key || gate.state !== expected.state || gate.reason !== expected.reason ||
        (expected.valid_until === null ? gate.valid_until !== null : typeof gate.valid_until !== 'string' || Date.parse(gate.valid_until) !== Date.parse(expected.valid_until))
    })) return false
  } else if (gates.gates !== undefined) return false
  // Validate the server's complete explanation, never construct missing v4
  // gates or replace its expired decision with a partial client-side verdict.
  const assessed = gates.version === 4 ? gates.gates! : checks
  const veto = assessed.find(gate => gate.state === 'fail') ?? assessed.find(gate => gate.state !== 'pass')
  const deadlines = assessed.flatMap(gate => gate.valid_until === null ? [] : [Date.parse(gate.valid_until)])
  return gates.state === (veto?.state ?? 'pass') && gates.reason === (veto?.reason ?? 'all_evaluated_gates_passed') &&
    gates.required_gates === assessed.length && gates.passed_gates === assessed.filter(gate => gate.state === 'pass').length &&
    (deadlines.length ? typeof gates.valid_until === 'string' && Date.parse(gates.valid_until) === Math.min(...deadlines) : gates.valid_until === null)
}
function validAvailabilityEvidence(e: AvailabilityScopeEvidence, observedAt: string): boolean {
  if (!e || e.version !== 1 || !['pass', 'fail', 'unknown', 'not_configured'].includes(e.status) ||
    !Object.prototype.hasOwnProperty.call(qualityMessages.procurement.availabilityReasons, e.reason) || e.reason === 'expired' ||
    !Number.isFinite(Date.parse(e.evaluated_at)) || Date.parse(e.evaluated_at) !== Date.parse(observedAt) || Date.parse(e.evaluated_at) > Date.now() + 60000 ||
    !Number.isSafeInteger(e.rule_count) || e.rule_count < 0 || !Number.isSafeInteger(e.confirmed_rules) || e.confirmed_rules < 0 || e.confirmed_rules > e.rule_count ||
    e.valid_until !== null && (typeof e.valid_until !== 'string' || !Number.isFinite(Date.parse(e.valid_until)))) return false
  if (e.status === 'pass') return e.reason === 'all_rules_passed' && e.rule_count > 0 && e.confirmed_rules === e.rule_count && e.valid_until !== null && Date.parse(e.valid_until) > Date.parse(e.evaluated_at)
  if (e.status === 'fail') return e.reason === 'availability_not_confirmed' && e.rule_count > e.confirmed_rules && e.valid_until !== null && Date.parse(e.valid_until) > Date.parse(e.evaluated_at)
  if (e.status === 'not_configured') return e.reason === 'availability_not_configured' && e.rule_count === 0 && e.confirmed_rules === 0 && e.valid_until === null
  return !['all_rules_passed', 'availability_not_configured'].includes(e.reason) && (e.rule_count === 0 || e.confirmed_rules < e.rule_count)
}
function validQualityEvidence(q: QualityScopeEvidence, observedAt: string): boolean {
  const count = (n: number, max = 4096) => Number.isSafeInteger(n) && n >= 0 && n <= max
  const knownReason = (reason: string) => typeof reason === 'string' && Object.prototype.hasOwnProperty.call(qualityMessages.procurement.qualityReasons, reason)
  const knownStatus = (status: string) => ['pass', 'fail', 'unknown', 'not_configured'].includes(status)
  if (!q || q.version !== 1 || q.model_identity !== 'unknown' || !knownStatus(q.status) || !knownReason(q.reason) ||
    typeof q.evaluated_at !== 'string' || !Number.isFinite(Date.parse(q.evaluated_at)) || Date.parse(q.evaluated_at) !== Date.parse(observedAt) || Date.parse(q.evaluated_at) > Date.now() + 60000 ||
    !count(q.rule_count, 512) || !count(q.confirmed_rules, q.rule_count) || !count(q.other_scope_rules, 512) ||
    q.rule_count + q.other_scope_rules > 512 || !count(q.samples) || !count(q.valid_samples, q.samples) || !Array.isArray(q.rules) || q.rules.length !== q.rule_count) return false
  const window = (valid: number, until: string | null) => valid === 0 ? until === null : typeof until === 'string' && Number.isFinite(Date.parse(until)) && Date.parse(until) > Date.parse(q.evaluated_at)
  const ids = new Set<number>()
  for (const rule of q.rules) {
    if (!rule || !Number.isSafeInteger(rule.plan_id) || rule.plan_id < 1 || ids.has(rule.plan_id) || !['pass', 'fail', 'unknown'].includes(rule.status) || !knownReason(rule.reason) ||
      !count(rule.samples) || !count(rule.valid_samples, rule.samples) || !window(rule.valid_samples, rule.valid_until) ||
      rule.status === 'unknown' && (['all_rules_passed', 'all_samples_passed', 'answer_incorrect', 'quality_not_configured'].includes(rule.reason) || rule.samples > 0 && rule.valid_samples === rule.samples) ||
      rule.status === 'pass' && (rule.samples === 0 || rule.valid_samples !== rule.samples || rule.reason !== 'all_samples_passed') ||
      rule.status === 'fail' && (rule.valid_samples === 0 || rule.reason !== 'answer_incorrect')) return false
    ids.add(rule.plan_id)
  }
  const passes = q.rules.filter(r => r.status === 'pass').length, failed = q.rules.some(r => r.status === 'fail')
  if (q.confirmed_rules !== passes || q.samples !== q.rules.reduce((sum, r) => sum + r.samples, 0) || q.valid_samples !== q.rules.reduce((sum, r) => sum + r.valid_samples, 0) || !window(q.valid_samples, q.valid_until)) return false
  const deadlines = q.rules.flatMap(r => r.valid_until === null ? [] : [Date.parse(r.valid_until)])
  if (deadlines.length && Date.parse(q.valid_until!) !== Math.min(...deadlines)) return false
  if (q.status === 'pass') return q.rule_count > 0 && passes === q.rule_count && !failed && q.reason === 'all_rules_passed'
  if (q.status === 'fail') return failed && q.reason === 'answer_incorrect'
  if (q.status === 'not_configured') return q.rule_count === 0 && q.other_scope_rules === 0 && q.reason === 'quality_not_configured'
  return !failed && (q.rule_count === 0 || passes < q.rule_count) && ['evidence_not_observed', 'account_unavailable', 'coverage_limit', 'scope_unconfirmed', 'scope_unobserved', 'invalid_rule', 'invalid_sample', 'rule_unconfirmed'].includes(q.reason)
}
onBeforeUnmount(() => { alive = false; controller.abort() })
</script>
