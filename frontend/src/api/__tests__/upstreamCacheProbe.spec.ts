import { afterEach, describe, expect, it, vi } from 'vitest'
import { readUpstreamCacheProbeRun, runUpstreamCacheProbe, validUpstreamCacheProbeInput, type UpstreamCacheProbeRun } from '@/api/admin/upstreamCacheProbe'

import { upstreamCacheResult } from './upstreamCacheProbe.fixture'

afterEach(() => { vi.unstubAllGlobals(); localStorage.clear() })
describe('upstream cache probe contract', () => {
  it('accepts canonical observations without inventing eligibility or retention', () => {
    const run = upstreamCacheResult()
    expect(readUpstreamCacheProbeRun(run)).toEqual(run)
    expect(validUpstreamCacheProbeInput(run.settings)).toBe(true)
  })
  it.each([
    (r: UpstreamCacheProbeRun) => { r.version = 99 as 1 },
    (r: UpstreamCacheProbeRun) => { r.model_identity = 'verified' },
    (r: UpstreamCacheProbeRun) => { r.cache_domain = 'shared' },
    (r: UpstreamCacheProbeRun) => { r.upstream_group = 'claimed' },
    (r: UpstreamCacheProbeRun) => { r.token_hit_rate = 1 },
    (r: UpstreamCacheProbeRun) => { r.request_hit_rate = 0 },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.cache!.read_tokens = 2001 },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.cache!.read_tokens = -1 },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.cache!.hit = false },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.cache!.status = 'not_reported' },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.scope = null },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.scope!.stream = false },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.scope!.model = 'other-model' },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.scope!.reasoning_effort = 'high' },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.status = 'failed' },
    (r: UpstreamCacheProbeRun) => { r.observed_warm_rounds = 1 },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.interval_ms = 0 },
    (r: UpstreamCacheProbeRun) => { r.finished_at = null },
    (r: UpstreamCacheProbeRun) => { r.cold_baseline = 'cold_not_confirmed' },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.cache!.source = 'estimated_billing' },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.cache!.scope = 'downstream_key' },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.output_tokens = 1.5 },
    (r: UpstreamCacheProbeRun) => { r.rounds[1]!.finished_at = null },
    (r: UpstreamCacheProbeRun) => { r.finished_at = '2000-01-01T00:00:00Z' },
    (r: UpstreamCacheProbeRun) => { (r as unknown as Record<string, unknown>).api_key = 'redacted-fixture' },
  ])('rejects contradictory or untrusted green results', mutate => {
    const result = upstreamCacheResult(); mutate(result)
    expect(() => readUpstreamCacheProbeRun(result)).toThrow('INVALID_UPSTREAM_CACHE_PROBE_RESULT')
  })
  it('keeps absent usage unknown and preserves expected warm coverage', () => {
    const result = upstreamCacheResult()
    Object.assign(result, { status: 'unknown', verdict: 'insufficient', reason: 'usage_not_observed', observed_warm_rounds: 0, request_hit_rate: null, token_hit_rate: null, cold_baseline: 'not_observed', route_continuity: 'not_observed' })
    result.rounds = [{ ...result.rounds[0]!, status: 'unknown', cache: null }]
    expect(readUpstreamCacheProbeRun(result).planned_warm_rounds).toBe(2)
  })
  it('accepts running and budget-deferred snapshots without inventing aggregate hit rates', () => {
    const result = upstreamCacheResult()
    Object.assign(result, { status: 'running', verdict: 'running', reason: 'collecting', finished_at: null, observed_warm_rounds: 0, request_hit_rate: null, token_hit_rate: null, cold_baseline: 'not_observed' })
    result.rounds = result.rounds.slice(0, 1)
    expect(readUpstreamCacheProbeRun(result).status).toBe('running')
    Object.assign(result, { status: 'deferred', verdict: 'insufficient', reason: 'budget_deferred', finished_at: result.started_at, cold_baseline: 'zero_read_observed', budget_deferral: { reason: 'token_limit', scope: 'key_minute', retry_at: result.started_at } })
    result.rounds.push({ ...result.rounds[0]!, index: 2, interval_ms: 1000, status: 'deferred', reason: 'budget_deferred', scope: null, cache: null })
    expect(readUpstreamCacheProbeRun(result).verdict).toBe('insufficient')
  })
  it('binds SSE progress to the administrator, selected account and settings', async () => {
    localStorage.setItem('auth_token', 'admin-fixture')
    const result = upstreamCacheResult(), progress = vi.fn()
    const body = ': heartbeat\r\n\r\ndata: ' + JSON.stringify({ type: 'cache_probe', data: result }) + '\r\n\r\n'
    const mock = vi.fn().mockResolvedValue(new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
    vi.stubGlobal('fetch', mock)
    await runUpstreamCacheProbe(7, result.settings, new AbortController().signal, progress)
    expect(progress).toHaveBeenCalledWith(result)
    expect(mock).toHaveBeenCalledWith(expect.stringContaining('/admin/accounts/7/cache-probe'), expect.objectContaining({ credentials: 'include', cache: 'no-store', redirect: 'error', headers: expect.objectContaining({ Authorization: 'Bearer admin-fixture' }), body: JSON.stringify(result.settings) }))
    expect(JSON.stringify(result)).not.toContain('admin-fixture')
  })
  it.each(['missing-token', 'wrong-account', 'unknown-version', 'truncated', 'http'])('rejects %s streams', async scenario => {
    const result = upstreamCacheResult()
    if (scenario !== 'missing-token') localStorage.setItem('auth_token', 'admin-fixture')
    if (scenario === 'wrong-account') result.account_id = 8
    if (scenario === 'unknown-version') result.version = 2 as 1
    const body = scenario === 'truncated' ? ': heartbeat\n\n' : 'data: ' + JSON.stringify({ type: 'cache_probe', data: result }) + '\n\n'
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(body, { status: scenario === 'http' ? 503 : 200, headers: { 'Content-Type': 'text/event-stream' } })))
    await expect(runUpstreamCacheProbe(7, result.settings, new AbortController().signal, vi.fn())).rejects.toThrow()
  })
})
