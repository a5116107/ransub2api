import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { Account } from '@/types'
import type { ModelTraceResult } from '@/api/admin/modelTrace'
import ModelTracePanel from '../ModelTracePanel.vue'

const { runModelTrace } = vi.hoisted(() => ({ runModelTrace: vi.fn() }))
vi.mock('@/api/admin/modelTrace', () => ({ runModelTrace }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const record: ModelTraceResult = {
  requested_model: 'public-model', reasoning_effort: '', status: 'partial', disagreement: false,
  created_at: '2026-10-18T12:00:00Z',
  bank: { source_commit: 'fixture', sha256: 'fixture', algorithm: 'fixture', built_at: '', models: ['gpt-6-astra'] },
  samples: [{ challenge: { id: 'sample1', expected_count: 300, prompt: 'mock question' }, text: 'mock numbers',
    environment: { protocol: 'responses', system_prompt: 'none', oauth: false }, parsed_numbers: 200, minimum_numbers: 165,
    accepted: true, mapped_model: 'gpt-6-astra', started_at: '2026-10-18T12:00:00Z', latency_ms: 1000 }],
  analysis: { prediction: 'gpt-6-astra', probability: 0.9, used_outputs: 1,
    candidates: [{ model: 'gpt-6-astra', family: 'gpt', probability: 0.9 }], families: [{ model: '', family: 'gpt', probability: 1 }] }
}

function mountPanel(account = {}) {
  return mount(ModelTracePanel, { props: {
    account: { id: 42, platform: 'openai', type: 'apikey', ...account } as Account,
    modelId: 'public-model', modelOptions: [], reasoningLevels: ['low', 'medium', 'high']
  }, global: { stubs: { TestModelSelect: true, Select: true } } })
}

beforeEach(() => { localStorage.clear(); runModelTrace.mockReset() })

describe('ModelTracePanel', () => {
  it.each([
    ['request_limit', 'global_minute'], ['recovery_reserve', 'global_day'],
    ['token_limit', 'global_minute'], ['token_limit', 'global_day'],
    ['token_limit', 'site_minute'], ['token_limit', 'key_minute'],
    ['request_limit', 'site_minute'], ['request_limit', 'key_minute'],
    ['concurrency_limit', 'global'], ['concurrency_limit', 'site'], ['concurrency_limit', 'key'],
    ['budget_unavailable', 'system']
  ])('labels the backend budget reason %s and scope %s', async (reason, scope) => {
    runModelTrace.mockResolvedValue({ ...record, budget_deferral: { reason, scope, retry_at: '2026-11-18T00:00:00Z' } })
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="modeltrace-start"]').trigger('click')
    await flushPromises()
    const notice = wrapper.get('[data-testid="modeltrace-budget-deferral"]')
    expect(notice.text()).toContain(`admin.settings.upstreamBillingProbe.budget.${reason}`)
    expect(notice.text()).toContain(`modelTrace.budgetScopes.${scope}`)
    expect(notice.text()).not.toContain('modelTrace.budgetReasonUnknown')
    wrapper.unmount()
  })

  it('explains a budget pause independently of preliminary attribution and restores it from history', async () => {
    const deferredRecord = { ...record, budget_deferral: { reason: 'request_limit', scope: 'global_day', retry_at: '2026-11-18T00:00:00Z' } }
    runModelTrace.mockResolvedValue(deferredRecord)
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="modeltrace-start"]').trigger('click')
    await flushPromises()
    const notice = wrapper.get('[data-testid="modeltrace-budget-deferral"]')
    expect(notice.attributes('role')).toBe('status')
    expect(notice.text()).toContain('modelTrace.budgetPaused')
    expect(notice.text()).toContain('admin.settings.upstreamBillingProbe.budget.request_limit')
    expect(notice.text()).toContain('modelTrace.budgetScopes.global_day')
    expect(notice.get('time').attributes('datetime')).toBe(deferredRecord.budget_deferral.retry_at)
    expect(wrapper.get('[data-testid="modeltrace-result"]').text()).toContain('modelTrace.status.partial')
    expect(wrapper.text()).toContain('modelTrace.partialHint')
    expect(wrapper.text()).not.toContain('modelTrace.errors.generation_failed')
    wrapper.unmount()
    const restored = mountPanel()
    await restored.get('[data-testid="modeltrace-history-record"]').trigger('click')
    expect(restored.get('[data-testid="modeltrace-budget-deferral"]').text()).toContain('modelTrace.budgetPaused')
    expect(runModelTrace).toHaveBeenCalledTimes(1)
    restored.unmount()
  })

  it.each(['constructor', '__proto__', 'future_reason'])('uses neutral labels for an unrecognized budget reason or scope: %s', async value => {
    runModelTrace.mockResolvedValue({ ...record, budget_deferral: { reason: value, scope: value, retry_at: 'invalid' } })
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="modeltrace-start"]').trigger('click')
    await flushPromises()
    const notice = wrapper.get('[data-testid="modeltrace-budget-deferral"]')
    expect(notice.text()).toContain('modelTrace.budgetReasonUnknown')
    expect(notice.text()).toContain('modelTrace.budgetScopes.unknown')
    expect(notice.text()).toContain('modelTrace.retryUnknown')
    expect(notice.find('time').exists()).toBe(false)
    expect(notice.text()).not.toContain(value)
    wrapper.unmount()
  })

  it('displays partial attribution and restores browser history without making a model request', async () => {
    runModelTrace.mockResolvedValue(record)
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="modeltrace-start"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="modeltrace-result"]').text()).toContain('modelTrace.status.partial')
    expect(wrapper.text()).toContain('modelTrace.partialHint')
    expect(wrapper.text()).toContain('gpt-6-astra')
    expect(wrapper.find('[data-testid="modeltrace-budget-deferral"]').exists()).toBe(false)
    expect(JSON.parse(localStorage.getItem('sub2api-modeltrace-test:42')!)).toEqual([record])
    wrapper.unmount()
    const restored = mountPanel()
    expect(restored.text()).toContain('modelTrace.history')
    expect(runModelTrace).toHaveBeenCalledTimes(1)
    restored.unmount()
  })

  it('aborts on stop and never saves a late completed result', async () => {
    let finish!: (record: ModelTraceResult) => void
    runModelTrace.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="modeltrace-start"]').trigger('click')
    const signal = runModelTrace.mock.calls[0]![3] as AbortSignal
    await wrapper.get('[data-testid="modeltrace-cancel"]').trigger('click')
    expect(signal.aborted).toBe(true)
    finish(record)
    await flushPromises()
    expect(localStorage.getItem('sub2api-modeltrace-test:42')).toBeNull()
    expect(wrapper.find('[data-testid="modeltrace-result"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('aborts when the panel closes', async () => {
    runModelTrace.mockImplementation(() => new Promise(() => undefined))
    const wrapper = mountPanel()
    await wrapper.get('[data-testid="modeltrace-start"]').trigger('click')
    const signal = runModelTrace.mock.calls[0]![3] as AbortSignal
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
  })

  it('warns about retained Claude OAuth prompts and disables unsupported platforms', () => {
    const claude = mountPanel({ platform: 'anthropic', type: 'oauth' })
    expect(claude.find('[data-testid="modeltrace-system-warning"]').exists()).toBe(true)
    claude.unmount()
    const unsupported = mountPanel({ platform: 'gemini' })
    expect((unsupported.get('[data-testid="modeltrace-start"]').element as HTMLButtonElement).disabled).toBe(true)
    unsupported.unmount()
  })
})
