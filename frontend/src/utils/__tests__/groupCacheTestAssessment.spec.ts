import { describe, expect, it } from 'vitest'
import { aggregateCacheTokens, assessCacheRun, cacheHitRate, DEFAULT_CACHE_SETTINGS, type CacheRound, type CacheTestRun } from '../groupCacheTest'

const round = (index: number, read: number | null = 80): CacheRound => ({
  index, status: 'completed', startedAt: '2026-11-13T00:00:00Z', requestId: `request-${index}`,
  tokens: { input: 100, read, write: null, output: 2 }, responseModel: 'fixture-model', serviceTier: null,
  firstTextMs: 10, durationMs: 20, error: null, httpStatus: 200, bill: null, billingState: 'pending'
})
const run = (): CacheTestRun => ({
  version: 1, id: 'fixture-run', startedAt: '2026-11-13T00:00:00Z', finishedAt: '2026-11-13T00:01:00Z',
  groupId: 7, groupName: 'Fixture', keyId: 10, keyName: 'Owned key',
  settings: { ...DEFAULT_CACHE_SETTINGS, model: 'fixture-model' }, status: 'completed',
  rounds: [round(1, 0), round(2), round(3)]
})

describe('cache experiment conclusions', () => {
  it('separates request hits from token-weighted hits and excludes the baseline', () => {
    const test = run()
    test.rounds[1].tokens = { input: 100, read: 100, write: 0, output: 2 }
    test.rounds[2].tokens = { input: 900, read: 0, write: 0, output: 2 }
    expect(assessCacheRun(test)).toMatchObject({
      verdict: 'hit', observed: 2, completed: 2, expected: 2, hits: 1,
      coverage: 1, requestHitRate: 0.5, tokenHitRate: 0.1, affinity: 'unknown'
    })
  })

  it('treats an explicit zero as a measured miss', () => {
    const test = run(); test.rounds.slice(1).forEach(r => { r.tokens!.read = 0 })
    expect(assessCacheRun(test)).toMatchObject({ verdict: 'miss', coverage: 1, requestHitRate: 0, tokenHitRate: 0 })
  })

  it('does not turn missing telemetry into a miss or a complete pass', () => {
    const test = run(); test.rounds[2].tokens!.read = null
    expect(assessCacheRun(test)).toMatchObject({ verdict: 'partial', observed: 1, completed: 2, coverage: 0.5 })
    test.rounds[1].tokens!.read = null
    expect(assessCacheRun(test)).toMatchObject({ verdict: 'insufficient', observed: 0, tokenHitRate: null, requestHitRate: null })
  })

  it.each(['running', 'failed', 'stopped'] as const)('keeps %s explicit even after a hit', status => {
    const test = run(); test.status = status
    expect(assessCacheRun(test).verdict).toBe(status)
  })

  it('does not count failed attempt telemetry as a successful repeat', () => {
    const test = run(); test.rounds[2].status = 'failed'
    expect(assessCacheRun(test)).toMatchObject({ verdict: 'partial', observed: 1, completed: 1, expected: 2 })
  })

  it('does not mark an incomplete or duplicate-index history as complete', () => {
    const test = run(); test.rounds.pop()
    expect(assessCacheRun(test).verdict).toBe('partial')
    test.rounds.push(round(2))
    expect(assessCacheRun(test).verdict).toBe('insufficient')
  })

  it('keeps account consistency separate from cache-domain proof', () => {
    const test = run()
    for (const r of test.rounds) {
      r.billingState = 'matched'
      r.bill = { id: r.index, requestId: r.requestId!, groupId: 7, accountId: 4, model: 'fixture-model' } as NonNullable<CacheRound['bill']>
    }
    expect(assessCacheRun(test).affinity).toBe('same_account')
    test.rounds[2].bill!.accountId = 5
    expect(assessCacheRun(test).affinity).toBe('changed_account')
    test.rounds[2].bill!.accountId = null
    expect(assessCacheRun(test).affinity).toBe('unknown')
    test.rounds[2].bill!.accountId = 4; test.rounds[2].bill!.groupId = 8
    expect(assessCacheRun(test).affinity).toBe('unknown')
  })

  it.each([-1, Number.NaN, Number.POSITIVE_INFINITY, 0.5, Number.MAX_SAFE_INTEGER + 1])('rejects malformed cache counts %s', read => {
    expect(cacheHitRate({ input: 100, read, write: 0, output: 2 })).toBeNull()
  })

  it('rejects overlapping read/write buckets and preserves missing aggregate fields', () => {
    expect(cacheHitRate({ input: 100, read: 80, write: 30, output: 2 })).toBeNull()
    expect(aggregateCacheTokens([round(2)])).toMatchObject({ write: null, read: 80 })
  })
})
