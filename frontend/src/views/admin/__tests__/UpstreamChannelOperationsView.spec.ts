import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import { readOperations, type OperationsSnapshot } from '@/api/admin/upstreamOperations'
import UpstreamChannelOperationsView from '../UpstreamChannelOperationsView.vue'
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, values?: Record<string, unknown>) => key === 'upstreamOps.rawCache.waitSamples' ? `${key} ${values?.samples} ${values?.unknown} ${values?.coverage}` : key }) }))
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
 it('shows per-scope phase means, independent coverage and a measured zero without using legacy TTFT', async () => {
  const metric=(mean: number|null,samples: number)=>({mean_ms:mean,samples,unknown_samples:4-samples,coverage:samples/4,latest_sample_at:null})
  const waits={version:1,source:'canonical_final_physical_http_attempt',first_byte:metric(20,4),first_event:metric(30,3),first_reasoning:metric(null,0),first_text:metric(0,2)}
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'ready',data:{summary,total:1,rows:[{...summary,account_id:42,public_model:'model',waits}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="upstream-wait-first_byte"]').text()).toContain('20.0 ms')
  expect(w.get('[data-testid="upstream-wait-first_byte"]').text()).toContain('upstreamOps.rawCache.waitObserved')
  expect(w.get('[data-testid="upstream-wait-first_event"]').text()).toContain('upstreamOps.rawCache.partial')
  expect(w.get('[data-testid="upstream-wait-first_event"]').text()).toContain('3 1 75.0%')
  expect(w.get('[data-testid="upstream-wait-first_reasoning"]').text()).toContain('upstreamOps.unknown')
  expect(w.get('[data-testid="upstream-wait-first_text"]').text()).toContain('0.0 ms')
  expect(w.get('[data-testid="upstream-waits"]').text()).toContain('upstreamOps.rawCache.waitScope')
  w.unmount()
 })
 it('shows WS generation waits without fabricating HTTP first-byte or SSE timing', async () => {
  const metric=(mean: number|null,samples: number)=>({mean_ms:mean,samples,unknown_samples:4-samples,coverage:samples/4,latest_sample_at:null})
  const waits={version:1,source:'canonical_physical_ws_turn_generation_start',first_byte:metric(1,4),first_event:metric(0,2),first_reasoning:metric(null,0),first_text:metric(30,3)}
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'ready',data:{summary,total:1,rows:[{...summary,account_id:42,public_model:'model',transport:'ws_turn',waits}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="upstream-wait-first_byte"]').exists()).toBe(false)
  expect(w.get('[data-testid="upstream-wait-first_event"]').text()).toContain('0.0 ms')
  expect(w.get('[data-testid="upstream-wait-first_event"]').text()).toContain('upstreamOps.rawCache.wsFirstEvent')
  expect(w.get('[data-testid="upstream-wait-first_event"]').text()).toContain('2 2 50.0%')
  expect(w.get('[data-testid="upstream-wait-first_reasoning"]').text()).toContain('upstreamOps.unknown')
  expect(w.get('[data-testid="upstream-wait-first_text"]').text()).toContain('30.0 ms')
  expect(w.get('[data-testid="upstream-waits"]').text()).toContain('upstreamOps.rawCache.wsWaitScope')
  w.unmount()
 })
 it.each([
  ['http','canonical_physical_ws_turn_generation_start'],
  ['ws_turn','canonical_final_physical_http_attempt'],
  ['ws_turn','legacy_ttft'],
 ])('rejects mismatched transport %s and source %s', async (transport,source) => {
  const metric={mean_ms:20,samples:1,unknown_samples:0,coverage:1,latest_sample_at:null}
  const waits={version:1,source,first_byte:metric,first_event:metric,first_reasoning:metric,first_text:metric}
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'ready',data:{summary,total:1,rows:[{...summary,account_id:42,public_model:'model',transport,waits}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="upstream-waits"]').exists()).toBe(false)
  w.unmount()
 })
 it.each(['future','wrong source','negative','no samples','nonfinite'])('keeps %s waits unknown rather than fast or healthy',async state=>{
  const metric={mean_ms:20,samples:1,unknown_samples:0,coverage:1,latest_sample_at:null}
  const waits={version:state==='future'?9:1,source:state==='wrong source'?'legacy_ttft':'canonical_final_physical_http_attempt',first_byte:metric,first_event:metric,first_reasoning:metric,first_text:{...metric,mean_ms:state==='negative'?-1:state==='nonfinite'?Infinity:20,samples:state==='no samples'?0:1}}
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'ready',data:{summary,total:1,rows:[{...summary,account_id:42,public_model:'model',waits}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  if(state==='future'||state==='wrong source') expect(w.find('[data-testid="upstream-waits"]').exists()).toBe(false)
  else expect(w.get('[data-testid="upstream-wait-first_text"]').text()).toContain('upstreamOps.unknown')
  w.unmount()
 })
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


describe('all retained upstream attempts', () => {
 const base = { scope: {}, usage: { summary: {}, rows: [], accounts: [], members: [], total: 0, catalog: [] }, quality: {status:'unsupported_scope'}, errors:{status:'query_failed'} }
 const summary={attempts:4,logical_requests:2,succeeded:1,failed:2,interrupted:0,unknown:1,unattributed_attempts:1,confirmed_success_rate:1/3,outcome_coverage:0.75,attempts_per_request:2,latest_sample_at:null}
 const data={version:1,source:'retained_physical_transport_attempts',summary,capture:{traces:3,complete_traces:2,unknown_traces:1,coverage:2/3},total:41,rows:[{...summary,account_id:42,credential_fingerprint:'a'.repeat(64),route_fingerprint:'b'.repeat(64),public_model:'request-alias',transport:'http',protocol:'responses'}]}
 it('shows earlier failures and retry amplification separately from the final cache observation',async()=>{
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="attempt-count-failed"]').text()).toBe('2')
  expect(w.get('[data-testid="attempt-count-succeeded"]').text()).toBe('1')
  expect(w.get('[data-testid="attempt-count-unknown"]').text()).toBe('1')
  expect(w.get('[data-testid="attempt-success-rate"]').text()).toBe('33.3%')
  expect(w.get('[data-testid="upstream-attempt-row"]').text()).toContain('request-alias')
  expect(w.get('[data-testid="upstream-attempt-row"]').text()).toContain('upstreamOps.attempts.physicalScopeUnknown')
  expect(w.find('[data-testid="upstream-raw-cache"]').exists()).toBe(false)
  expect(w.get('[data-testid="operations-next"]').attributes('disabled')).toBeUndefined()
  await w.get('[data-testid="operations-next"]').trigger('click');await flushPromises()
  expect(readOperations).toHaveBeenLastCalledWith(expect.objectContaining({page:2}),expect.anything())
  w.unmount()
 })
 it.each(['query_failed','unsupported_scope','future','wrong_source'])('preserves %s as missing evidence',async state=>{
  const response={status:state==='query_failed'||state==='unsupported_scope'?state:'ready',data:state==='query_failed'||state==='unsupported_scope'?null:{...data,version:state==='future'?9:1,source:state==='wrong_source'?'other':data.source}}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:response} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="attempt-success-rate"]').exists()).toBe(false)
  expect(w.get('[data-testid="upstream-attempts"]').text()).toContain(state==='unsupported_scope'?'upstreamOps.attempts.unsupported':'upstreamOps.queryFailed')
  w.unmount()
 })
 it('keeps an empty capture distinct from a perfect success rate',async()=>{
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,summary:{...summary,attempts:0,confirmed_success_rate:null,outcome_coverage:null,attempts_per_request:null},total:0,rows:[]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="attempt-success-rate"]').text()).toBe('upstreamOps.unknown')
  expect(w.get('[data-testid="attempt-empty"]').text()).toContain('upstreamOps.attempts.empty')
  w.unmount()
 })
 it('shows sent model and tier while keeping unknown attempts in cohort coverage',async()=>{
  const requestScope={attempts:7,observed_attempts:3,unknown_attempts:4,coverage:3/7}
  const observed={...data.rows[0],upstream_model_status:'observed',upstream_model:'actual-wire-model',upstream_protocol:'responses',service_tier:'priority',reasoning_effort:'high',upstream_stream:true}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,request_scope:requestScope,rows:[observed,data.rows[0]]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="attempt-request-scope"]').text()).toContain('actual-wire-model')
  expect(w.get('[data-testid="attempt-request-scope"]').text()).toContain('priority')
  expect(w.get('[data-testid="attempt-request-scope"]').text()).toContain('high')
  expect(w.get('[data-testid="attempt-request-scope"]').text()).toContain('upstreamOps.attempts.requestMetadataObserved')
  expect(w.get('[data-testid="attempt-request-scope-coverage"]').text()).toContain('upstreamOps.attempts.requestScopeCoverage')
  expect(w.get('[data-testid="attempt-request-scope-unknown"]').text()).toContain('upstreamOps.attempts.physicalScopeUnknown')
  expect(w.findAll('[data-testid="upstream-attempt-row"]')).toHaveLength(2)
  expect(w.get('[data-testid="upstream-attempt-row"]').text()).toContain('upstreamOps.attempts.supplierScopeUnknown')
  w.unmount()
 })
 it.each(['not_observed',undefined])('keeps %s scope unknown even if a stale model label exists',async status=>{
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[{...data.rows[0],upstream_model_status:status,upstream_model:'unverified-wire-model'}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="attempt-request-scope"]').exists()).toBe(false)
  expect(w.text()).not.toContain('unverified-wire-model')
  w.unmount()
 })
})
