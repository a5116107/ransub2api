<template>
  <section class="space-y-4 border-t border-gray-100 pt-5 dark:border-dark-700" data-testid="monitoring-probe-budget">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h3 class="font-semibold text-gray-900 dark:text-white">{{ t(`${prefix}.title`) }}</h3>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading || saving" data-testid="probe-budget-refresh" @click="load">{{ t('common.refresh') }}</button>
    </div>
    <p class="text-sm text-gray-600 dark:text-gray-300">{{ t(`${prefix}.scopeHint`) }}</p>
    <p v-if="loading" class="text-sm text-gray-500" role="status">{{ t('common.loading') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ error }}</p>
    <p v-if="notice" role="status" class="text-sm text-emerald-700 dark:text-emerald-300">{{ notice }}</p>
    <template v-if="summary">
      <p class="text-sm font-medium text-gray-800 dark:text-gray-200" data-testid="probe-budget-mode">{{ t(`${prefix}.${summary.settings.enabled ? 'enforced' : 'observeOnly'}`) }}</p>
      <div class="grid gap-4 sm:grid-cols-3" data-testid="probe-budget-usage">
        <div v-for="counter in counters" :key="counter.label" class="min-w-0">
          <p class="text-sm text-gray-700 dark:text-gray-200">{{ t(`${prefix}.${counter.label}`) }}</p>
          <p class="font-mono text-sm text-gray-900 dark:text-white">{{ counter.used }} / {{ counter.limit }}</p>
          <progress class="h-2 w-full accent-blue-600" :aria-label="t(`${prefix}.${counter.label}`)" :value="counter.used" :max="counter.limit" />
        </div>
      </div>
      <p class="text-xs text-gray-600 dark:text-gray-300">{{ t(`${prefix}.observedAt`, { value: summary.observed_at }) }}</p>
    </template>
    <fieldset v-if="loaded" :disabled="loading || saving" class="space-y-4">
      <div class="flex items-center justify-between gap-4">
        <label id="probe-budget-enabled-label" class="font-medium text-gray-900 dark:text-white">{{ t(`${prefix}.enabled`) }}</label>
        <Toggle v-model="form.enabled" aria-labelledby="probe-budget-enabled-label" data-testid="probe-budget-enabled" />
      </div>
      <div class="grid gap-4 sm:grid-cols-2">
        <div v-for="field in fields" :key="field.key" class="min-w-0">
          <label :for="`probe-budget-${field.key}`" class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t(`${prefix}.${field.key}`) }}</label>
          <input :id="`probe-budget-${field.key}`" v-model.number="form[field.key]" type="number" :min="field.min" :max="field.max" step="1" class="input w-full" />
        </div>
      </div>
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ t(`${prefix}.reserveHint`) }}</p>
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ t(`${prefix}.deferralHint`) }}</p>
      <div class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700">
        <button type="button" class="btn btn-primary btn-sm" data-testid="probe-budget-save" @click="save">{{ saving ? t('common.saving') : t('common.save') }}</button>
      </div>
    </fieldset>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import Toggle from '@/components/common/Toggle.vue'
import type { MonitoringProbeBudgetSummary } from '@/types/monitoringProbeBudget'

const { t } = useI18n()
const prefix = 'admin.settings.upstreamBillingProbe.budget'
const form = reactive({ enabled: false, revision: 0, requests_per_minute: 600, requests_per_day: 100000,
  site_requests_per_minute: 600, key_requests_per_minute: 200, concurrency: 16,
  site_concurrency: 4, key_concurrency: 2, recovery_reserve_percent: 20 })
const fields = [
  { key: 'requests_per_minute', min: 1, max: 1000000 },
  { key: 'requests_per_day', min: 1, max: 1000000000 },
  { key: 'site_requests_per_minute', min: 1, max: 1000000 },
  { key: 'key_requests_per_minute', min: 1, max: 1000000 },
  { key: 'concurrency', min: 1, max: 1024 },
  { key: 'site_concurrency', min: 1, max: 1024 },
  { key: 'key_concurrency', min: 1, max: 1024 },
  { key: 'recovery_reserve_percent', min: 0, max: 80 },
] as const
const summary = ref<MonitoringProbeBudgetSummary | null>(null)
const loading = ref(false)
const saving = ref(false)
const loaded = ref(false)
const error = ref('')
const notice = ref('')
const counters = computed(() => summary.value ? [
  { label: 'minuteUsage', used: summary.value.minute_used, limit: summary.value.settings.requests_per_minute },
  { label: 'dayUsage', used: summary.value.day_used, limit: summary.value.settings.requests_per_day },
  { label: 'activeUsage', used: summary.value.active_requests, limit: summary.value.settings.concurrency },
] : [])

async function load() {
  if (loading.value || saving.value) return
  loading.value = true
  error.value = notice.value = ''
  try {
    summary.value = await adminAPI.accounts.getMonitoringProbeBudget()
    Object.assign(form, summary.value.settings)
    loaded.value = true
  } catch {
    error.value = t(`${prefix}.loadFailed`)
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!loaded.value || loading.value || saving.value) return
  notice.value = ''
  if (fields.some(({ key, min, max }) => !Number.isInteger(form[key]) || form[key] < min || form[key] > max)) {
    error.value = t(`${prefix}.invalid`)
    return
  }
  saving.value = true
  error.value = ''
  try {
    Object.assign(form, await adminAPI.accounts.updateMonitoringProbeBudget({ ...form }))
    notice.value = t(`${prefix}.saved`)
    // A failed read must not turn a completed policy write into a failed save.
    try { summary.value = await adminAPI.accounts.getMonitoringProbeBudget() }
    catch { error.value = t(`${prefix}.summaryFailed`) }
  } catch (cause: unknown) {
    const status = (cause as { status?: number })?.status
    error.value = t(`${prefix}.${status === 409 ? 'conflict' : 'saveFailed'}`)
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>
