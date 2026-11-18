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

function billingScopes() {
  return { version: 1, sites: 2, credential_groups: 3, new_api_authorization_groups: 1,
    max_site_requests_per_sweep: 8, max_credential_requests_per_sweep: 6,
    site_sweep_capacity: 5, credential_sweep_capacity: 5, over_limit_sites: 1,
    over_limit_credential_groups: 1, unresolved_sites: 0, unresolved_credentials: 1,
    unresolved_new_api_authorizations: 1 }
}

function periodicSites() {
  return { version: 1, scope: 'billing_and_v1', sites: 2, billing_requests_per_day: 96,
    active_model_requests_per_day: 1440, origin_ping_requests_per_day: 1440,
    max_requests_per_day: 1536, site_day_capacity: 1440, over_limit_sites: 1,
    unresolved_billing_targets: 0, unresolved_active_targets: 1 }
}

describe('periodic shared-site coverage', () => {
  it.each(['en', 'zh'])('shows cross-family contribution, anonymous site shortfalls and credential unknowns in %s', async locale => {
    api.getMonitoringProbeBudget.mockResolvedValue({ ...summary(), coverage: { ...coverage(), periodic_sites: {
      ...periodicSites(), site: 'PRIVATE_PERIODIC_SITE', key: 'PRIVATE_PERIODIC_KEY' } } })
    const wrapper = await open(locale)
    const panel = wrapper.get('[data-testid="probe-budget-periodic-sites"]')
    expect(panel.text()).toContain(locale === 'zh' ? '倍率与 V1：共享站点日下界' : 'Billing and V1: shared-site daily lower bounds')
    expect(panel.get('[data-testid="periodic-sites-verdict"]').text()).toContain(locale === 'zh' ? '已证明存在共享站点超限' : 'Proven shared-site shortfall')
    expect(panel.get('[data-testid="periodic-sites-capacity"]').text()).toContain('1536 / 1440')
    expect(panel.get('[data-testid="periodic-sites-contributions"]').text()).toContain('96')
    expect(panel.get('[data-testid="periodic-sites-contributions"]').text()).toContain('1440')
    expect(panel.text()).toContain(locale === 'zh' ? '未归桶 V1 目标 1' : '1 unbucketed V1 targets')
    expect(panel.text()).toContain(locale === 'zh' ? 'HEAD 归属根域' : 'HEAD belongs to the origin')
    expect(panel.text()).toContain(locale === 'zh' ? 'V1 最终凭据共享关系仍未编译' : 'V1 final-credential sharing remains uncompiled')
    expect(panel.text()).not.toContain('PRIVATE_')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toContain('69456')
  })

  it.each([undefined, { ...periodicSites(), version: 2 }, { ...periodicSites(), scope: 'fleet' },
    { ...periodicSites(), sites: -1 }, { ...periodicSites(), over_limit_sites: 3 },
    { ...periodicSites(), active_model_requests_per_day: 1.5 }])('keeps missing or malformed periodic scope evidence unknown: %j', async sites => {
    api.getMonitoringProbeBudget.mockResolvedValue({ ...summary(), coverage: { ...coverage(), periodic_sites: sites } })
    const wrapper = await open()
    const panel = wrapper.get('[data-testid="probe-budget-periodic-sites"]')
    expect(panel.text()).toContain('Unknown')
    expect(panel.find('[data-testid="periodic-sites-capacity"]').exists()).toBe(false)
  })

  it('blocks a combined site shortfall even with billing disabled and keeps disabling enforcement available', async () => {
    api.previewMonitoringProbeCoverage.mockImplementation(async draft => ({ ...coverage(), billing_enabled: false,
      enforced: draft.enabled, can_save: !draft.enabled,
      blocking_reasons: draft.enabled ? ['periodic_site_capacity'] : [], periodic_sites: periodicSites() }))
    const wrapper = await open()
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="probe-budget-capacity-reasons"]').text()).toContain('shared site')
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).toHaveBeenCalledOnce()
  })

  it('invalidates periodic site evidence on draft changes and preview failure', async () => {
    api.getMonitoringProbeBudget.mockResolvedValue({ ...summary(), coverage: { ...coverage(), periodic_sites: periodicSites() } })
    const wrapper = await open()
    expect(wrapper.find('[data-testid="periodic-sites-capacity"]').exists()).toBe(true)
    await wrapper.get('#probe-budget-site_requests_per_minute').setValue(1)
    expect(wrapper.find('[data-testid="probe-budget-periodic-sites"]').exists()).toBe(false)
    api.previewMonitoringProbeCoverage.mockRejectedValueOnce(new Error('unavailable'))
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="probe-budget-periodic-sites"]').exists()).toBe(false)
  })
})

describe('billing shared-scope coverage', () => {
  it.each(['en', 'zh'])('shows anonymous scope lower bounds and unknowns without changing total demand in %s', async locale => {
    api.getMonitoringProbeBudget.mockResolvedValue({ ...summary(), coverage: { ...coverage(), billing_scopes: {
      ...billingScopes(), site: 'PRIVATE_SITE', credential: 'PRIVATE_CREDENTIAL' } } })
    const wrapper = await open(locale)
    const panel = wrapper.get('[data-testid="probe-budget-billing-scopes"]')
    expect(panel.text()).toContain(locale === 'zh' ? '倍率周期：站点与共享凭据下界' : 'Billing sweep: site and shared-credential lower bounds')
    expect(panel.get('[data-testid="billing-scopes-sites"]').text()).toContain('8 / 5')
    expect(panel.get('[data-testid="billing-scopes-credentials"]').text()).toContain('6 / 5')
    expect(panel.get('[data-testid="billing-scopes-verdict"]').text()).toContain(locale === 'zh' ? '已证明存在超限' : 'Proven scope shortfall')
    expect(panel.text()).toContain(locale === 'zh' ? '管理授权组 1' : '1 management authorization groups')
    expect(panel.text()).toContain(locale === 'zh' ? '未确认管理授权 1' : '1 unresolved management authorizations')
    expect(panel.text()).toContain(locale === 'zh' ? '不得相加' : 'Do not sum')
    expect(panel.text()).not.toContain('PRIVATE_')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toContain('69456')
  })

  it.each([undefined, { ...billingScopes(), version: 2 }, { ...billingScopes(), sites: -1 }, { ...billingScopes(), credential_groups: 1.5 }, { ...billingScopes(), over_limit_sites: 3 }])('keeps legacy or malformed scope evidence unknown: %j', async scopes => {
    api.getMonitoringProbeBudget.mockResolvedValue({ ...summary(), coverage: { ...coverage(), billing_scopes: scopes } })
    const wrapper = await open()
    const panel = wrapper.get('[data-testid="probe-budget-billing-scopes"]')
    expect(panel.text()).toContain('Unknown')
    expect(panel.find('[data-testid="billing-scopes-sites"]').exists()).toBe(false)
  })

  it.each(['site_capacity', 'key_capacity'])('blocks a proven %s shortfall and keeps disabling available', async reason => {
    api.previewMonitoringProbeCoverage.mockImplementation(async draft => ({ ...coverage(), enforced: draft.enabled,
      can_save: !draft.enabled, blocking_reasons: draft.enabled ? [reason] : [], billing_scopes: billingScopes() }))
    const wrapper = await open()
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="probe-budget-capacity-reasons"]').text()).not.toContain('unrecognized')
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).toHaveBeenCalledOnce()
  })

  it('clears scope evidence after a draft change and preview failure', async () => {
    api.getMonitoringProbeBudget.mockResolvedValue({ ...summary(), coverage: { ...coverage(), billing_scopes: billingScopes() } })
    const wrapper = await open()
    expect(wrapper.get('[data-testid="billing-scopes-sites"]').text()).toContain('8 / 5')
    await wrapper.get('#probe-budget-key_requests_per_minute').setValue(1)
    expect(wrapper.find('[data-testid="probe-budget-billing-scopes"]').exists()).toBe(false)
    api.previewMonitoringProbeCoverage.mockRejectedValueOnce(new Error('unavailable'))
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="probe-budget-billing-scopes"]').exists()).toBe(false)
  })
})

function ollamaRuntime() {
  return { version: 1, scope: 'responding_process', instance_id: 'fixture-ollama', state: 'running', periodic_state: 'enabled',
    observed_at: '2026-11-19T12:00:00Z', scan_interval_seconds: 60, scan_limit: 20,
    pending_rate_limit_probes: 4, active_rate_limit_probes: 1, active_refreshes: 2,
    last_scan: { state: 'observed', observed_at: '2026-11-19T11:59:00Z', enabled: true, interval_minutes: 60, debounce_minutes: 1,
      candidates: 3 as number | null, due_groups: 2 as number | null, limit_reached: false as boolean | null }
  }
}

function responsesRuntime() {
  return { version: 1, scope: 'responding_process', instance_id: 'fixture-responses', state: 'active',
    observed_at: '2026-11-19T12:00:00Z', started_events: 5, finished_events: 3, active_events: 2,
    lifecycle_timeout_seconds: 30, http_timeout_seconds: 15,
    last_started_at: '2026-11-19T11:59:59Z', last_finished_at: '2026-11-19T11:59:58Z' }
}

describe('Shared physical probe budgets', () => {
  it.each(['en', 'zh'])('shows Responses event activity without treating exits as passes or call demand in %s', async locale => {
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), responses_capability: responsesRuntime() })
    const wrapper = await open(locale)
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    const panel = wrapper.get('[data-testid="probe-budget-responses-runtime"]')
    expect(panel.get('[data-testid="responses-runtime-state"]').text()).toBe(locale === 'zh' ? '事件执行中' : 'Processing events')
    expect(panel.get('[data-testid="responses-runtime-counts"]').text()).toContain(locale === 'zh' ? '5 次启动' : '5 started')
    expect(panel.get('[data-testid="responses-runtime-counts"]').text()).toContain(locale === 'zh' ? '3 次退出' : '3 exited')
    expect(panel.get('[data-testid="responses-runtime-counts"]').text()).toContain(locale === 'zh' ? '2 次在途' : '2 in flight')
    expect(panel.text()).toContain(locale === 'zh' ? '退出不等于检测通过' : 'An exit is not a passing capability result')
    expect(panel.get('[data-testid="responses-runtime-timeouts"]').text()).toContain('30')
    expect(panel.get('[data-testid="responses-runtime-timeouts"]').text()).toContain('15')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toBe('69456 / 80000')
  })

  it.each(['en', 'zh'])('keeps absent, future and invalid Responses runtime unknown in %s', async locale => {
    for (const runtime of [undefined, { ...responsesRuntime(), version: 2 }, { ...responsesRuntime(), scope: 'fleet' },
      { ...responsesRuntime(), state: 'future', started_events: -1 },
      { ...responsesRuntime(), started_events: 4 }, { ...responsesRuntime(), observed_at: 'bad' },
      { ...responsesRuntime(), instance_id: '' }, { ...responsesRuntime(), finished_events: Number.MAX_SAFE_INTEGER + 1 }]) {
      api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), responses_capability: runtime } })
      const wrapper = await open(locale)
      const panel = wrapper.get('[data-testid="probe-budget-responses-runtime"]')
      expect(panel.get('[data-testid="responses-runtime-state"]').text()).toBe(locale === 'zh' ? '未知' : 'Unknown')
      expect(panel.find('[data-testid="responses-runtime-counts"]').exists()).toBe(false)
      wrapper.unmount()
    }
  })

  it.each(['en', 'zh'])('distinguishes a real empty instance, an unbound source and invalid deadline/time data in %s', async locale => {
    const runtime = { ...responsesRuntime(), state: 'idle', started_events: 0, finished_events: 0, active_events: 0, last_started_at: null, last_finished_at: null }
    api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), responses_capability: runtime } })
    let wrapper = await open(locale)
    expect(wrapper.get('[data-testid="responses-runtime-state"]').text()).toBe(locale === 'zh' ? '空闲（事件驱动）' : 'Idle (event-driven)')
    expect(wrapper.text()).toContain(locale === 'zh' ? '本实例尚未观察到事件' : 'No event observed in this instance')
    wrapper.unmount()
    api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), responses_capability: { ...runtime, state: 'unavailable', instance_id: '' } } })
    wrapper = await open(locale)
    expect(wrapper.get('[data-testid="responses-runtime-state"]').text()).toBe(locale === 'zh' ? '运行依赖未接入' : 'Runtime dependencies unavailable')
    expect(wrapper.find('[data-testid="responses-runtime-counts"]').exists()).toBe(false)
    wrapper.unmount()
    api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), responses_capability: {
      ...responsesRuntime(), lifecycle_timeout_seconds: -1, http_timeout_seconds: 99, last_started_at: 'bad', last_finished_at: 'bad' } } })
    wrapper = await open(locale)
    expect(wrapper.get('[data-testid="responses-runtime-timeouts"]').text()).toContain(locale === 'zh' ? '未知' : 'Unknown')
    expect(wrapper.get('[data-testid="responses-runtime-times"]').text()).not.toContain('bad')
  })

  it('invalidates Responses runtime on a changed draft and a failed preview', async () => {
    api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), responses_capability: responsesRuntime() } })
    const wrapper = await open()
    expect(wrapper.find('[data-testid="probe-budget-responses-runtime"]').exists()).toBe(true)
    await wrapper.get('[id="probe-budget-requests_per_day"]').setValue(100001)
    expect(wrapper.find('[data-testid="probe-budget-responses-runtime"]').exists()).toBe(false)
    api.previewMonitoringProbeCoverage.mockRejectedValueOnce(new Error('unavailable'))
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="probe-budget-responses-runtime"]').exists()).toBe(false)
  })

  it.each(['en', 'zh'])('shows Ollama activity and 429 queues separately from projected call demand in %s', async locale => {
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), ollama_usage: ollamaRuntime() })
    const wrapper = await open(locale)
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    const panel = wrapper.get('[data-testid="probe-budget-ollama-runtime"]')
    expect(panel.get('[data-testid="ollama-runtime-state"]').text()).toBe(locale === 'zh' ? '运行中' : 'Running')
    expect(panel.get('[data-testid="ollama-periodic-state"]').text()).toBe(locale === 'zh' ? '已启用（活动驱动）' : 'Enabled (activity-driven)')
    expect(panel.get('[data-testid="ollama-runtime-scan"]').text()).toContain(locale === 'zh' ? '3 个候选' : '3 candidates')
    expect(panel.get('[data-testid="ollama-runtime-scan"]').text()).toContain(locale === 'zh' ? '2 个到期凭据组' : '2 due credential groups')
    expect(panel.text()).toContain(locale === 'zh' ? '不是全部上游库存' : 'not the full upstream inventory')
    expect(panel.get('[data-testid="ollama-runtime-queue"]').text()).toContain('4')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toBe('69456 / 80000')
  })

  it.each(['en', 'zh'])('keeps missing or future Ollama runtime unknown in %s', async locale => {
    for (const runtime of [undefined, { ...ollamaRuntime(), version: 2 }, { ...ollamaRuntime(), scope: 'fleet' }]) {
      api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), ollama_usage: runtime } })
      const wrapper = await open(locale)
      expect(wrapper.get('[data-testid="ollama-runtime-state"]').text()).toBe(locale === 'zh' ? '未知' : 'Unknown')
      expect(wrapper.find('[data-testid="ollama-runtime-scan"]').exists()).toBe(false)
      expect(wrapper.find('[data-testid="ollama-runtime-queue"]').exists()).toBe(false)
      wrapper.unmount()
    }
  })

  it.each(['en', 'zh'])('distinguishes bounded, failed and unobserved Ollama scans without fabricating zero in %s', async locale => {
    for (const [state, label] of Object.entries(locale === 'zh'
      ? { observed: '最近到期扫描', disabled: '自动刷新停用', not_leader: '本次未取得周期执行权', settings_unavailable: '配置读取失败', inventory_unavailable: '候选读取失败' }
      : { observed: 'Last due scan', disabled: 'Automatic refresh disabled', not_leader: 'This cycle did not acquire leadership', settings_unavailable: 'Settings read failed', inventory_unavailable: 'Candidate read failed' })) {
      const runtime = ollamaRuntime()
      runtime.last_scan = { ...runtime.last_scan, state, candidates: state === 'observed' ? 0 : null, due_groups: state === 'observed' ? 0 : null, limit_reached: state === 'observed' ? true : null }
      api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), ollama_usage: runtime } })
      const wrapper = await open(locale)
      expect(wrapper.get('[data-testid="ollama-scan-state"]').text()).toBe(label)
      if (state === 'observed') {
        expect(wrapper.get('[data-testid="ollama-runtime-scan"]').text()).toContain(locale === 'zh' ? '0 个候选' : '0 candidates')
        expect(wrapper.text()).toContain(locale === 'zh' ? '达到单次上限' : 'Reached the per-scan limit')
      } else expect(wrapper.find('[data-testid="ollama-runtime-scan"]').exists()).toBe(false)
      wrapper.unmount()
    }
    for (const patch of [{ candidates: -1 }, { due_groups: 9 }, { candidates: null }, { observed_at: 'bad' }]) {
      const runtime = ollamaRuntime()
      api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), ollama_usage: { ...runtime, last_scan: { ...runtime.last_scan, ...patch } } } })
      const wrapper = await open(locale)
      expect(wrapper.find('[data-testid="ollama-runtime-scan"]').exists()).toBe(false)
      wrapper.unmount()
    }
  })

  it('clears Ollama runtime along with stale drafts and preview failures', async () => {
    api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), ollama_usage: ollamaRuntime() } })
    const wrapper = await open()
    expect(wrapper.find('[data-testid="probe-budget-ollama-runtime"]').exists()).toBe(true)
    await wrapper.get('[id="probe-budget-requests_per_day"]').setValue(100001)
    expect(wrapper.find('[data-testid="probe-budget-ollama-runtime"]').exists()).toBe(false)
    api.previewMonitoringProbeCoverage.mockRejectedValueOnce(new Error('unavailable'))
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="probe-budget-ollama-runtime"]').exists()).toBe(false)
  })

  it.each(['en', 'zh'])('shows the responding CN process and its observed inventory without forecasting calls in %s', async locale => {
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), cn_quota_cycle: {
      version: 1, scope: 'responding_process', instance_id: 'fixture-process', state: 'running', interval_seconds: 600,
      observed_at: '2026-11-19T12:00:00Z', last_scan: {
        observed_at: '2026-11-19T11:59:00Z', complete: false, targets: 12, balance_targets: 2, quota_targets: 10
      }
    } })
    const wrapper = await open(locale)
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    const panel = wrapper.get('[data-testid="probe-budget-cn-runtime"]')
    expect(panel.text()).toContain(locale === 'zh' ? '本次响应进程' : 'Responding process')
    expect(panel.get('[data-testid="cn-runtime-state"]').text()).toBe(locale === 'zh' ? '运行中' : 'Running')
    expect(panel.text()).toContain('600')
    expect(panel.get('[data-testid="cn-runtime-inventory"]').text()).toContain('12')
    expect(panel.text()).toContain(locale === 'zh' ? '部分读取' : 'Partial scan')
    expect(panel.text()).toContain(locale === 'zh' ? '不代表其他节点' : 'does not cover other nodes')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toBe('69456 / 80000')
  })

  it.each(['en', 'zh'])('keeps unobserved or unrecognized CN runtime unknown in %s', async locale => {
    for (const runtime of [undefined, { version: 1, scope: 'future', state: 'running' }, { version: 2, scope: 'responding_process', state: 'running' }, {
      version: 1, scope: 'responding_process', state: 'future', interval_seconds: null, last_scan: null
    }]) {
      api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), cn_quota_cycle: runtime })
      const wrapper = await open(locale)
      await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
      await flushPromises()
      const panel = wrapper.get('[data-testid="probe-budget-cn-runtime"]')
      expect(panel.get('[data-testid="cn-runtime-state"]').text()).toBe(locale === 'zh' ? '未知' : 'Unknown')
      expect(panel.find('[data-testid="cn-runtime-inventory"]').exists()).toBe(false)
      expect(panel.text()).toContain(locale === 'zh' ? '未观察到库存扫描' : 'No inventory scan observed')
      wrapper.unmount()
    }
  })

  it.each(['en', 'zh'])('preserves lifecycle, explicit empty scans and malformed-scan unknowns in %s', async locale => {
    const states = ['running', 'stopping', 'stopped', 'disabled', 'role_disabled', 'not_started', 'unavailable']
    const labels = locale === 'zh'
      ? ['运行中', '停止中（等待在途收尾）', '已停止', '配置停用', '本节点角色不执行周期任务', '尚未启动', '运行依赖未接入']
      : ['Running', 'Stopping (waiting for in-flight work)', 'Stopped', 'Disabled by configuration', 'Node role excludes periodic jobs', 'Not started', 'Runtime dependencies unavailable']
    const scan = { observed_at: '2026-11-19T11:59:00Z', complete: true, targets: 0, balance_targets: 0, quota_targets: 0 }
    const runtime = { version: 1, scope: 'responding_process', instance_id: 'fixture-process', state: 'running', interval_seconds: 600, observed_at: scan.observed_at, last_scan: scan }
    for (const [index, state] of states.entries()) {
      api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), cn_quota_cycle: { ...runtime, state } } })
      const wrapper = await open(locale)
      expect(wrapper.get('[data-testid="cn-runtime-state"]').text()).toBe(labels[index])
      expect(wrapper.get('[data-testid="cn-runtime-inventory"]').text()).toContain(locale === 'zh' ? '0 个目标' : '0 targets')
      wrapper.unmount()
    }
    for (const last_scan of [{ ...scan, targets: -1 }, { ...scan, targets: 1 }, { ...scan, complete: undefined }, { ...scan, observed_at: 'invalid' }]) {
      api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), cn_quota_cycle: { ...runtime, last_scan } } })
      const wrapper = await open(locale)
      expect(wrapper.find('[data-testid="cn-runtime-inventory"]').exists()).toBe(false)
      wrapper.unmount()
    }
  })

  it('invalidates the runtime projection along with a changed draft, without sending a probe', async () => {
    api.getMonitoringProbeBudget.mockResolvedValueOnce({ ...summary(), coverage: { ...coverage(), cn_quota_cycle: {
      version: 1, scope: 'responding_process', instance_id: 'fixture-process', state: 'running', interval_seconds: 600,
      observed_at: '2026-11-19T12:00:00Z', last_scan: null
    } } })
    const wrapper = await open()
    expect(wrapper.find('[data-testid="probe-budget-cn-runtime"]').exists()).toBe(true)
    await wrapper.get('#probe-budget-requests_per_day').setValue(12345)
    expect(wrapper.find('[data-testid="probe-budget-cn-runtime"]').exists()).toBe(false)
    expect(api.previewMonitoringProbeCoverage).not.toHaveBeenCalled()
    expect(api.updateMonitoringProbeBudget).not.toHaveBeenCalled()
  })

  it.each(['en', 'zh'])('discloses unestimated runtime and event demand without forecasting it in %s', async locale => {
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), billing_enabled: false,
      account_plans: 0, group_plans: 0, candy_probes: 0, candy_inventory_complete: true,
      active_monitors: 0, active_inventory_complete: true, minimum_total_requests_per_day: 0,
      unestimated_demand_sources: ['account_quota_cycle', 'ollama_usage', 'responses_capability', 'event_diagnostics'] })
    const wrapper = await open(locale)
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    const panel = wrapper.get('[data-testid="probe-budget-unestimated-demand"]')
    expect(panel.findAll('[data-source]')).toHaveLength(4)
    expect(panel.text()).toContain('Ollama')
    expect(panel.text()).toContain('Responses')
    expect(panel.text()).toContain(locale === 'zh' ? '未估算' : 'Unestimated')
    expect(panel.text()).toContain(locale === 'zh' ? '不是已启用库存' : 'not an inventory of enabled')
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '已编译范围' : 'compiled scopes')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toBe('0 / 80000')
    await wrapper.get('[data-testid="probe-budget-save"]').trigger('click')
    await flushPromises()
    expect(api.updateMonitoringProbeBudget).toHaveBeenCalledTimes(1)
  })
  it.each(['missing', 'empty', 'future'])('keeps %s demand information unknown, sanitized and bounded', async mode => {
    const sources = mode === 'missing' ? undefined : mode === 'empty' ? [] : ['ollama_usage', 'ollama_usage', 'PRIVATE_SOURCE', 'ANOTHER_PRIVATE_SOURCE']
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), unestimated_demand_sources: sources })
    const wrapper = await open()
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    const panel = wrapper.get('[data-testid="probe-budget-unestimated-demand"]')
    expect(panel.text()).toContain('Unknown')
    expect(panel.text()).not.toContain('PRIVATE_SOURCE')
    expect(panel.text()).not.toContain('ANOTHER_PRIVATE_SOURCE')
    expect(panel.findAll('[data-source="unknown"]')).toHaveLength(1)
    expect(panel.findAll('[data-source]')).toHaveLength(mode === 'future' ? 2 : 1)
  })
  it('does not report known scopes inactive when schedule inventory is incomplete', async () => {
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), billing_enabled: false,
      account_plans: 0, group_plans: 0, candy_probes: 0, candy_inventory_complete: true,
      active_monitors: 0, active_inventory_complete: true, plan_inventory_complete: false })
    const wrapper = await open()
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain('still unverified')
  })
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
      candy_inventory_complete: true, minimum_candy_requests: 0, minimum_total_requests_per_day: 0,
      active_monitors: 0, active_inventory_complete: true })
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '均未启用' : 'inactive')
  })
  it.each(['en', 'zh'])('separates V1 physical lower bounds from unestimated quota calls in %s', async locale => {
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), billing_enabled: false,
      account_plans: 0, group_plans: 0, candy_probes: 0, candy_inventory_complete: true,
      active_monitors: 3, active_quota_monitors: 2, unresolved_active_monitors: 0,
      active_inventory_complete: true, active_monitor_runs: 2880, minimum_active_requests: 4320,
      minimum_total_requests_per_day: 4320, warnings: ['active_quota_unestimated'] })
    const wrapper = await open(locale)
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    const panel = wrapper.get('[data-testid="probe-budget-active-capacity"]')
    expect(panel.text()).toContain('4320')
    expect(panel.text()).toContain('2880')
    expect(panel.text()).toContain(locale === 'zh' ? '配额' : 'quota')
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '仍不是全覆盖保证' : 'still unverified')
    expect(wrapper.get('[data-testid="probe-budget-capacity-reasons"]').text()).not.toContain('active_quota_unestimated')
    expect(wrapper.get('[data-testid="probe-budget-total-calls"]').text()).toBe('4320 / 80000')
  })
  it.each(['en', 'zh'])('keeps absent V1 inventory unknown, not inactive, in %s', async locale => {
    api.previewMonitoringProbeCoverage.mockResolvedValue({ ...coverage(), billing_enabled: false,
      account_plans: 0, group_plans: 0, candy_probes: 0, candy_inventory_complete: true })
    const wrapper = await open(locale)
    await wrapper.get('[data-testid="probe-budget-preview"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="probe-budget-active-capacity"]').text()).toContain(locale === 'zh' ? '未知' : 'Unknown')
    expect(wrapper.get('[data-testid="probe-budget-capacity-verdict"]').text()).toContain(locale === 'zh' ? '仍不是全覆盖保证' : 'still unverified')
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
