<template>
  <BaseDialog
    :show="show"
    :title="t('admin.channelMonitor.runResultTitle')"
    width="normal"
    @close="$emit('close')"
  >
    <div class="space-y-2">
      <div
        v-for="r in results"
        :key="r.model"
        class="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-gray-200 px-3 py-2 text-sm dark:border-dark-600"
      >
        <div class="flex min-w-0 flex-1 basis-40 flex-col break-words">
          <span class="font-medium text-gray-900 dark:text-white">{{ formatMonitorModel(r.model) }}</span>
          <span v-if="r.skipped" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.channelMonitor.runResultSkippedHint') }}</span>
          <span v-else-if="r.message" class="text-xs text-gray-500 dark:text-gray-400">{{ r.message }}</span>
          <MonitorQuotaView :snapshot="r.quota" class="mt-1" />
        </div>
        <div class="flex items-center gap-2">
          <span
            class="inline-flex items-center rounded-full px-2 py-0.5 text-[11px]"
            :class="statusBadgeClass(r.skipped ? 'error' : r.status)"
          >
            {{ r.skipped ? t('admin.channelMonitor.runResultSkipped') : statusLabel(r.status) }}
          </span>
          <span v-if="!r.skipped" class="text-xs text-gray-500 dark:text-gray-400">{{ formatLatency(r.latency_ms) }} ms</span>
        </div>
      </div>
    </div>
    <template #footer>
      <div class="flex justify-end">
        <button @click="$emit('close')" class="btn btn-primary">
          {{ t('common.close') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { CheckResult } from '@/api/admin/channelMonitor'
import BaseDialog from '@/components/common/BaseDialog.vue'
import MonitorQuotaView from '@/components/common/MonitorQuotaView.vue'
import { useChannelMonitorFormat } from '@/composables/useChannelMonitorFormat'

defineProps<{
  show: boolean
  results: CheckResult[]
}>()

defineEmits<{
  (e: 'close'): void
}>()

const { t } = useI18n()
const { statusLabel, statusBadgeClass, formatLatency, formatMonitorModel } = useChannelMonitorFormat()
</script>
