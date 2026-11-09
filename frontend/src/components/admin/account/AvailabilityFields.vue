<template>
  <fieldset class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
    <legend class="px-1 text-sm font-medium text-gray-700 dark:text-gray-200">{{ t('admin.accounts.pelicanTest.availabilityTitle') }}</legend>
    <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-200">
      <input type="checkbox" :checked="!!modelValue" :disabled="!supported && !modelValue" data-testid="availability-enabled" @change="toggle" />
      {{ t('admin.accounts.pelicanTest.availabilityEnable') }}
    </label>
    <p class="text-xs text-gray-600 dark:text-gray-300">{{ t('admin.accounts.pelicanTest.availabilityHelp') }}</p>
    <template v-if="modelValue">
      <p v-if="!supported" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ t('admin.accounts.pelicanTest.availabilityUnsupported') }}</p>
      <div class="grid gap-3 sm:grid-cols-2">
        <label class="block space-y-1 text-sm text-gray-700 dark:text-gray-200">
          <span>{{ t('admin.accounts.pelicanTest.availabilityValidity') }}</span>
          <input type="number" min="30" max="86400" step="1" class="input" data-testid="availability-validity" :value="modelValue.valid_for_seconds" @input="update('valid_for_seconds', $event)" />
        </label>
        <label class="block space-y-1 text-sm text-gray-700 dark:text-gray-200">
          <span>{{ t('admin.accounts.pelicanTest.availabilityBudget') }}</span>
          <input type="number" min="0" max="64" step="1" class="input" data-testid="availability-budget" :value="modelValue.max_extra_samples ?? 0" @input="update('max_extra_samples', $event)" />
        </label>
      </div>
      <p class="text-xs text-gray-600 dark:text-gray-300">{{ t('admin.accounts.pelicanTest.availabilityBudgetHelp') }}</p>
      <p v-if="!validAvailability(modelValue, supported) && supported" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ t('admin.accounts.pelicanTest.availabilityInvalid') }}</p>
    </template>
  </fieldset>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { AvailabilityPolicy } from '@/types'
import { validAvailability } from '@/utils/availability'

const props = defineProps<{ modelValue?: AvailabilityPolicy; supported: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: AvailabilityPolicy | undefined] }>()
const { t } = useI18n()

function toggle(event: Event) {
  const enabled = (event.target as HTMLInputElement).checked
  emit('update:modelValue', enabled ? { valid_for_seconds: 300, max_extra_samples: 0 } : undefined)
}

function update(key: keyof AvailabilityPolicy, event: Event) {
  if (!props.modelValue) return
  emit('update:modelValue', { ...props.modelValue, [key]: (event.target as HTMLInputElement).valueAsNumber })
}
</script>
