<template>
  <div class="space-y-4 p-6">
    <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    <p v-if="notice" role="status" class="text-sm text-green-600">{{ notice }}</p>
    <fieldset v-if="loaded" :disabled="saving || loading" class="space-y-4">
      <div class="flex items-center justify-between gap-4">
        <label id="apikey-health-enabled-label" class="font-medium text-gray-900 dark:text-white">{{ t('admin.settings.openaiAPIKeyHealthBreaker.enabled') }}</label>
        <Toggle v-model="form.enabled" aria-labelledby="apikey-health-enabled-label" data-testid="apikey-health-enabled" />
      </div>
      <div class="grid gap-4 sm:grid-cols-3">
        <div v-for="field in fields" :key="field.key">
          <label :for="`apikey-health-${field.key}`" class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t(`admin.settings.openaiAPIKeyHealthBreaker.${field.key}`) }}</label>
          <input :id="`apikey-health-${field.key}`" v-model.number="form[field.key]" type="number" min="1" :max="field.max" step="1" class="input w-full" />
        </div>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.openaiAPIKeyHealthBreaker.scopeHint') }}</p>
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.openaiAPIKeyHealthBreaker.recoveryHint') }}</p>
      <div class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700">
        <button type="button" class="btn btn-primary btn-sm" data-testid="apikey-health-save" @click="save">{{ saving ? t('common.saving') : t('common.save') }}</button>
      </div>
    </fieldset>
    <button v-else-if="!loading" type="button" class="btn btn-secondary btn-sm" data-testid="apikey-health-reload" @click="load">{{ t('common.refresh') }}</button>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import Toggle from '@/components/common/Toggle.vue'

const { t } = useI18n()
const form = reactive({ enabled: false, window_minutes: 2, failure_threshold: 10, cooldown_minutes: 5 })
const fields = [
  { key: 'window_minutes', max: 60 },
  { key: 'failure_threshold', max: 10000 },
  { key: 'cooldown_minutes', max: 60 },
] as const
const loading = ref(false)
const loaded = ref(false)
const saving = ref(false)
const error = ref('')
const notice = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    Object.assign(form, await adminAPI.settings.getOpenAIAPIKeyHealthBreakerSettings())
    loaded.value = true
  } catch {
    error.value = t('admin.settings.openaiAPIKeyHealthBreaker.loadFailed')
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!loaded.value || saving.value || loading.value) return
  notice.value = ''
  if (fields.some(({ key, max }) => !Number.isInteger(form[key]) || form[key] < 1 || form[key] > max)) {
    error.value = t('admin.settings.openaiAPIKeyHealthBreaker.invalid')
    return
  }
  saving.value = true
  error.value = ''
  try {
    Object.assign(form, await adminAPI.settings.updateOpenAIAPIKeyHealthBreakerSettings({ ...form }))
    notice.value = t('admin.settings.openaiAPIKeyHealthBreaker.saved')
  } catch {
    error.value = t('admin.settings.openaiAPIKeyHealthBreaker.saveFailed')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>
