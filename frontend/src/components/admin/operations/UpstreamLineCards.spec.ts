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
  quality: { status: 'ready', data: { rows: [] } },
  model_tests: { status: 'ready', data: { version: 1, source: 'persisted_pelican_group_test_frozen_model', total: 1, rows: [{ ...artwork, id: 5, plan_id: 3, status: 'failed', has_output: false, finished_at: '2026-11-25T08:01:00Z', attempts: [{ account_id: 14 }], artwork: { ...artwork } }] } }
 } as unknown as OperationsSnapshot
}
const render = (snapshot = fixture()) => mount(UpstreamLineCards, { props: { snapshot }, global: { stubs: { Teleport: true } } })
const detail = () => ({ ...artwork, status: 'success', pelican_config: { model_id: artwork.model, reasoning_effort: artwork.reasoning_effort }, response_text: '<svg xmlns="http://www.w3.org/2000/svg"><text>Historical output</text></svg>' })

function availabilityFixture(): OperationsSnapshot {
 const s=fixture(), evaluated='2026-11-25T10:00:00Z'
 const decision={version:1,status:'pass',reason:'completed_response',started_at:'2026-11-25T09:59:58Z',finished_at:'2026-11-25T09:59:59Z',attempt_count:1,scope:{version:2,protocol:'responses_http',model:'wire-model',stream:true,service_tier:'default',reasoning_effort:'low',capability:'text'}}
 s.availability={status:'ready',queried_at:evaluated,data:{version:1,source:'current_account_availability_authority',plan_total:1,model_total:1,models_loaded:1,truncated:false,reports:[{plan_id:12,account_id:9,configured:true,enabled:true,evaluated_at:evaluated,models:[{model_id:'tested-model',status:'pass',reason:'latest_decision',observation:{...decision,status:'inconclusive'},decision,valid_until:'2026-11-25T10:00:30Z'}]}]}}
 return s
}

describe('latest availability on the original line card',()=>{
 it('separates the latest inconclusive observation from an existing current decision and frozen wire scope',()=>{
  const w=render(availabilityFixture())
  expect(w.get('[data-testid="line-availability-state"]').text()).toBe('upstreamOps.lines.availability.pass')
  expect(w.get('[data-testid="line-availability-model"]').text()).toContain('wire-model')
  expect(w.get('[data-testid="line-availability-model"]').text()).toContain('responses_http')
  expect(w.get('[data-testid="line-availability-model"]').text()).toContain('upstreamOps.lines.availability.unknown')
  expect(w.get('[data-account-id="14"]').find('[data-testid="line-availability-state"]').exists()).toBe(false)
  w.unmount()
 })
 it.each(['future','wrong source','bad total','no scope','no decision','paused','expired','changed route'])('never upgrades %s evidence to a pass',mode=>{
  const s=availabilityFixture(),data=s.availability!.data!,report=data.reports[0],model=report.models[0]
  if(mode==='future')data.version=9
  else if(mode==='wrong source')data.source='historical_success_rate'
  else if(mode==='bad total')data.models_loaded=2
  else if(mode==='no scope')delete model.decision!.scope
  else if(mode==='no decision')delete model.decision
  else if(mode==='paused')report.enabled=false
  else if(mode==='expired')model.valid_until=report.evaluated_at
  else {model.status='unknown';model.reason='route_changed';delete model.decision}
  const w=render(s)
  expect(w.findAll('[data-testid="line-availability-state"]').some(b=>b.text()==='upstreamOps.lines.availability.pass')).toBe(false)
  w.unmount()
 })
 it('expires with monotonic elapsed time rather than extending a pass when the browser wall clock moves back',async()=>{
  let monotonic=0
  vi.useFakeTimers({toFake:['setInterval','clearInterval','Date']})
  const spy=vi.spyOn(performance,'now').mockImplementation(()=>monotonic)
  const w=render(availabilityFixture())
  try {
   expect(w.get('[data-testid="line-availability-state"]').text()).toContain('.pass')
   vi.setSystemTime(new Date('2000-01-01T00:00:00Z'));monotonic=31_000
   await vi.advanceTimersByTimeAsync(1_000)
   expect(w.get('[data-testid="line-availability-state"]').text()).toContain('.expired')
  } finally {w.unmount();spy.mockRestore();vi.useRealTimers()}
 })
 it('charges the full request/read duration before presenting a current pass',()=>{
  const w=mount(UpstreamLineCards,{props:{snapshot:availabilityFixture(),readElapsedMs:31_000},global:{stubs:{Teleport:true}}})
  expect(w.get('[data-testid="line-availability-state"]').text()).toContain('.expired')
  w.unmount()
 })
})

describe('actual upstream line panel', () => {
 it('shows actual tested model, latest failure and historical artwork without equating generation to verification', () => {
  const w = render()
  expect(w.findAll('[data-testid="upstream-line-card"]')).toHaveLength(2)
  expect(w.get('[data-testid="line-test-model"]').text()).toBe('tested-model')
  expect(w.get('[data-testid="line-test-outcome"]').text()).toContain('executionFailed')
  expect(w.get('[data-testid="line-test-attribution"]').text()).toContain('finalAccount 9')
  expect(w.get('[data-testid="line-test-attribution"]').text()).toContain('priorAttempt 14')
  expect(w.get('[data-account-id="14"]').find('[data-testid="line-pelican-result"]').exists()).toBe(false)
  expect(w.get('[data-testid="line-cache-unknown"]').text()).toContain('noComparable')
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

it('shows the accepted HTTP 429 policy reason without rewriting it as an incorrect answer', () => {
 const s = fixture()
 s.quality.data!.rows = [{ account_id: 9, id: 12, plan_id: 3, measured_model: 'tested-model', observed_at: '2026-11-25T09:00:00Z', probes: 1, passed_probes: 0, expected: 1, outcome: 'degraded', round_complete: true, round_probes: 1, round_expected: 1, quality_429_streak: 3 }]
 const w = render(s)
 expect(w.get('[data-testid="quality-rate-limit-policy"]').text()).toContain('qualityOps.rateLimitDegraded')
 expect(w.get('[data-testid="quality-round-identity"]').text()).toContain('channelOps.evidence.identityUnknown')
 w.unmount()
})
