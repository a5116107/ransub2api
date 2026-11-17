import { mount, flushPromises, enableAutoUnmount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import MonitoringProbeBudgetSettings from './MonitoringProbeBudgetSettings.vue'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

enableAutoUnmount(afterEach)
const api = vi.hoisted(() => ({ getMonitoringProbeBudget: vi.fn(), updateMonitoringProbeBudget: vi.fn() }))
vi.mock('@/api', () => ({ adminAPI: { accounts: api } }))
const settings = () => ({ enabled: false, revision: 3, requests_per_minute: 600, requests_per_day: 100000,
  tokens_per_minute: 0, tokens_per_day: 0, site_tokens_per_minute: 0, key_tokens_per_minute: 0,
  site_requests_per_minute: 600, key_requests_per_minute: 200, concurrency: 16,
  site_concurrency: 4, key_concurrency: 2, recovery_reserve_percent: 20 })
const summary = () => ({ observed_at: '2026-11-17T12:00:00Z', settings: settings(), minute_used: 17,
  minute_regular: 10, day_used: 123, day_regular: 100, active_requests: 2, active_regular: 1,
  minute_tokens: 17, day_tokens: 1234, usage_reported: 8, usage_unknown: 3, usage_pending: 2 })

beforeEach(() => {
  vi.resetAllMocks()
  api.getMonitoringProbeBudget.mockResolvedValue(summary())
  api.updateMonitoringProbeBudget.mockImplementation(async value => ({ ...value, revision: value.revision + 1 }))
})
async function open(locale = 'en') {
  const wrapper = mount(MonitoringProbeBudgetSettings, { global: { plugins: [createI18n({ legacy: false, locale, messages: { en, zh } })] } })
  await flushPromises()
  return wrapper
}

describe('Shared physical probe budgets', () => {
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
