import type { AvailabilityPolicy } from '@/types'

export function validAvailability(policy: AvailabilityPolicy | undefined, supported: boolean): boolean {
  if (!policy) return true
  const budget = policy.max_extra_samples ?? 0
  return supported && Number.isInteger(policy.valid_for_seconds) && policy.valid_for_seconds >= 30 && policy.valid_for_seconds <= 86400 &&
    Number.isInteger(budget) && budget >= 0 && budget <= 64
}
