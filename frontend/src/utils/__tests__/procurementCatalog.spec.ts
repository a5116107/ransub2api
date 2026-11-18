import { describe, expect, it } from 'vitest'
import type { ProcurementCatalog } from '@/api/admin/procurementCatalog'
import { validProcurementSnapshot, validateProcurementCatalog } from '../procurementCatalog'

const fixture = (): ProcurementCatalog => ({ version: 1, revision: 1, buckets: [
  { code: 'second', name: 'Second', min_rate: '0.03000000', max_rate: null },
  { code: 'first', name: 'First', min_rate: null, max_rate: '0.03' }
] })
describe('exact procurement catalog contract', () => {
  it('accepts adjacent unsorted intervals, explicit zero and open final upper bound without mutating input', () => {
    const value = fixture(), before = JSON.stringify(value)
    expect(validateProcurementCatalog(value)).toBeNull()
    expect(JSON.stringify(value)).toBe(before)
    value.buckets[1]!.min_rate = '0'
    expect(validateProcurementCatalog(value)).toBeNull()
    expect(validProcurementSnapshot({ catalog: value, gaps: [] })).toBe(true)
  })
  it.each(['overlap', 'openMiddle', 'duplicateCode', 'duplicateName', 'badCode', 'badName', 'emptyRange', 'exponent', 'precision', 'negative', 'unsafeRevision', 'nilBuckets', 'unknownVersion'])('rejects %s', kind => {
    const value = fixture()
    switch (kind) {
      case 'overlap': value.buckets[0]!.min_rate = '0.02999999'; break
      case 'openMiddle': value.buckets[1]!.max_rate = null; break
      case 'duplicateCode': value.buckets[0]!.code = 'first'; break
      case 'duplicateName': value.buckets[0]!.name = 'First'; break
      case 'badCode': value.buckets[0]!.code = 'UPPER'; break
      case 'badName': value.buckets[0]!.name = 'A\nB'; break
      case 'emptyRange': value.buckets[1]!.min_rate = '0.03'; break
      case 'exponent': value.buckets[1]!.max_rate = '3e-2'; break
      case 'precision': value.buckets[1]!.max_rate = '0.030000001'; break
      case 'negative': value.buckets[1]!.min_rate = '-1'; break
      case 'unsafeRevision': value.revision = 2 ** 53; break
      case 'nilBuckets': value.buckets = null as never; break
      case 'unknownVersion': value.version = 2; break
    }
    expect(validateProcurementCatalog(value)).not.toBeNull()
    expect(validProcurementSnapshot({ catalog: value, gaps: [] })).toBe(false)
  })
  it('distinguishes an unconfigured catalog and malformed server gaps', () => {
    expect(validProcurementSnapshot({ catalog: { version: 1, revision: 0, buckets: [] }, gaps: [] })).toBe(true)
    expect(validProcurementSnapshot({ catalog: { ...fixture(), revision: 0 }, gaps: [] })).toBe(false)
    expect(validProcurementSnapshot({ catalog: fixture(), gaps: [{ min_rate: '0.05', max_rate: '0.04' }] })).toBe(false)
  })
})
