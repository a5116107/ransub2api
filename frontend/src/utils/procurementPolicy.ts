import type { GroupProcurementPolicy } from '@/types'

const ratePattern = /^(0|[1-9]\d{0,7})(?:\.(\d{1,8}))?$/

export function procurementDecimalUnits(value: string | null): bigint | null {
  if (value === null) return null
  if (typeof value !== 'string' || !ratePattern.test(value)) throw new Error('invalid rate')
  const [whole, fraction = ''] = value.split('.')
  return BigInt(whole) * 100000000n + BigInt(fraction.padEnd(8, '0'))
}

export function validateProcurementPolicy(policy: GroupProcurementPolicy | null): 'unsupported' | 'format' | 'emptyRange' | null {
  if (policy === null) return null
  if (policy.version !== 1) return 'unsupported'
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
    current?.min_rate === original?.min_rate && current?.max_rate === original?.max_rate) return undefined
  return current === null ? undefined : { ...current }
}
