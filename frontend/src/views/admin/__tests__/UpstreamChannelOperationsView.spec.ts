import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import { readOperations, type OperationsSnapshot } from '@/api/admin/upstreamOperations'
import UpstreamChannelOperationsView from '../UpstreamChannelOperationsView.vue'
import UpstreamTrafficSources from '@/components/admin/operations/UpstreamTrafficSources.vue'
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, values?: Record<string, unknown>) => key.startsWith('upstreamOps.attempts.messages') ? `${key} ${values?.value}` : ['upstreamOps.rawCache.waitSamples','upstreamOps.output.samples'].includes(key) ? `${key} ${values?.samples} ${values?.unknown} ${values?.coverage}` : key === 'upstreamOps.output.distribution' ? `${key} ${values?.mean} ${values?.p95} ${values?.max}` : key === 'upstreamOps.output.partial' ? `${key} ${values?.count} ${values?.max}` : key }) }))
vi.mock('@/api/admin/upstreamOperations', () => ({ readOperations: vi.fn() }))
vi.mock('@/api/admin/procurementCatalog', () => ({ readProcurementCatalog: vi.fn(), saveProcurementCatalog: vi.fn(), previewProcurementScopes: vi.fn(), previewProcurementProductImpact: vi.fn() }))
vi.mock('@/api/admin/groups', () => ({ list: vi.fn() }))

describe('server-recorded traffic source composition', () => {
 const traffic = { version: 1, source: 'server_recorded_entrypoints', samples: 10, known_samples: 2, unknown_samples: 8, coverage: .2,
  rows: [{traffic_source:'gateway_ingress',samples:1},{traffic_source:'account_connection_test',samples:1},{traffic_source:'unknown',samples:8}] }
 it('shows gateway, synthetic and unknown as separate counts with an explicit scope boundary',()=>{
  const w=mount(UpstreamTrafficSources,{props:{traffic:traffic as never,samples:10}})
  expect(w.get('[data-testid="traffic-gateway_ingress"] dd').text()).toBe('1')
  expect(w.get('[data-testid="traffic-account_connection_test"] dd').text()).toBe('1')
  expect(w.get('[data-testid="traffic-unknown"] dd').text()).toBe('8')
  expect(w.text()).toContain('upstreamOps.traffic.scope')
  w.unmount()
 })
 it.each(['missing','future','wrong_source','total_mismatch','counts','duplicate','unknown_source','negative','unsafe','coverage','nonfinite'])('keeps %s composition unknown',mode=>{
  const changed=structuredClone(traffic)
  switch(mode){
   case 'future':changed.version=9;break
   case 'wrong_source':changed.source='client_request_header';break
   case 'total_mismatch':changed.samples=11;break
   case 'counts':changed.known_samples=10;break
   case 'duplicate':changed.rows[2]={traffic_source:'gateway_ingress',samples:8};break
   case 'unknown_source':changed.rows[0].traffic_source='browser_declared';break
   case 'negative':changed.rows[0].samples=-1;break
   case 'unsafe':changed.known_samples=2**53;break
   case 'coverage':changed.coverage=.9;break
   case 'nonfinite':changed.coverage=Infinity;break
  }
  const w=mount(UpstreamTrafficSources,{props:{traffic:mode==='missing'?undefined:changed as never,samples:10}})
  expect(w.get('[data-testid="traffic-gateway_ingress"] dd').text()).toBe('0')
  expect(w.get('[data-testid="traffic-account_connection_test"] dd').text()).toBe('0')
  expect(w.get('[data-testid="traffic-unknown"] dd').text()).toBe('10')
  w.unmount()
 })
 it('does not claim full source coverage for an empty denominator',()=>{
  const w=mount(UpstreamTrafficSources,{props:{traffic:{...traffic,samples:0,known_samples:0,unknown_samples:0,coverage:null,rows:[]},samples:0}})
  expect(w.get('[data-testid="traffic-unknown"] dd').text()).toBe('0')
  expect(w.text()).not.toContain('100.0%')
  w.unmount()
 })
})
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
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'ready',data:{summary,total:1,rows:[{...summary,account_id:42,public_model:'model',transport:'http',waits}]}}} as unknown as OperationsSnapshot)
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
  vi.mocked(readOperations).mockResolvedValue({...base,raw_cache:{status:'ready',data:{summary,total:1,rows:[{...summary,account_id:42,public_model:'model',transport:'http',waits}]}}} as unknown as OperationsSnapshot)
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
 it('separates per-attempt cache coverage, token weighting and final downstream statistics',async()=>{
  const raw={version:1,source:'canonical_physical_attempt_cache_reports',attempts:4,observed_attempts:2,unknown_attempts:2,hit_attempts:1,input_tokens:1000,read_tokens:100,coverage:0.5,attempt_hit_rate:0.5,token_read_rate:0.1,latest_sample_at:'2026-11-16T01:00:00Z'}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,raw_cache:raw,rows:[{...data.rows[0],raw_cache:raw}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  const cache=w.get('[data-testid="attempt-cache-summary"]')
  expect(cache.text()).toContain('upstreamOps.attempts.cacheScope')
  expect(cache.text()).toContain('upstreamOps.rawCache.partial')
  expect(cache.get('[data-testid="attempt-cache-token-rate"]').text()).toBe('10.0%')
  expect(cache.get('[data-testid="attempt-cache-hit-rate"]').text()).toBe('50.0%')
  expect(cache.get('[data-testid="attempt-cache-coverage"]').text()).toBe('50.0%')
  expect(w.findAll('[data-testid="upstream-attempt-cache"]')).toHaveLength(2)
  expect(w.find('[data-testid="upstream-raw-cache"]').exists()).toBe(false)
  w.unmount()
 })
 it.each(['missing','future','wrong-source','count-mismatch','all-unknown','explicit-zero'])('keeps %s attempt-cache evidence distinct from a cache miss',async state=>{
  const raw={version:state==='future'?9:1,source:state==='wrong-source'?'canonical_final_transport_cache_reports':'canonical_physical_attempt_cache_reports',attempts:state==='count-mismatch'?5:4,observed_attempts:state==='all-unknown'?0:2,unknown_attempts:state==='all-unknown'?4:2,hit_attempts:0,input_tokens:state==='all-unknown'?0:1000,read_tokens:0,coverage:state==='all-unknown'?0:0.5,attempt_hit_rate:state==='all-unknown'?null:0,token_read_rate:state==='all-unknown'?null:0,latest_sample_at:null}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,raw_cache:state==='missing'?undefined:raw}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  const cache=w.get('[data-testid="attempt-cache-summary"]')
  expect(cache.get('[data-testid="attempt-cache-token-rate"]').text()).toBe(state==='explicit-zero'?'0.0%':'upstreamOps.unknown')
  expect(cache.text()).toContain(state==='explicit-zero'?'upstreamOps.rawCache.partial':'upstreamOps.rawCache.unknown')
  w.unmount()
 })
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
 it.each(['responses','responses_compact'])('shows exact %s output options separately from model verification',async protocol=>{
  const row={...data.rows[0],upstream_model_status:'observed',upstream_model:'wire-responses',upstream_protocol:protocol,service_tier:'priority',reasoning_effort:'high',upstream_stream:true,reasoning_summary:'concise',encrypted_content:'requested',text_verbosity:'low'}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  const output=w.get('[data-testid="attempt-responses-output"]')
  expect(output.text()).toContain('upstreamOps.attempts.responsesSummaryOptions.concise')
  expect(output.text()).toContain('upstreamOps.attempts.responsesEncryptedOptions.requested')
  expect(output.text()).toContain('upstreamOps.attempts.responsesVerbosityOptions.low')
  expect(w.find('[data-testid="attempt-responses-output-unknown"]').exists()).toBe(false)
  expect(w.text()).toContain('upstreamOps.attempts.supplierScopeUnknown')
  w.unmount()
 })
 it('does not fill historical Responses output options from current defaults',async()=>{
  const row={...data.rows[0],upstream_model_status:'observed',upstream_model:'historical-wire',upstream_protocol:'responses',service_tier:'priority',reasoning_effort:'high',upstream_stream:true}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="attempt-responses-output-unknown"]').text()).toContain('upstreamOps.attempts.responsesOutputUnknown')
  expect(w.find('[data-testid="attempt-responses-output"]').exists()).toBe(false)
  w.unmount()
 })
 it.each([
  {reasoning_summary:'future'},{reasoning_summary:null},{encrypted_content:true},{encrypted_content:undefined},
  {text_verbosity:'future'},{text_verbosity:null},{upstream_protocol:'chat_completions'},
  {upstream_protocol:'messages'},{upstream_protocol:'gemini_generate_content'},
 ])('keeps malformed or cross-protocol Responses options unknown: %j',async patch=>{
  const row={...data.rows[0],upstream_model_status:'observed',upstream_model:'unverified-response',upstream_protocol:'responses',service_tier:'priority',reasoning_effort:'high',upstream_stream:true,reasoning_summary:'auto',encrypted_content:'not_requested',text_verbosity:'medium',...patch}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="attempt-responses-output"]').exists()).toBe(false)
  expect(w.get('[data-testid="attempt-request-scope-unknown"]').text()).toContain('upstreamOps.attempts.physicalScopeUnknown')
  w.unmount()
 })
 it.each(['unspecified','disabled','adaptive','enabled'])('shows Messages %s thinking scope and exact sent speed without implying supplier verification',async thinking=>{
  const row={...data.rows[0],upstream_model_status:'observed',upstream_model:'wire-messages',upstream_protocol:'messages',service_tier:'standard_only',reasoning_effort:'max',upstream_stream:true,speed:'fast',thinking_mode:thinking,thinking_budget_tokens:thinking==='enabled'?4096:null}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="attempt-request-scope"]').text()).toContain('wire-messages')
  expect(w.get('[data-testid="attempt-messages-speed"]').text()).toContain('fast')
  expect(w.get('[data-testid="attempt-messages-thinking"]').text()).toContain(thinking)
  expect(w.find('[data-testid="attempt-messages-budget"]').exists()).toBe(thinking==='enabled')
  if(thinking==='enabled') expect(w.get('[data-testid="attempt-messages-budget"]').text()).toContain('4,096')
  expect(w.get('[data-testid="upstream-attempt-row"]').text()).toContain('upstreamOps.attempts.supplierScopeUnknown')
  w.unmount()
 })
 it.each(['unspecified','budget_dynamic','budget_zero','budget_fixed','level'])('shows Gemini %s with a distinct sent setting',async mode=>{
  const row={...data.rows[0],upstream_model_status:'observed',upstream_model:'wire-gemini',upstream_protocol:'gemini_stream_generate_content',service_tier:'unspecified',reasoning_effort:'unspecified',upstream_stream:true,thinking_mode:mode,include_thoughts:'enabled',thinking_budget_tokens:mode==='budget_fixed'?4096:null,thinking_level:mode==='level'?'HIGH':null}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="attempt-request-scope"]').text()).toContain('wire-gemini')
  expect(w.get('[data-testid="attempt-gemini-thinking"]').text()).toContain(`upstreamOps.attempts.geminiModes.${mode}`)
  expect(w.get('[data-testid="attempt-gemini-thoughts"]').text()).toContain('geminiThoughtOptions.enabled')
  expect(w.find('[data-testid="attempt-gemini-budget"]').exists()).toBe(mode==='budget_fixed')
  expect(w.find('[data-testid="attempt-gemini-level"]').exists()).toBe(mode==='level')
  expect(w.get('[data-testid="upstream-attempt-row"]').text()).toContain('upstreamOps.attempts.supplierScopeUnknown')
  w.unmount()
 })
 it.each([
  {include_thoughts:undefined},{include_thoughts:true},{thinking_mode:'future'},{thinking_mode:'budget_fixed'},
  {thinking_mode:'budget_fixed',thinking_budget_tokens:0},{thinking_mode:'budget_fixed',thinking_budget_tokens:1024.5},
  {thinking_mode:'budget_fixed',thinking_budget_tokens:2**53},{thinking_mode:'level'},{thinking_level:'high'},
  {thinking_mode:'level',thinking_level:'future'},{thinking_budget_tokens:4096},{upstream_stream:false},{speed:'fast'},
  {service_tier:'priority'},{reasoning_effort:'high'}
 ])('keeps malformed Gemini scope %j unknown',async patch=>{
  const row={...data.rows[0],upstream_model_status:'observed',upstream_model:'unverified-wire-gemini',upstream_protocol:'gemini_stream_generate_content',service_tier:'unspecified',reasoning_effort:'unspecified',upstream_stream:true,thinking_mode:'unspecified',include_thoughts:'unspecified',...patch}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="attempt-request-scope"]').exists()).toBe(false)
  expect(w.get('[data-testid="attempt-request-scope-unknown"]').exists()).toBe(true)
  expect(w.text()).not.toContain('unverified-wire-gemini')
  w.unmount()
 })
 it.each([
  {speed:undefined},{speed:'turbo'},{thinking_mode:undefined},{thinking_mode:'future'},
  {thinking_mode:'enabled'},{thinking_mode:'enabled',thinking_budget_tokens:0},
  {thinking_mode:'enabled',thinking_budget_tokens:1024.5},{thinking_mode:'enabled',thinking_budget_tokens:2**53},
  {thinking_budget_tokens:4096},{service_tier:'priority'},{reasoning_effort:'none'}
 ])('keeps invalid Messages scope %j unknown rather than showing a misleading default',async patch=>{
  const row={...data.rows[0],upstream_model_status:'observed',upstream_model:'unverified-wire-messages',upstream_protocol:'messages',service_tier:'standard_only',reasoning_effort:'high',upstream_stream:true,speed:'standard',thinking_mode:'adaptive',...patch}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="attempt-request-scope"]').exists()).toBe(false)
  expect(w.get('[data-testid="attempt-request-scope-unknown"]').exists()).toBe(true)
  expect(w.text()).not.toContain('unverified-wire-messages')
  w.unmount()
 })
 it.each(['not_observed',undefined])('keeps %s scope unknown even if a stale model label exists',async status=>{
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[{...data.rows[0],upstream_model_status:status,upstream_model:'unverified-wire-model'}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="attempt-request-scope"]').exists()).toBe(false)
  expect(w.text()).not.toContain('unverified-wire-model')
  w.unmount()
 })
 it('shows all-attempt HTTP waits with measured zero and independent unknown stages',async()=>{
  const known={samples:1,unknown_samples:6,mean_ms:0,coverage:1/7,latest_sample_at:'2026-11-16T12:00:00Z'}
  const unknown={samples:0,unknown_samples:7,mean_ms:null,coverage:0,latest_sample_at:null}
  const row={...data.rows[0],waits:{version:1,source:'canonical_physical_http_attempt',first_byte:known,first_event:known,first_reasoning:unknown,first_text:unknown}}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="upstream-attempt-waits"]').text()).toContain('upstreamOps.attempts.waitScope')
  expect(w.get('[data-testid="upstream-attempt-wait-first_byte"]').text()).toContain('0.0 ms')
  expect(w.get('[data-testid="upstream-attempt-wait-first_byte"]').text()).toContain('upstreamOps.rawCache.partial')
  expect(w.get('[data-testid="upstream-attempt-wait-first_text"]').text()).toContain('upstreamOps.unknown')
  expect(w.get('[data-testid="upstream-attempt-wait-first_text"]').text()).not.toContain('0.0 ms')
  w.unmount()
 })
 it('keeps WS generation waits separate and excludes HTTP first-byte evidence',async()=>{
  const known={samples:1,unknown_samples:0,mean_ms:40,coverage:1,latest_sample_at:'2026-11-16T12:00:00Z'}
  const row={...data.rows[0],transport:'ws_turn',waits:{version:1,source:'canonical_physical_ws_turn_generation_start',first_byte:known,first_event:known,first_reasoning:known,first_text:known}}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[row]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.get('[data-testid="upstream-attempt-waits"]').text()).toContain('upstreamOps.attempts.wsWaitScope')
  expect(w.get('[data-testid="upstream-attempt-wait-first_event"]').text()).toContain('upstreamOps.rawCache.wsFirstEvent')
  expect(w.find('[data-testid="upstream-attempt-wait-first_byte"]').exists()).toBe(false)
  w.unmount()
 })
 it.each([{version:9,source:'canonical_physical_http_attempt'},{version:1,source:'canonical_final_physical_http_attempt'},{version:1,source:'future_source'}])('rejects unsupported attempt wait evidence %j',async waits=>{
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[{...data.rows[0],waits}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="upstream-attempt-waits"]').exists()).toBe(false)
  w.unmount()
 })
 it.each([undefined,null,'unknown_transport'])('keeps missing or unsupported transport %s unknown',async transport=>{
  const metric={samples:1,unknown_samples:0,mean_ms:0,coverage:1,latest_sample_at:null}
  const waits={version:1,source:'canonical_physical_http_attempt',first_byte:metric,first_event:metric,first_reasoning:metric,first_text:metric}
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{...data,rows:[{...data.rows[0],transport,waits}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises()
  expect(w.find('[data-testid="upstream-attempt-waits"]').exists()).toBe(false)
  expect(w.find('[data-testid="upstream-attempt-row"]').exists()).toBe(true)
  w.unmount()
 })
})


describe('per-attempt output diagnostics', () => {
 const metric={samples:2,unknown_samples:2,partial_samples:1,coverage:0.5,mean_ms:300,p95_ms:390,max_ms:400,partial_max_ms:600,latest_sample_at:'2026-11-16T01:00:00Z',latest_partial_at:'2026-11-16T01:00:00Z'}
 const output={version:1,source:'canonical_physical_attempt_output',attempts:4,max_gap:metric,tail_wait:{...metric,mean_ms:150,p95_ms:195,max_ms:200,partial_max_ms:100},rate:{samples:2,unknown_samples:2,coverage:0.5,tokens_per_second:200/1.2,non_reasoning_tokens:200,window_ms:1200,latest_sample_at:'2026-11-16T01:00:00Z'}}
 const summary={attempts:4,logical_requests:4,succeeded:2,failed:1,interrupted:0,unknown:1,unattributed_attempts:0,confirmed_success_rate:2/3,outcome_coverage:0.75,attempts_per_request:1,latest_sample_at:null}
 const base={scope:{},usage:{summary:{},rows:[],accounts:[],members:[],total:0,catalog:[]},quality:{status:'unsupported_scope'},errors:{status:'query_failed'}}
 async function render(value: unknown=output,transport='http') {
  vi.mocked(readOperations).mockResolvedValue({...base,attempts:{status:'ready',data:{version:1,source:'retained_physical_transport_attempts',summary,capture:{traces:4,complete_traces:4,unknown_traces:0,coverage:1},total:1,rows:[{...summary,account_id:42,transport,protocol:'responses',public_model:'alias',output:value}]}}} as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView);await flushPromises();return w
 }
 it.each(['http','ws_turn'])('shows %s complete distributions and separate partial lower bounds with weighted throughput',async transport=>{
  const w=await render(output,transport)
  const gap=w.get('[data-testid="attempt-output-max_gap"]')
  expect(gap.text()).toContain('300.0 ms 390.0 ms 400.0 ms')
  expect(gap.text()).toContain('upstreamOps.rawCache.partial')
  expect(gap.text()).toContain('2 2 50.0%')
  expect(gap.get('[data-testid="output-partial"]').text()).toContain('1 600.0 ms')
  expect(w.get('[data-testid="attempt-output-tail_wait"]').text()).toContain('150.0 ms 195.0 ms 200.0 ms')
  expect(w.get('[data-testid="output-rate-value"]').text()).toContain('166.7 Token/s')
  expect(w.get('[data-testid="upstream-attempt-output"]').text()).toContain('upstreamOps.output.scope')
  expect(w.get('[data-testid="attempt-output-rate"]').text()).toContain('upstreamOps.output.rateScope')
  expect(w.get('[data-testid="attempt-count-failed"]').text()).toBe('1')
  w.unmount()
 })
 it.each(['missing','future','source','attempt_count','negative','nonfinite','counts','coverage','rate_window','rate_mismatch','rate_fractional','rate_unsafe'])('keeps invalid %s evidence unknown',async mode=>{
  const changed=structuredClone(output)
  switch(mode){
   case 'future':changed.version=9;break
   case 'source':changed.source='legacy_tps';break
   case 'attempt_count':changed.attempts=5;break
   case 'negative':changed.max_gap.mean_ms=-1;break
   case 'nonfinite':changed.max_gap.max_ms=Infinity;break
   case 'counts':changed.max_gap.samples=9;break
   case 'coverage':changed.max_gap.coverage=0.9;break
   case 'rate_window':changed.rate.window_ms=0;break
   case 'rate_mismatch':changed.rate.tokens_per_second=999;break
   case 'rate_fractional':changed.rate.non_reasoning_tokens=200.5;changed.rate.tokens_per_second=200.5/1.2;break
   case 'rate_unsafe':changed.rate.non_reasoning_tokens=2**53;changed.rate.tokens_per_second=2**53/1.2;break
  }
  const w=await render(mode==='missing'?null:changed)
  if(mode.startsWith('rate'))expect(w.get('[data-testid="output-rate-value"]').text()).toContain('upstreamOps.unknown')
  else expect(w.get('[data-testid="attempt-output-max_gap"]').get('[data-testid="output-distribution"]').text()).toContain('upstreamOps.unknown')
  w.unmount()
 })
 it('separates a measured zero from unknown and keeps partial samples outside complete coverage',async()=>{
  const changed=structuredClone(output)
  changed.max_gap={...metric,mean_ms:0,p95_ms:0,max_ms:0}
  const w=await render(changed)
  expect(w.get('[data-testid="attempt-output-max_gap"]').text()).toContain('0.0 ms 0.0 ms 0.0 ms')
  expect(w.get('[data-testid="attempt-output-max_gap"]').text()).toContain('2 2 50.0%')
  w.unmount()
 })
})
