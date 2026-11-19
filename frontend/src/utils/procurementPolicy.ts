import type { GroupProcurementPolicy } from '@/types'

const ratePattern = /^(0|[1-9]\d{0,7})(?:\.(\d{1,8}))?$/

export function procurementDecimalUnits(value: string | null): bigint | null {
  if (value === null) return null
  if (typeof value !== 'string' || !ratePattern.test(value)) throw new Error('invalid rate')
  const [whole, fraction = ''] = value.split('.')
  return BigInt(whole) * 100000000n + BigInt(fraction.padEnd(8, '0'))
}

export function validateProcurementPolicy(policy: GroupProcurementPolicy | null): 'unsupported' | 'format' | 'emptyRange' | 'sourceFormat' | 'bucketFormat' | null {
  if (policy === null) return null
  if (policy.version !== 1 && policy.version !== 2 && policy.version !== 3) return 'unsupported'
  if (policy.version >= 2 && (!policy.sources || !Object.prototype.hasOwnProperty.call(policy.sources, 'account_ids'))) return 'unsupported'
  if (policy.version === 1 && policy.sources !== undefined) return 'sourceFormat'
  if (policy.version >= 2 && policy.sources!.account_ids !== null) {
    const ids = policy.sources!.account_ids
    if (!Array.isArray(ids) || ids.length > 1000 || new Set(ids).size !== ids.length ||
      ids.some(id => !Number.isSafeInteger(id) || id < 1)) return 'sourceFormat'
  }
  if (policy.version < 3 && policy.buckets !== undefined) return 'bucketFormat'
  if (policy.version === 3) {
    if (!policy.buckets || !Object.prototype.hasOwnProperty.call(policy.buckets, 'codes')) return 'unsupported'
    const codes = policy.buckets.codes
    if (codes !== null && (!Array.isArray(codes) || codes.length > 64 || new Set(codes).size !== codes.length || codes.some(code => typeof code !== 'string' || !/^[a-z0-9][a-z0-9_-]{0,63}$/.test(code)))) return 'bucketFormat'
  }
  if (!Number.isSafeInteger(policy.revision) || policy.revision < 0) return 'format'
  try {
    const min = procurementDecimalUnits(policy.min_rate) ?? 0n
    const max = procurementDecimalUnits(policy.max_rate)
    return max !== null && min >= max ? 'emptyRange' : null
  } catch {
    return 'format'
  }
}

export function procurementPolicyCommand(current: GroupProcurementPolicy | null, original: GroupProcurementPolicy | null): GroupProcurementPolicy | undefined {
  if (current?.version === original?.version && current?.revision === original?.revision &&
    current?.min_rate === original?.min_rate && current?.max_rate === original?.max_rate &&
    JSON.stringify(current?.sources) === JSON.stringify(original?.sources) && JSON.stringify(current?.buckets) === JSON.stringify(original?.buckets)) return undefined
  return current === null ? undefined : cloneProcurementPolicy(current)
}

export function cloneProcurementPolicy(policy: GroupProcurementPolicy): GroupProcurementPolicy {
  const next = { ...policy }
  if (policy.sources) next.sources = { account_ids: policy.sources.account_ids === null ? null : [...policy.sources.account_ids] }
  if (policy.buckets) next.buckets = { codes: Array.isArray(policy.buckets.codes) ? [...policy.buckets.codes] : policy.buckets.codes }
  return next
}

export function parseProcurementSourceIDs(value: string): number[] {
  if (!value.trim()) return []
  return value.trim().split(/[\s,，]+/).map(id => /^[1-9]\d*$/.test(id) ? Number(id) : NaN)
}
