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

  it('explains a token budget deferral separately from the upstream status', () => {
    const wrapper = render([result({
      skipped: true,
      budget_deferral: { reason: 'token_limit', scope: 'global_day', retry_at: '2026-11-19T00:00:00Z' },
    })])
    expect(wrapper.text()).toContain('admin.settings.upstreamBillingProbe.budget.token_limit')
    expect(wrapper.text()).toContain('modelTrace.budgetScopes.global_day')
    expect(wrapper.find('time').attributes('datetime')).toBe('2026-11-19T00:00:00Z')
    expect(wrapper.text()).not.toContain('monitorCommon.status.error')
    expect(wrapper.text()).not.toContain(' ms')
  })

  it('keeps unknown reasons and invalid retry times explicitly unknown', () => {
    const wrapper = render([result({
      skipped: true,
      budget_deferral: { reason: 'future', scope: 'future', retry_at: 'invalid' },
    })])
    expect(wrapper.text()).toContain('modelTrace.budgetReasonUnknown')
    expect(wrapper.text()).toContain('modelTrace.budgetScopes.unknown')
    expect(wrapper.text()).toContain('modelTrace.retryUnknown')
    expect(wrapper.find('time').exists()).toBe(false)
  })

  it('shows a quota-only deferral without discarding the observed model', () => {
    const wrapper = render([result({
      status: 'operational', latency_ms: 42,
      quota_budget_deferral: { reason: 'request_limit', scope: 'global_day', retry_at: '2026-11-19T00:00:00Z' },
    })])
    expect(wrapper.text()).toContain('monitorCommon.status.operational')
    expect(wrapper.text()).toContain('42 ms')
    expect(wrapper.text()).toContain('admin.channelMonitor.runQuotaDeferred')
    expect(wrapper.text()).toContain('admin.settings.upstreamBillingProbe.budget.request_limit')
    expect(wrapper.text()).not.toContain('admin.channelMonitor.runResultSkippedHint')
  })

  it('does not combine a quota snapshot with a contradictory deferral', () => {
    const wrapper = render([result({
      status: 'operational',
      quota: { source: 'usage', success: true, fetched_at: '2026-11-18T00:00:00Z' },
      quota_budget_deferral: { reason: 'request_limit', scope: 'global_day', retry_at: '2026-11-19T00:00:00Z' },
    })])
    expect(wrapper.text()).not.toContain('admin.channelMonitor.runQuotaDeferred')
  })

  it('does not attribute an observed supplier error to an untrusted budget field', () => {
    const wrapper = render([result({
      status: 'error', message: 'upstream HTTP 503', latency_ms: 12,
      budget_deferral: { reason: 'token_limit', scope: 'global_day', retry_at: '2026-11-19T00:00:00Z' },
    })])
    expect(wrapper.text()).toContain('upstream HTTP 503')
    expect(wrapper.text()).toContain('12 ms')
    expect(wrapper.text()).not.toContain('admin.settings.upstreamBillingProbe.budget.token_limit')
  })
})
