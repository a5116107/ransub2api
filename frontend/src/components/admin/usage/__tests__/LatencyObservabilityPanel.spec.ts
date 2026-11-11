import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import LatencyObservabilityPanel from '../LatencyObservabilityPanel.vue'
import en from '@/i18n/locales/en/latencyObservability'
import zh from '@/i18n/locales/zh/latencyObservability'
function localePlugin(locale = 'en') {
  return createI18n({ legacy: false, locale, messages: { [locale]: { latencyObservability: locale === 'zh' ? zh : en, common: { loading: locale === 'zh' ? '加载中' : 'Loading' } } } })
}
const mocks = vi.hoisted(() => ({ read: vi.fn() }))
vi.mock('@/api/admin/latencyObservability', () => ({ getLatencyReport: mocks.read }))
const group = { body_bucket: '1-4MiB', cache_bucket: '95%+', reasoning_effort: 'xhigh', requests: 2, traces: 2, attempts: 3, reasoning_samples: 1, reasoning_first_avg_ms: 120, text_samples: 1, visible_text_first_avg_ms: 450, byte_samples: 1, response_byte_first_avg_ms: 30, upload_samples: 1, wait_samples: 0, visible_samples: 0, body_upload_avg_ms: 0, post_upload_first_content_wait_avg_ms: null, post_upload_first_visible_wait_avg_ms: null, large_body_switches: 1, large_body_same_account_retries: 1, failover_samples: 1, cost_samples: 0, extra_upload_component_ms: null, extra_wait_component_ms: null }
const result = { groups: [group], end_to_end_ms: null, end_to_end_status: 'not_collected', end_to_end_source: 'none', client_upload_ms: null, client_upload_status: 'not_collected' }
const coldStart = { status: 'historical_association', method: 'independent_history_p90_difference_not_causal', min_samples: 5, max_age_seconds: 900, cold_samples: 5, warm_samples: 6, cold_p90_ms: 6000, warm_p90_ms: 1000, delta_ms: 5000, as_of: '2026-11-11T00:00:00Z', latest_cold_at: '2026-11-10T23:59:00Z', latest_warm_at: '2026-11-10T23:59:00Z' }
describe('Issue603 latency observations', () => {
  for (const [locale, messages] of Object.entries({ en, zh })) {
    it(`shows cache evidence, route cohorts and historical uncertainty in ${locale}`, async () => {
      const base = { ...group, account_id: 4, model: 'public', upstream_model: 'actual', service_tier: 'default', upstream_endpoint: '/v1/responses', route_fingerprint: 'a'.repeat(64), proxy_id: 0, stream: true, cache_source: 'canonical_responses_terminal_usage', cold_start: coldStart }
      mocks.read.mockResolvedValue({ ...result, groups: [base, { ...base, service_tier: 'priority', route_fingerprint: 'b'.repeat(64) }] })
      const w = mount(LatencyObservabilityPanel, { global: { plugins: [localePlugin(locale)] } })
      await flushPromises()
      expect(w.findAll('[data-testid="latency-group"]')).toHaveLength(2)
      expect(w.text()).toContain(messages.cache)
      expect(w.text()).toContain(messages.cacheCanonical)
      expect(w.text()).toContain(messages.coldHistorical)
      expect(w.text()).toContain(messages.coldScope)
      expect(w.text()).toContain('actual · default')
      expect(w.text()).toContain('actual · priority')
      expect(w.text()).toContain('aaaaaaaaaaaa')
      expect(w.text()).toContain('bbbbbbbbbbbb')
      expect(w.get('[data-testid="cache-cold-comparison"]').text()).toContain('5000.0 ms')
      expect(w.get('[role="region"]').attributes('tabindex')).toBe('0')
      w.unmount()
    })
  }
  for (const [status, key] of Object.entries({ unknown_scope: 'coldUnknownScope', insufficient_samples: 'coldInsufficient', stale_samples: 'coldStale', invalid_window: 'coldInvalidWindow', invalid_measurement: 'coldInvalidMeasurement', ambiguous_cohort: 'coldAmbiguous' } as const)) {
    it(`hides estimated savings for ${status}`, async () => {
      mocks.read.mockResolvedValue({ ...result, groups: [{ ...group, cold_start: { ...coldStart, status } }] })
      const w = mount(LatencyObservabilityPanel, { global: { plugins: [localePlugin()] } })
      await flushPromises()
      expect(w.get('[data-testid="cache-cold-start"]').text()).toContain(en[key])
      expect(w.find('[data-testid="cache-cold-comparison"]').exists()).toBe(false)
      w.unmount()
    })
  }
  for (const [locale, messages] of Object.entries({ en, zh })) {
    it(`keeps canonical protocol evidence distinct in ${locale}`, async () => {
      mocks.read.mockResolvedValue({ ...result, groups: [
        { ...group, cache_source: 'canonical_messages_cumulative_usage' },
        { ...group, cache_source: 'canonical_gemini_usage_metadata' },
        { ...group, cache_source: 'future_unrecognized_source' },
      ] })
      const w = mount(LatencyObservabilityPanel, { global: { plugins: [localePlugin(locale)] } })
      await flushPromises()
      const evidence = w.findAll('[data-testid="cache-evidence"]')
      expect(evidence[0].text()).toContain(messages.cacheMessages)
      expect(evidence[1].text()).toContain(messages.cacheGemini)
      expect(evidence[2].text()).toContain(messages.notCollected)
      expect(w.text()).not.toContain(messages.cacheCanonical)
      w.unmount()
    })
  }
  it('preserves explicit zero cache misses and never invents a cold-start estimate for legacy reports', async () => {
    mocks.read.mockResolvedValue({ ...result, groups: [{ ...group, cache_bucket: '0%', cache_source: 'canonical_responses_terminal_usage' }, { ...group, cache_bucket: 'unknown', cache_source: 'none' }] })
    const w = mount(LatencyObservabilityPanel, { global: { plugins: [localePlugin()] } })
    await flushPromises()
    expect(w.get('[data-testid="cache-evidence"]').text()).toContain('0%')
    expect(w.findAll('[data-testid="cache-cold-start"]')[0].text()).toBe(en.notCollected)
    expect(w.find('[data-testid="cache-cold-comparison"]').exists()).toBe(false)
    w.unmount()
  })
  for (const [locale, messages] of Object.entries({ en, zh })) {
    it(`omits client timing while retaining server observations in ${locale}`, async () => {
      mocks.read.mockResolvedValue(result)
      const w = mount(LatencyObservabilityPanel, { props: { model: 'fixture', accountId: 4 }, global: { plugins: [localePlugin(locale)] } })
      await flushPromises()
      expect(w.text()).toContain('0.0 ms (n=1)')
      expect(w.text()).toContain(`${messages.notCollected} (n=0)`)
      expect(w.text()).not.toMatch(/Client end-to-end time|Client upload before application entry|客户端端到端耗时|进入应用前的客户端上传/)
      expect(w.find('[data-testid="reasoning-capability"]').exists()).toBe(false)
      expect(w.text()).toContain('1 / 1 (n=1)')
      expect(w.text()).toContain(messages.reasoning_first_ms)
      expect(w.text()).toContain('120.0 ms (n=1)')
      expect(w.text()).toContain(messages.visible_text_first_ms)
      expect(w.text()).toContain('450.0 ms (n=1)')
      expect(w.text()).toContain(messages.response_byte_first_ms)
      expect(w.text()).toContain('30.0 ms (n=1)')
      expect(mocks.read).toHaveBeenLastCalledWith(false, expect.objectContaining({ account_id: 4, model: 'fixture' }), expect.any(AbortSignal))
      w.unmount()
    })
  }
  it('rejects stale report responses when filters change', async () => {
    let resolve!: (v: unknown) => void
    mocks.read.mockImplementationOnce(() => new Promise(r => { resolve = r })).mockResolvedValueOnce({ ...result, groups: [] })
    const w = mount(LatencyObservabilityPanel, { props: { model: 'old' }, global: { plugins: [localePlugin()] } })
    await w.setProps({ model: 'new', ops: true }); await flushPromises()
    resolve(result); await flushPromises()
    expect(w.find('[data-testid="latency-group"]').exists()).toBe(false)
    expect(mocks.read).toHaveBeenLastCalledWith(true, expect.objectContaining({ model: 'new' }), expect.any(AbortSignal))
    w.unmount()
  })
})

it('omits unavailable reasoning capability and retains per-hop socket evidence scope', async () => {
  mocks.read.mockResolvedValue(result)
  const w = mount(LatencyObservabilityPanel, { props: { ops: true }, global: { plugins: [localePlugin()] } })
  await flushPromises()
  expect(w.find('[data-testid="reasoning-capability"]').exists()).toBe(false)
  expect(w.get('[data-testid="socket-evidence"]').text()).toContain('source none')
  expect(w.get('[data-testid="socket-evidence"]').text()).toContain('reverse proxy')
  expect(w.text()).toContain(en.typedScope)
  w.unmount()
})

it('supports flat embedding without a duplicate title inside the latency tab', async () => {
  mocks.read.mockResolvedValue({ ...result, groups: [] })
  const w = mount(LatencyObservabilityPanel, {
    props: { flat: true, hideTitle: true },
    global: { plugins: [localePlugin()] },
  })
  await flushPromises()
  expect(w.get('[data-testid="latency-observability"]').classes()).not.toContain('card')
  expect(w.find('h3').exists()).toBe(false)
  w.unmount()
})
