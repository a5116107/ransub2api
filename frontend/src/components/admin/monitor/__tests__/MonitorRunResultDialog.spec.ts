import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import type { CheckResult } from '@/api/admin/channelMonitor'
import MonitorRunResultDialog from '../MonitorRunResultDialog.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

function result(overrides: Partial<CheckResult>): CheckResult {
  return { model: 'test-model', status: 'error', latency_ms: null, ping_latency_ms: null, message: '', checked_at: '2026-11-07T00:00:00Z', ...overrides }
}

function render(results: CheckResult[]) {
  return mount(MonitorRunResultDialog, {
    props: { show: true, results },
    global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, MonitorQuotaView: true } },
  })
}

describe('MonitorRunResultDialog', () => {
  it('presents local admission as skipped, without an upstream error or latency', () => {
    const wrapper = render([result({ skipped: true, message: 'Local capacity unavailable' })])
    expect(wrapper.text()).toContain('admin.channelMonitor.runResultSkipped')
    expect(wrapper.text()).toContain('admin.channelMonitor.runResultSkippedHint')
    expect(wrapper.text()).not.toContain('monitorCommon.status.error')
    expect(wrapper.text()).not.toContain(' ms')
  })

  it('retains actual upstream status and latency for observed results', () => {
    const wrapper = render([result({ status: 'operational', latency_ms: 42 })])
    expect(wrapper.text()).toContain('monitorCommon.status.operational')
    expect(wrapper.text()).toContain('42 ms')
    expect(wrapper.text()).not.toContain('admin.channelMonitor.runResultSkipped')
  })
})
