import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import { readOperations, type OperationsSnapshot } from '@/api/admin/upstreamOperations'
import UpstreamChannelOperationsView from '../UpstreamChannelOperationsView.vue'
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/upstreamOperations', () => ({ readOperations: vi.fn() }))
describe('upstream channel definition', () => {
 it.each([
  { complete: true, outcome: 'passed', expected: 'channelOps.passed' },
  { complete: true, outcome: 'degraded', expected: 'channelOps.degraded' },
  { complete: false, outcome: 'passed', expected: 'channelOps.inconclusive' }
 ])('shows bounded quality evidence for $outcome, complete=$complete without claiming model identity', async ({complete, outcome, expected}) => {
  vi.mocked(readOperations).mockResolvedValue({ scope: {}, usage: { summary: {}, rows: [], accounts: [], members: [], total: 0, catalog: [] }, quality: {status:'ready',queried_at:'2026-11-13T12:00:00Z', data: {summary:{rounds:1,passed:0,degraded:0,inconclusive:1},rows:[{id:1,account_id:42,plan_id:8,measured_model:'fixture-model',observed_at:'2026-11-13T11:55:00Z',probes:2,passed_probes:outcome==='degraded'?1:2,expected:2,outcome,round_complete:complete,round_probes:2,round_expected:complete?2:4,actions:[]}]}},errors:{status:'query_failed'} } as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView); await flushPromises()
  expect(w.get('[data-testid="quality-round-outcome"]').text()).toBe(expected)
  expect(w.get('[data-testid="quality-round-identity"]').text()).toContain('channelOps.evidence.identityUnknown')
  expect(w.get('[data-testid="quality-round-coverage"]').text()).toContain(complete?'100%':'50%')
  expect(w.get('[data-testid="quality-round-evidence"]').text()).toContain('channelOps.evidence.historical')
  expect(w.get('a[href="/admin/account-quality?plan_id=8"]').exists()).toBe(true)
  w.unmount()
 })
 it('discovers URLs without creating or fetching model-pricing channels, including idle accounts', async () => {
  vi.mocked(readOperations).mockResolvedValue({ scope: {}, usage: { summary: {}, rows: [], accounts: [], members: [{id:42,name:'Idle account'}],member_total:1, total: 0, catalog_total: 1, catalog: [{key:'https://idle.example:8443/tenant/v1',name:'https://idle.example:8443/tenant/v1',accounts:2}] }, quality: {status:'unsupported_scope'}, errors:{status:'query_failed'} } as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView); await flushPromises()
  expect(w.text()).toContain('upstreamOps.title'); expect(w.text()).toContain('https://idle.example:8443/tenant/v1')
  expect(w.text()).toContain('upstreamOps.latencyNote'); expect(w.text()).toContain('upstreamOps.qualityNote'); expect(w.text()).toContain('upstreamOps.errorNote')
  expect(readOperations).toHaveBeenCalledWith(expect.objectContaining({dimension:'upstream'}),expect.anything())
  expect(w.find('[data-testid="new-channel"]').exists()).toBe(false)
  const idle=w.findAll('button').find(b=>b.text().includes('idle.example'))!;await idle.trigger('click');await flushPromises()
  expect(readOperations).toHaveBeenLastCalledWith(expect.objectContaining({upstream:'https://idle.example:8443/tenant/v1',account_id:0,dimension:'account'}),expect.anything())
  await w.get('[data-testid="member-drill"]').trigger('click');await flushPromises()
  expect(readOperations).toHaveBeenLastCalledWith(expect.objectContaining({account_id:42,dimension:'model'}),expect.anything())
  w.unmount()
 })
})


describe('upstream raw cache monitoring', () => {
 const base = { scope: {}, usage: { summary: {}, rows: [], accounts: [], members: [], total: 0, catalog: [] }, quality: {status:'unsupported_scope'}, errors:{status:'query_failed'} }
 const summary = { requests:4, observed_requests:2, unknown_requests:2,unattributed_requests:1,hit_requests:1,input_tokens:1000,read_tokens:100,coverage:0.5,request_hit_rate:0.5,token_read_rate:0.1,latest_sample_at:'2026-11-15T00:00:00Z' }
 it('separates weighted raw token, request hit and coverage from historical billing', async () => {
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'ready',data:{summary,total:1,retention_days:30,rows:[{...summary,account_id:42,credential_fingerprint:'a'.repeat(64),route_fingerprint:'b'.repeat(64),public_model:'requested',upstream_model:'wire-model',protocol:'responses',transport:'http',upstream_group_status:'not_observed',service_tier:'priority',reasoning_effort:'high'}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="upstream-cache-token-rate"]').text()).toBe('10.0%')
  expect(w.get('[data-testid="upstream-cache-request-rate"]').text()).toBe('50.0%')
  expect(w.get('[data-testid="upstream-cache-coverage"]').text()).toBe('50.0%')
  expect(w.get('[data-testid="upstream-cache-status"]').text()).toContain('upstreamOps.rawCache.partial')
  expect(w.get('[data-testid="upstream-cache-row"]').text()).toContain('aaaaaaaaaaaa')
  expect(w.get('[data-testid="upstream-cache-row"]').text()).toContain('wire-model')
  expect(w.get('[data-testid="upstream-cache-row"]').text()).toContain('upstreamOps.rawCache.groupUnknown')
  w.unmount()
 })
 it('keeps missing ratios unknown, including explicit zero observation coverage', async () => {
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'ready',data:{summary:{...summary,observed_requests:0,coverage:0,token_read_rate:null,request_hit_rate:null},total:0,rows:[]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="upstream-cache-token-rate"]').text()).toBe('upstreamOps.unknown')
  expect(w.get('[data-testid="upstream-cache-coverage"]').text()).toBe('0.0%')
  expect(w.get('[data-testid="upstream-cache-status"]').text()).toContain('upstreamOps.rawCache.unknown')
  w.unmount()
 })
 it('shows query failure rather than successful zeroes', async () => {
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'query_failed',data:null}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="upstream-raw-cache"]').text()).toContain('upstreamOps.queryFailed')
  expect(w.find('[data-testid="upstream-cache-token-rate"]').exists()).toBe(false)
  w.unmount()
 })
})
