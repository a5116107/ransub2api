import { mount, flushPromises, enableAutoUnmount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import MonitoringProbeBudgetSettings from './MonitoringProbeBudgetSettings.vue'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

enableAutoUnmount(afterEach)
const api = vi.hoisted(() => ({ getMonitoringProbeBudget: vi.fn(), updateMonitoringProbeBudget: vi.fn(), previewMonitoringProbeCoverage: vi.fn() }))
vi.mock('@/api', () => ({ adminAPI: { accounts: api } }))
const settings = () => ({ enabled: false, revision: 3, requests_per_minute: 600, requests_per_day: 100000,
  tokens_per_minute: 0, tokens_per_day: 0, site_tokens_per_minute: 0, key_tokens_per_minute: 0,
  site_requests_per_minute: 600, key_requests_per_minute: 200, concurrency: 16,
  site_concurrency: 4, key_concurrency: 2, recovery_reserve_percent: 20 })
const summary = () => ({ observed_at: '2026-11-17T12:00:00Z', settings: settings(), minute_used: 17,
  minute_regular: 10, day_used: 123, day_regular: 100, active_requests: 2, active_regular: 1,
  minute_tokens: 17, day_tokens: 1234, usage_reported: 8, usage_unknown: 3, usage_pending: 2 })
const coverage = () => ({ observed_at: '2026-11-18T12:00:00Z', budget_revision: 3, billing_enabled: true,
  interval_minutes: 5, accounts: 100, new_api_accounts: 20, unresolved_accounts: 2, inventory_complete: true,
  minimum_requests_per_sweep: 240, minimum_requests_per_day: 69120, minimum_sweep_batches: 5,
  runner_accounts_per_minute: 20, regular_minute_capacity: 480, regular_day_capacity: 80000,
  enforced: false, can_save: true, blocking_reasons: [] as string[], warnings: ['runner_no_headroom'],
  schedule_window_start: '2026-11-19T00:00:00Z', schedule_window_end: '2026-11-20T00:00:00Z',
  account_plans: 1, group_plans: 1, unresolved_plans: 0, plan_inventory_complete: true,
  scheduled_runs: 48, scheduled_samples: 192, minimum_judge_requests: 144,
  minimum_scheduled_requests: 336, minimum_total_requests_per_day: 69456 })

beforeEach(() => {
  vi.resetAllMocks()
  api.getMonitoringProbeBudget.mockResolvedValue(summary())
  api.previewMonitoringProbeCoverage.mockResolvedValue(coverage())
  api.updateMonitoringProbeBudget.mockImplementation(async value => ({ ...value, revision: value.revision + 1 }))
})
async function open(locale = 'en') {
  const wrapper = mount(MonitoringProbeBudgetSettings, { global: { plugins: [createI18n({ legacy: false, locale, messages: { en, zh } })] } })
  await flushPromises()
  return wrapper
}

describe('Shared physical probe budgets', () => {
  it.each(['en', 'zh'])('shows candy demand separately when billing and saved plans are off in %s', async locale => {
    const wrapper = await open(locale)
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), billing_enabled: false,
      account_plans: 0, group_plans: 0, candy_probes: 1, unresolved_candy_probes: 0,
      candy_inventory_complete: true, minimum_candy_requests: 1440, minimum_total_requests_per_day: 1440 })
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="probe-budget-candy-capacity"]').text()).toContain('1440')
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '仍不是全覆盖保证' : 'still unverified')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toBe('1440 / 80000')
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), billing_enabled: false,
      account_plans: 0, group_plans: 0, candy_probes: 0, unresolved_candy_probes: 0,
      candy_inventory_complete: true, minimum_candy_requests: 0, minimum_total_requests_per_day: 0 })
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '均未启用' : 'inactive')
  })
  it.each(['en', 'zh'])('keeps missing candy coverage unknown and localizes warnings in %s', async locale => {
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), billing_enabled: false, account_plans: 0, group_plans: 0,
      warnings: ['candy_unresolved', 'candy_inventory_truncated'] })
    const wrapper = await open(locale)
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="probe-budget-candy-capacity"]').text()).toContain(locale === 'zh' ? '未知' : 'Unknown')
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '仍不是全覆盖保证' : 'still unverified')
    expect(wrapper.get('[data-testid="probe-budget-capacity-reasons"]').text()).not.toContain('candy_unresolved')
    expect(wrapper.get('[data-testid="probe-budget-scheduled-capacity"]').text()).toContain(locale === 'zh' ? '旧服务' : 'older server')
  })
  it.each(['en', 'zh'])('separates scheduled samples, grading and combined capacity in %s', async locale => {
    const wrapper = await open(locale)
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), billing_enabled: false,
      minimum_requests_per_day: 0, minimum_total_requests_per_day: 336, enforced: true,
      can_save: false, blocking_reasons: ['day_capacity'] })
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    const panel = wrapper.get('[data-testid="probe-budget-scheduled-capacity"]')
    for (const value of ['192', '144', '336', '2026-11-19T00:00:00Z']) expect(panel.text()).toContain(value)
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '明确超限' : 'shortfall')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toBe('336 / 80000')
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
  })
  it('keeps missing legacy schedule data unknown instead of displaying zero', async () => {
    const old = { ...coverage(), account_plans: undefined, group_plans: undefined,
      scheduled_samples: undefined, minimum_judge_requests: undefined, minimum_scheduled_requests: undefined,
      minimum_total_requests_per_day: undefined, plan_inventory_complete: undefined, schedule_window_start: 'bad-date' }
    api.previewMonitoringProbeCoverage.mockResolvedValue(old)
    const wrapper = await open()
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="probe-budget-scheduled-capacity"]').text()).toContain('older server')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toContain('Unknown')
    expect(wrapper.get('[data-testid="probe-budget-scheduled-capacity"]').text()).not.toContain('bad-date')
  })
  it.each(['en', 'zh'])('previews lower bounds, invalidates on edits and blocks an impossible save in %s', async locale => {
    const wrapper = await open(locale)
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="probe-budget-capacity"]').text()).toContain('≥ 240')
    expect(wrapper.get('[data-testid="probe-budget-capacity-reasons"]').text()).toContain(locale === 'zh' ? '没有失败' : 'no room for failures')
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '仍不是全覆盖保证' : 'still unverified')
    await wrapper.get('#probe-budget-requests_per_day').setValue('100')
    expect(wrapper.find('[data-testid="probe-budget-capacity-stale"]').exists()).toBe(true)
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), enforced: true, can_save: false, blocking_reasons: ['day_capacity'] })
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.previewMonitoringProbeCoverage).toHaveBeenCalledTimes(2)
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain(locale === 'zh' ? '未保存' : 'Nothing saved')
    expect(wrapper.get<HTMLInputElement>('#probe-budget-requests_per_day').element.value).toBe('100')
  })
  it('does not save on a failed preview, and permits an explicit observe-only preview', async () => {
    const wrapper = await open()
    api.previewMonitoringProbeCoverage.mockRejectedValueOnce(new Error('storage offline'))
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
    api.previewMonitoringProbeCoverage.mockResolvedValueOnce({ ...coverage(), blocking_reasons: ['runner_capacity'], can_save: true })
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).toHaveBeenCalledTimes(1)
  })
  it('preserves drafts on a stale preview revision and labels bounded/unknown results', async () => {
    const wrapper = await open()
    api.previewMonitoringProbeCoverage.mockRejectedValueOnce({ status: 409 })
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('Another administrator')
    api.previewMonitoringProbeCoverage.mockResolvedValueOnce({ ...coverage(), inventory_complete: false, warnings: ['future-reason'] })
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="probe-budget-capacity"]').text()).toContain('remaining targets are not evaluated')
    expect(wrapper.get('[data-testid="probe-budget-capacity-reasons"]').text()).toContain('unrecognized')
  })
  it('invalidates the earlier preview when backend inventory revalidation rejects a save', async () => {
    const wrapper = await open()
    await wrapper.get('#probe-budget-requests_per_day').setValue('800')
    api.updateMonitoringProbeBudget.mockRejectedValueOnce({ status: 400, reason: 'MONITORING_PROBE_COVERAGE_IMPOSSIBLE' })
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('Nothing saved')
    expect(wrapper.find('[data-testid="probe-budget-capacity-stale"]').exists()).toBe(true)
    expect(wrapper.get<HTMLInputElement>('#probe-budget-requests_per_day').element.value).toBe('800')
    expect(api.getMonitoringProbeBudget).toHaveBeenCalledTimes(1)
  })
  it.each(['en', 'zh'])('shows actual usage, unknown coverage and overshoot separately in %s', async locale => {
    api.getMonitoringProbeBudget.mockResolvedValue({ ...summary(), settings: { ...settings(), tokens_per_day: 1000 } })
    const wrapper = await open(locale)
    expect(wrapper.get('[data-testid="probe-budget-tokens"]').text()).toContain('1234 / 1000')
    expect(wrapper.get('[data-testid="probe-budget-coverage"]').text()).toContain(locale === 'zh' ? '用量未知/不完整 3 次' : '3 unknown/partial usage')
    expect(wrapper.get('[data-testid="probe-budget-coverage"]').text()).toContain(locale === 'zh' ? '待结算 2 次' : '2 unsettled')
    await wrapper.get('#probe-budget-tokens_per_day').setValue('2000')
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).toHaveBeenCalledWith({ ...settings(), tokens_per_day: 2000 })
    expect(api.getMonitoringProbeBudget).toHaveBeenCalledTimes(2)
  })
  it('does not display missing legacy token fields as observed zero', async () => {
    const legacy = summary() as Partial<ReturnType<typeof summary>>
    delete legacy.day_tokens
    delete legacy.usage_unknown
    api.getMonitoringProbeBudget.mockResolvedValue(legacy)
    const wrapper = await open()
    expect(wrapper.get('[data-testid="probe-budget-tokens"]').text()).toContain('Unknown / Unlimited')
    expect(wrapper.get('[data-testid="probe-budget-coverage"]').text()).toContain('Unknown unknown/partial usage')
  })
  it.each(['-1', '1.5', '1000000000001', ''])('rejects invalid token limit %s', async value => {
    const wrapper = await open()
    await wrapper.get('#probe-budget-tokens_per_day').setValue(value)
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
  })
  it.each(['en', 'zh'])('reads counters and saves a revisioned policy in %s', async locale => {
    const wrapper = await open(locale)
    expect(wrapper.get('[data-testid="probe-budget-usage"]').text()).toContain('17 / 600')
    expect(wrapper.findAll('progress')).toHaveLength(3)
    await wrapper.get('#probe-budget-requests_per_minute').setValue('90')
    await wrapper.get('[role="switch"]').trigger('click')
    expect(wrapper.get('[data-testid="probe-budget-mode"]').text()).toContain(locale === 'zh' ? '仅观测' : 'Observe only')
    api.getMonitoringProbeBudget.mockResolvedValue({ ...summary(), settings: { ...settings(), enabled: true, requests_per_minute: 90, revision: 4 } })
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).toHaveBeenCalledWith({ ...settings(), requests_per_minute: 90, enabled: true })
    expect(wrapper.get('[data-testid="probe-budget-mode"]').text()).toContain(locale === 'zh' ? '限额已生效' : 'Limits enforced')
    expect(wrapper.get('[role="status"]').text()).toContain(locale === 'zh' ? '已保存' : 'saved')
    expect(wrapper.get('[data-testid="probe-budget-usage"]').text()).toContain('17 / 90')
  })
  it.each(['', '0', '-1', '1.5', '1000001'])('rejects invalid request limit %s', async value => {
    const wrapper = await open()
    await wrapper.get('#probe-budget-requests_per_minute').setValue(value)
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('whole numbers')
  })
  it('requires a successful read before editing and supports retry', async () => {
    api.getMonitoringProbeBudget.mockRejectedValueOnce(new Error('fixture unavailable'))
    const wrapper = await open()
    expect(wrapper.find('fieldset').exists()).toBe(false)
    expect(wrapper.get('[role="alert"]').text()).toContain('Refresh before editing')
    await wrapper.get('[data-testid="probe-budget-refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('fieldset').exists()).toBe(true)
  })
  it('retains edits and the old revision on conflict until an explicit refresh', async () => {
    const wrapper = await open()
    api.updateMonitoringProbeBudget.mockRejectedValue({ status: 409 })
    await wrapper.get('#probe-budget-key_concurrency').setValue('7')
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('Another administrator')
    expect(wrapper.get<HTMLInputElement>('#probe-budget-key_concurrency').element.value).toBe('7')
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    expect(api.getMonitoringProbeBudget).toHaveBeenCalledTimes(1)
  })
  it('reports a saved policy separately from a failed counter refresh', async () => {
    const wrapper = await open()
    api.getMonitoringProbeBudget.mockRejectedValueOnce(new Error('summary unavailable'))
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="status"]').text()).toContain('saved')
    expect(wrapper.get('[role="alert"]').text()).toContain('usage refresh failed')
  })
})
