import { describe, expect, it } from 'vitest'
import type { GroupProcurementPolicy } from '@/types'
import { parseProcurementSourceIDs, procurementPolicyCommand, validateProcurementPolicy } from '@/utils/procurementPolicy'

const policy = (min_rate: string | null = '0.02', max_rate: string | null = '0.03'): GroupProcurementPolicy =>
  ({ version: 1, revision: 1, min_rate, max_rate })

describe('exact procurement interval commands', () => {
  it('preserves an explicit version 4 answer quality requirement', () => {
    for (const quality of ['not_required', 'current_answer']) {
      const selected = { ...policy(), version: 4, sources: { account_ids: [901] }, buckets: { codes: ['low'] }, quality }
      expect(validateProcurementPolicy(selected)).toBeNull()
      expect(procurementPolicyCommand(selected, selected)).toBeUndefined()
      expect(procurementPolicyCommand({ ...selected, quality: quality === 'not_required' ? 'current_answer' : 'not_required' }, selected)).toBeDefined()
    }
    for (const quality of [undefined, '', 'future']) {
      expect(validateProcurementPolicy({ ...policy(), version: 4, sources: { account_ids: null }, buckets: { codes: null }, quality })).toBe('qualityFormat')
    }
    expect(validateProcurementPolicy({ ...policy(), quality: 'current_answer' })).toBe('qualityFormat')
  })
  it('preserves bucket selections, empty and unrestricted modes in complete version 3 commands', () => {
    for (const codes of [null, [], ['low', 'standard']]) {
      const selected = { ...policy(), version: 3, sources: { account_ids: null }, buckets: { codes } }
      expect(validateProcurementPolicy(selected)).toBeNull()
      expect(procurementPolicyCommand(selected, selected)).toBeUndefined()
      expect(procurementPolicyCommand(selected, policy())).toEqual(selected)
    }
    for (const codes of [['low', 'low'], ['Bad Code'], [''], Array.from({ length: 65 }, (_, i) => `bucket-${i}`)]) {
      expect(validateProcurementPolicy({ ...policy(), version: 3, sources: { account_ids: null }, buckets: { codes } })).toBe('bucketFormat')
    }
  })
  it.each(['', '-0.01', '+0.01', '.02', '00.02', '0.123456789', '100000000', '1e-2', 'NaN', ' 0.02'])('rejects %s without rounding', value => {
    expect(validateProcurementPolicy(policy(value))).toBe('format')
  })
  it.each([
    ['0.02', '0.03'], ['0.02000000', '0.02000001'], ['99999999.99999998', '99999999.99999999'],
    [null, '0.00000001'], ['0', null], [null, null]
  ])('accepts the exact interval [%s, %s)', (min, max) => {
    expect(validateProcurementPolicy(policy(min, max))).toBeNull()
  })
  it.each([['0.03', '0.03'], ['0.03', '0.02'], [null, '0']])('rejects an empty interval [%s, %s)', (min, max) => {
    expect(validateProcurementPolicy(policy(min, max))).toBe('emptyRange')
  })
  it('keeps legacy omission and future versions, and sends explicit clearing with the revision', () => {
    expect(validateProcurementPolicy(null)).toBeNull()
    expect(validateProcurementPolicy({ ...policy(), version: 2 })).toBe('unsupported')
    expect(validateProcurementPolicy({ ...policy(), revision: Number.MAX_SAFE_INTEGER + 1 })).toBe('format')
    expect(procurementPolicyCommand(null, null)).toBeUndefined()
    expect(procurementPolicyCommand({ max_rate: '0.03', min_rate: '0.02', revision: 1, version: 1 }, policy())).toBeUndefined()
    expect(procurementPolicyCommand(policy(null, null), policy())).toEqual(policy(null, null))
    expect(procurementPolicyCommand({ ...policy(), version: 2 }, { ...policy(), version: 2 })).toBeUndefined()
  })

  it('distinguishes the original pool, explicitly empty, and approved sources', () => {
    for (const ids of [null, [], [901, 902], [Number.MAX_SAFE_INTEGER]]) {
      const current = { ...policy(), version: 2, sources: { account_ids: ids } }
      expect(validateProcurementPolicy(current)).toBeNull()
      expect(procurementPolicyCommand(current, { ...current })).toBeUndefined()
      expect(procurementPolicyCommand(current, policy())).toEqual(current)
    }
    for (const ids of [[0], [-1], [1, 1], [1.1], [NaN], [Number.MAX_SAFE_INTEGER + 1], Array.from({ length: 1001 }, (_, i) => i + 1)]) {
      expect(validateProcurementPolicy({ ...policy(), version: 2, sources: { account_ids: ids } })).toBe('sourceFormat')
    }
    expect(validateProcurementPolicy({ ...policy(), sources: { account_ids: null } })).toBe('sourceFormat')
  })

  it('parses IDs without coercing exponent, zero or partial values into an approval', () => {
    expect(parseProcurementSourceIDs('901, 902\n903，904')).toEqual([901, 902, 903, 904])
    expect(parseProcurementSourceIDs('  ')).toEqual([])
    for (const text of ['1e2', '1.2', '+1', '01', 'abc', '1,']) expect(parseProcurementSourceIDs(text).some(Number.isNaN)).toBe(true)
  })

  it('sends source changes and deep-copies the approved list', () => {
    const original = { ...policy(), version: 2, sources: { account_ids: [901] } }
    const current = { ...original, sources: { account_ids: [] as number[] } }
    expect(procurementPolicyCommand(current, original)).toEqual(current)
    const command = procurementPolicyCommand(original, policy())!
    command.sources!.account_ids!.push(902)
    expect(original.sources.account_ids).toEqual([901])
    expect(procurementPolicyCommand({ ...original, sources: { account_ids: null } }, original)).toMatchObject({ sources: { account_ids: null } })
  })
})
