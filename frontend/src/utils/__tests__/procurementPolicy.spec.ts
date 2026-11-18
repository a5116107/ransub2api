import { describe, expect, it } from 'vitest'
import type { GroupProcurementPolicy } from '@/types'
import { procurementPolicyCommand, validateProcurementPolicy } from '@/utils/procurementPolicy'

const policy = (min_rate: string | null = '0.02', max_rate: string | null = '0.03'): GroupProcurementPolicy =>
  ({ version: 1, revision: 1, min_rate, max_rate })

describe('exact procurement interval commands', () => {
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
})
