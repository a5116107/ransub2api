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
      <p v-else-if="latestError">{{ t(`admin.groups.profitControl.procurement.${latestError}`) }}</p>
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
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { GroupProcurementPolicy } from '@/types'
import { validateProcurementPolicy } from '@/utils/procurementPolicy'

const props = defineProps<{
  modelValue: GroupProcurementPolicy | null
  idPrefix: string
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
const error = computed(() => validateProcurementPolicy(props.modelValue))
const unsupported = computed(() => error.value === 'unsupported')
const latestError = computed(() => props.latestPolicy === undefined ? null : validateProcurementPolicy(props.latestPolicy))

function update(field: 'min_rate' | 'max_rate', value: string) {
  if (unsupported.value) return
  const next: GroupProcurementPolicy = {
    version: 1, revision: props.modelValue?.revision ?? 0,
    min_rate: props.modelValue?.min_rate ?? null, max_rate: props.modelValue?.max_rate ?? null,
    [field]: value.trim() || null
  }
  emit('update:modelValue', next.revision === 0 && next.min_rate === null && next.max_rate === null ? null : next)
}
</script>
