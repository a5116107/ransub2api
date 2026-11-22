import { describe, expect, it } from 'vitest'
import type { GroupProcurementPolicy } from '@/types'
import { applyProcurementEvidenceTemplate, cloneProcurementPolicy, parseProcurementSourceIDs, procurementPolicyCommand, validateProcurementPolicy } from '@/utils/procurementPolicy'

const policy = (min_rate: string | null = '0.02', max_rate: string | null = '0.03'): GroupProcurementPolicy =>
  ({ version: 1, revision: 1, min_rate, max_rate })

describe('exact procurement interval commands', () => {
  it('requires an explicit latest-state mode in v5 and detects mode-only edits', () => {
    const base = { ...policy(), version: 5, sources: { account_ids: [901] }, buckets: { codes: ['low'] }, quality: 'current_answer' }
    for (const availability of ['not_required', 'current_state']) {
      const next = { ...base, availability }
      expect(validateProcurementPolicy(next)).toBeNull()
      expect(procurementPolicyCommand(next, next)).toBeUndefined()
      expect(procurementPolicyCommand({ ...next, availability: availability === 'not_required' ? 'current_state' : 'not_required' }, next)).toBeDefined()
    }
    for (const availability of [undefined, '', 'future']) expect(validateProcurementPolicy({ ...base, availability })).toBe('availabilityFormat')
    expect(validateProcurementPolicy({ ...base, quality: undefined, availability: 'current_state' })).toBe('qualityFormat')
    expect(validateProcurementPolicy({ ...base, version: 4, availability: 'not_required' })).toBe('availabilityFormat')
  })
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

describe('approved source-group contract', () => {
 const base: GroupProcurementPolicy = { version: 6, revision: 2, min_rate: null, max_rate: null, sources: { account_ids: null }, buckets: { codes: null }, quality: 'not_required', availability: 'not_required', source_group_ids: null }
 it('retains null, explicit empty and selected groups in changed commands', () => {
  for (const ids of [null, [], [2, 3]]) expect(validateProcurementPolicy({ ...base, source_group_ids: ids })).toBeNull()
  expect(procurementPolicyCommand({ ...base, source_group_ids: [] }, base)?.source_group_ids).toEqual([])
  expect(procurementPolicyCommand(base, base)).toBeUndefined()
  const selected = { ...base, source_group_ids: [2, 3] }
  const clone = cloneProcurementPolicy(selected); clone.source_group_ids![0] = 4
  expect(selected.source_group_ids).toEqual([2, 3])
 })
 it('rejects missing, duplicate, unsafe, excess and old-version fields', () => {
  const { source_group_ids: _omitted, ...missing } = base
  expect(validateProcurementPolicy(missing)).toBe('unsupported')
  for (const ids of [[0], [-1], [2, 2], [Number.MAX_SAFE_INTEGER + 1], Array.from({ length: 65 }, (_, i) => i + 1)]) expect(validateProcurementPolicy({ ...base, source_group_ids: ids })).toBe('sourceGroupFormat')
  expect(validateProcurementPolicy({ ...base, version: 5 })).toBe('sourceGroupFormat')
  expect(validateProcurementPolicy({ ...base, version: 8 })).toBe('unsupported')
 })
})

describe('pinned procurement evidence templates',()=>{
 const base={version:6,revision:3,min_rate:'0.02',max_rate:'0.05',sources:{account_ids:[91]},buckets:{codes:['low']},quality:'not_required',availability:'not_required',source_group_ids:[12]}
 it('compiles once into the existing fields and preserves all independent constraints',()=>{
  const policy=applyProcurementEvidenceTemplate(base,'answer_availability')
  expect(policy).toEqual({...base,version:7,quality:'current_answer',availability:'current_state',evidence_template:{version:1,code:'answer_availability'}})
  expect(validateProcurementPolicy(policy)).toBeNull();expect(base.quality).toBe('not_required')
  const clone=cloneProcurementPolicy(policy);clone.evidence_template!.version=2;expect(policy.evidence_template!.version).toBe(1)
  const custom=applyProcurementEvidenceTemplate(policy,'custom');expect(custom.quality).toBe('current_answer');expect(custom.availability).toBe('current_state')
  expect(procurementPolicyCommand(custom,policy)?.evidence_template?.code).toBe('custom')
 })
 it('keeps incomplete sources pending and rejects drift instead of downgrading the template',()=>{
  const policy=applyProcurementEvidenceTemplate({...base,source_group_ids:null},'answer_availability')
  expect(policy.source_group_ids).toEqual([]);expect(validateProcurementPolicy(policy)).toBe('templateSources')
  for(const template of [undefined,{version:2,code:'answer_availability'},{version:1,code:'future'},{version:1,code:'custom',extra:true}]) expect(validateProcurementPolicy({...policy,evidence_template:template})).toBe('templateFormat')
  expect(validateProcurementPolicy({...policy,source_group_ids:[12],quality:'not_required'})).toBe('templateFormat')
  expect(validateProcurementPolicy({...policy,source_group_ids:[12],availability:'not_required'})).toBe('templateFormat')
  expect(validateProcurementPolicy({...base,evidence_template:{version:1,code:'custom'}})).toBe('templateFormat')
 })
})
