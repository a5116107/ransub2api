import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import type { OperationsSnapshot } from '@/api/admin/upstreamOperations'
import { getResult } from '@/api/admin/pelicanTests'
import UpstreamLineCards from './UpstreamLineCards.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => args ? `${key} ${Object.values(args).join(' ')}` : key }) }))
vi.mock('@/api/admin/pelicanTests', () => ({ getResult: vi.fn() }))
vi.mock('@/components/account/UpstreamBillingRateCell.vue', () => ({ default: { props: { readOnly: Boolean }, template: '<div data-testid="price-read-only">{{ readOnly }}</div>' } }))
vi.mock('@/components/user/pelican/PelicanArtworkPreview.vue', () => ({ default: { template: '<div data-testid="sandbox-preview" />' } }))
vi.mock('@/components/user/pelican/PelicanShowcaseCard.vue', () => ({ default: { props: ['item', 'body'], emits: ['visible', 'open'], template: '<div><span data-testid="art-status">{{ body?.status }}</span><button data-testid="load-art" @click="$emit(\'visible\')">Read saved artwork</button><button data-testid="open-art" @click="$emit(\'open\')">Open</button></div>' } }))

const artwork = { id: 4, account_id: 9, group_id: 2, model: 'tested-model', reasoning_effort: 'low', started_at: '2026-11-25T08:00:00Z' }
function fixture(): OperationsSnapshot {
 return {
  scope: {}, queried_at: '2026-11-25T10:00:00Z',
  usage: { members: [{ id: 9, name: 'Line nine', type: 'apikey', platform: 'openai', key: 'https://line.invalid', schedulable: true, status: 'active', concurrency: 10, rate_multiplier: .2, cost_multiplier: .08, metrics: { requests: 2, p50_ms: 100, p90_ms: 200 } }, { id: 14, name: 'Paused line', schedulable: false, status: 'active' }], summary: {}, rows: [], total: 0, catalog: [] },
  quality: { status: 'ready', data: { rows: [] } }, raw_cache: { status: 'ready', data: { rows: [{ account_id: 9, public_model: 'tested-model', token_read_rate: 0, service_tier: 'standard', reasoning_effort: 'low' }] } },
  model_tests: { status: 'ready', data: { version: 1, source: 'persisted_pelican_group_test_frozen_model', total: 1, rows: [{ ...artwork, id: 5, plan_id: 3, status: 'failed', has_output: false, finished_at: '2026-11-25T08:01:00Z', attempts: [{ account_id: 14 }], artwork: { ...artwork } }] } }
 } as unknown as OperationsSnapshot
}
const render = (snapshot = fixture()) => mount(UpstreamLineCards, { props: { snapshot }, global: { stubs: { Teleport: true } } })
const detail = () => ({ ...artwork, status: 'success', pelican_config: { model_id: artwork.model, reasoning_effort: artwork.reasoning_effort }, response_text: '<svg xmlns="http://www.w3.org/2000/svg"><text>Historical output</text></svg>' })

describe('actual upstream line panel', () => {
 it('shows actual tested model, latest failure and historical artwork without equating generation to verification', () => {
  const w = render()
  expect(w.findAll('[data-testid="upstream-line-card"]')).toHaveLength(2)
  expect(w.get('[data-testid="line-test-model"]').text()).toBe('tested-model')
  expect(w.get('[data-testid="line-test-outcome"]').text()).toContain('executionFailed')
  expect(w.get('[data-testid="line-test-attribution"]').text()).toContain('finalAccount 9')
  expect(w.get('[data-testid="line-test-attribution"]').text()).toContain('priorAttempt 14')
  expect(w.get('[data-account-id="14"]').find('[data-testid="line-pelican-result"]').exists()).toBe(false)
  expect(w.text()).toContain('0.00%')
  expect(w.get('[data-testid="price-read-only"]').text()).toBe('true')
  expect(w.text()).not.toContain('100%')
  w.unmount()
 })
 it('filters only the current page participation and preserves explicit edit and drill entrypoints', async () => {
  const w = render()
  await w.get('[data-testid="line-participation"]').setValue('off')
  expect(w.findAll('[data-testid="upstream-line-card"]')).toHaveLength(1)
  await w.get('[data-testid="line-details"]').trigger('click')
  expect(w.emitted('select')).toEqual([[14]])
  await w.get('[data-testid="line-price-edit"]').trigger('click')
  expect(w.emitted('edit')).toEqual([[14]])
  w.unmount()
 })
 it('keeps a deleted account with historical usage out of scheduling-enabled lines', async () => {
  const s = fixture()
  s.usage.members[0].deleted = true
  const w = render(s)
  expect(w.get('[data-account-id="9"] header').text()).toContain('upstreamOps.lines.deleted')
  await w.get('[data-testid="line-participation"]').setValue('on')
  expect(w.find('[data-account-id="9"]').exists()).toBe(false)
  w.unmount()
 })
 it.each(['http', 'ws_turn', 'future', 'wrong source', 'no samples', 'unknown transport'])('keeps %s first-text timing bound to its observed transport', mode => {
  const s = fixture()
  const metric = { mean_ms: 123, samples: mode === 'no samples' ? 0 : 1, unknown_samples: 0, coverage: 1, latest_sample_at: null }
  Object.assign(s.raw_cache!.data!.rows[0], { transport: mode === 'unknown transport' ? 'legacy' : mode === 'ws_turn' ? 'ws_turn' : 'http', waits: { version: mode === 'future' ? 2 : 1, source: mode === 'wrong source' ? 'legacy_ttft' : mode === 'ws_turn' ? 'canonical_physical_ws_turn_generation_start' : 'canonical_final_physical_http_attempt', first_text: metric } })
  const w = render(s)
  expect(w.get('[data-account-id="9"]').text()).toContain(`upstreamOps.lines.firstText ${['http', 'ws_turn'].includes(mode) ? '123' : 'upstreamOps.unknown'} ms`)
  w.unmount()
 })
 it.each(['missing', 'future', 'wrong source', 'bad total'])('keeps %s test data unknown instead of showing historical green evidence', mode => {
  const s = fixture()
  if (mode === 'missing') delete s.model_tests
  else if (mode === 'future') s.model_tests!.data!.version = 9
  else if (mode === 'wrong source') s.model_tests!.data!.source = 'client_claim'
  else s.model_tests!.data!.total = 0
  const w = render(s)
  expect(w.find('[data-testid="line-pelican-result"]').exists()).toBe(false)
  expect(w.text()).toContain('testScopeUnknown')
  w.unmount()
 })
 it.each(['model', 'account_id', 'group_id', 'reasoning_effort'])('does not attach an artwork belonging to another %s', key => {
  const s = fixture()
  Object.assign(s.model_tests!.data!.rows[0].artwork!, { [key]: key.endsWith('id') ? 999 : 'different' })
  const w = render(s)
  expect(w.find('[data-testid="line-historical-artwork"]').exists()).toBe(false)
  w.unmount()
 })
 it('lazily reads the original result, extracts HTML and opens its sandbox preview without executing a test', async () => {
  vi.mocked(getResult).mockResolvedValue(detail() as never)
  const w = render()
  await w.get('[data-testid="load-art"]').trigger('click'); await flushPromises()
  expect(getResult).toHaveBeenLastCalledWith(4, expect.any(AbortSignal))
  expect(w.get('[data-testid="art-status"]').text()).toBe('ready')
  await w.get('[data-testid="open-art"]').trigger('click'); await flushPromises()
  expect(w.get('[data-testid="sandbox-preview"]').exists()).toBe(true)
  expect(w.get('[data-testid="line-test-outcome"]').text()).toContain('executionFailed')
  w.unmount()
 })
 it('rejects a mismatched fetched model and recovers only after an explicit read retry', async () => {
  vi.mocked(getResult).mockResolvedValueOnce({ ...detail(), pelican_config: { model_id: 'another-model', reasoning_effort: 'low' } } as never)
  const w = render()
  await w.get('[data-testid="load-art"]').trigger('click'); await flushPromises()
  expect(w.get('[data-testid="art-status"]').text()).toBe('error')
  vi.mocked(getResult).mockResolvedValueOnce(detail() as never)
  await w.findAll('button').find(b => b.text() === 'upstreamOps.lines.retryArtwork')!.trigger('click'); await flushPromises()
  expect(w.get('[data-testid="art-status"]').text()).toBe('ready')
  w.unmount()
 })
})
