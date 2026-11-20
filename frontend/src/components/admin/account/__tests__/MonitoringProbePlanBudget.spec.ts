import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import MonitoringProbePlanBudget from '../MonitoringProbePlanBudget.vue'
import { getMonitoringProbeBudget, previewMonitoringProbeCoverage } from '@/api/admin/accounts'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

vi.mock('@/api/admin/accounts', () => ({ getMonitoringProbeBudget: vi.fn(), previewMonitoringProbeCoverage: vi.fn() }))
const draft = { plan_id: 0, account_id: 42, model_id: 'model-a', cron_expression: '0 * * * *', enabled: true, auto_recover: false, max_results: 50 }
const settings = { revision: 7, enabled: true }
function coverage() {
  return { observed_at: '2026-11-20T12:00:00Z', budget_revision: 7, schedule_window_start: '2026-11-21T00:00:00Z', schedule_window_end: '2026-11-22T00:00:00Z',
    enforced: true, can_save: true, regular_day_capacity: 100, minimum_total_requests_per_day: 72, inventory_complete: true, plan_inventory_complete: true,
    candy_inventory_complete: true, active_inventory_complete: true, warnings: [], blocking_reasons: [], unestimated_demand_sources: [],
    plan_change: { version: 1, plan_id: 0, account_id: 42, enabled: true, current_minimum_requests_per_day: 48, delta_minimum_requests_per_day: 24, proposed_minimum_requests_per_day: 72 } }
}
function panel(locale = 'zh') {
  return mount(MonitoringProbePlanBudget, { props: { draft: { ...draft } }, global: { plugins: [createI18n({ legacy: false, locale, messages: { en, zh } })] } })
}
beforeEach(() => { vi.mocked(getMonitoringProbeBudget).mockResolvedValue({ settings } as any); vi.mocked(previewMonitoringProbeCoverage).mockResolvedValue(coverage() as any) })
afterEach(() => vi.resetAllMocks())
describe('native account plan budget preview', () => {
  it.each(['zh', 'en'])('shows independent current/delta/proposed/capacity and a bounded conclusion in %s', async locale => {
    const w = panel(locale)
    expect(getMonitoringProbeBudget).not.toHaveBeenCalled()
    await w.get('[data-testid="plan-budget-preview"]').trigger('click'); await flushPromises()
    expect(previewMonitoringProbeCoverage).toHaveBeenCalledWith({ ...settings, plan_draft: draft }, { signal: expect.any(AbortSignal) })
    for (const [key, value] of Object.entries({ current: '48', delta: '+24', proposed: '72', capacity: '100' })) expect(w.get(`[data-testid="plan-budget-${key}"]`).text()).toBe(value)
    expect(w.get('[data-testid="plan-budget-conclusion"]').text()).toMatch(locale === 'zh' ? /不代表完整/ : /not complete/)
    w.unmount()
  })
  it.each(['partial', 'insufficient', 'notEnforced'])('separates %s from complete feasibility', async kind => {
    const data = coverage()
    if (kind === 'partial') data.unestimated_demand_sources = ['event_diagnostics'] as never[]
    if (kind === 'insufficient') { data.can_save = false; data.blocking_reasons = ['day_capacity'] as never[] }
    if (kind === 'notEnforced') data.enforced = false
    vi.mocked(previewMonitoringProbeCoverage).mockResolvedValueOnce(data as any)
    const w = panel(); await w.get('button').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="plan-budget-conclusion"]').text()).toMatch({ partial: /仍有未估算/, insufficient: /超限/, notEnforced: /未启用/ }[kind]!)
    w.unmount()
  })
  it('clears old results on a changed draft but retains them on an equivalent rerender', async () => {
    const w = panel(); await w.get('button').trigger('click'); await flushPromises()
    await w.setProps({ draft: { ...draft } })
    expect(w.find('[data-testid="plan-budget-result"]').exists()).toBe(true)
    await w.setProps({ draft: { ...draft, cron_expression: '* * * * *' } })
    expect(w.find('[data-testid="plan-budget-result"]').exists()).toBe(false)
    w.unmount()
  })
  it.each([403, 409, 503])('keeps the draft and supports recovery after %s', async status => {
    vi.mocked(previewMonitoringProbeCoverage).mockRejectedValueOnce({ response: { status } })
    const w = panel(); await w.get('button').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="plan-budget-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="plan-budget-error"]').text()).toMatch(status === 409 ? /预算已变化/ : status === 403 ? /权限/ : /未完成/)
    await w.get('button').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="plan-budget-error"]').exists()).toBe(false)
    expect(w.get('[data-testid="plan-budget-delta"]').text()).toBe('+24')
    w.unmount()
  })
  it.each(['missing', 'mismatch', 'contradiction', 'bad-window', 'bad-revision', 'null'])('does not bless %s response data', async kind => {
    const data = coverage() as any
    if (kind === 'missing') delete data.plan_change
    if (kind === 'mismatch') data.plan_change.account_id = 99
    if (kind === 'contradiction') data.plan_change.delta_minimum_requests_per_day = 0
    if (kind === 'bad-window') data.schedule_window_end = 'invalid'
    if (kind === 'bad-revision') data.budget_revision = 8
    vi.mocked(previewMonitoringProbeCoverage).mockResolvedValueOnce(kind === 'null' ? null as any : data)
    const w = panel(); await w.get('button').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="plan-budget-result"]').exists()).toBe(false)
    expect(w.find('[data-testid="plan-budget-error"]').exists()).toBe(true)
    w.unmount()
  })
  it.each(['resolve', 'reject'])('discards a late %s after input changes', async kind => {
    let resolve!: (v: any) => void, reject!: (e: any) => void
    vi.mocked(previewMonitoringProbeCoverage).mockReturnValueOnce(new Promise((yes, no) => { resolve = yes; reject = no }))
    const w = panel(); await w.get('button').trigger('click'); await flushPromises()
    expect(w.get('button').attributes('disabled')).toBeDefined()
    await w.setProps({ draft: { ...draft, model_id: 'model-b' } })
    if (kind === 'resolve') resolve(coverage()); else reject(new Error('late'))
    await flushPromises()
    expect(w.find('[data-testid="plan-budget-result"]').exists()).toBe(false)
    expect(w.find('[data-testid="plan-budget-error"]').exists()).toBe(false)
    w.unmount()
  })
  it('does not continue a request after unmount or a delayed settings read', async () => {
    let resolve!: (v: any) => void
    vi.mocked(getMonitoringProbeBudget).mockReturnValueOnce(new Promise(yes => { resolve = yes }))
    const w = panel(); await w.get('button').trigger('click'); w.unmount(); resolve({ settings }); await flushPromises()
    expect(previewMonitoringProbeCoverage).not.toHaveBeenCalled()
  })
  it('permits negative deltas for repairs and never describes them as reserved tokens', async () => {
    const data = coverage(); data.minimum_total_requests_per_day = 24; data.plan_change.proposed_minimum_requests_per_day = 24; data.plan_change.delta_minimum_requests_per_day = -24; data.plan_change.enabled = false
    vi.mocked(previewMonitoringProbeCoverage).mockResolvedValueOnce(data as any)
    const w = panel(); await w.setProps({ draft: { ...draft, enabled: false } }); await w.get('button').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="plan-budget-delta"]').text()).toBe('-24'); expect(w.text()).toContain('仍可停用原计划')
    w.unmount()
  })
})
