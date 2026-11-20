import type { ProcurementProductImpact } from '@/api/admin/procurementCatalog'

const impactPriceClasses = [
  'admitted', 'rejected_threshold', 'rejected_procurement_range', 'rejected_procurement_source',
  'rejected_procurement_bucket', 'rejected_procurement_catalog_unavailable', 'rejected_procurement_policy_unavailable',
  'rejected_invalid_account_rate', 'rejected_upstream_rate_unavailable', 'rejected_account_sale_rate_mismatch'
] as const
const finiteRate = (value: unknown): value is number => typeof value === 'number' && Number.isFinite(value) && value >= 0
const count = (value: unknown): value is number => Number.isSafeInteger(value) && Number(value) >= 0

// Fail closed on an unknown/inconsistent contract; never turn a missing result
// into a zero-cost supplier or a green eligibility badge.
export function validProcurementProductImpact(value: ProcurementProductImpact, groupID: number): boolean {
  if (!value || value.version !== 1 || !Number.isFinite(Date.parse(value.observed_at)) ||
    !['active', 'inactive'].includes(value.group_status) || typeof value.gate_enabled !== 'boolean' ||
    !count(value.bound_accounts) || value.bound_accounts > 1000 || !count(value.considered_accounts) ||
    value.considered_accounts > value.bound_accounts || !count(value.user_override_count)) return false
  const report = value.report
  if (!report || report.group_id !== groupID || typeof report.group_name !== 'string' || typeof report.platform !== 'string' ||
    typeof report.effective_gate !== 'boolean' || report.assumed_enabled != null && typeof report.assumed_enabled !== 'boolean' || report.model_counts_scope !== 'account_price_only' ||
    ![report.default_d, report.min_effective_d, report.threshold_default, report.threshold_min_d].every(finiteRate) ||
    report.min_effective_d > report.default_d || report.threshold_min_d > report.threshold_default ||
    !Array.isArray(report.verdicts) || report.verdicts.length !== value.considered_accounts) return false
  const supported = ['openai', 'anthropic', 'gemini', 'grok', 'antigravity'].includes(report.platform)
  if (report.effective_gate !== supported || Boolean(report.assumed_enabled) !== (supported && !value.gate_enabled)) return false
  const ids = new Set<number>()
  return report.verdicts.every(row => {
    if (!row || !count(row.account_id) || row.account_id === 0 || ids.has(row.account_id) || typeof row.name !== 'string' ||
      typeof row.platform !== 'string' || !(impactPriceClasses as readonly string[]).includes(row.class) ||
      !['manual', 'upstream_probe_sync'].includes(row.rate_source) || !finiteRate(row.estimated_cost_rate) ||
      !['saved', 'default', 'invalid_fallback'].includes(row.estimated_cost_source) ||
      row.account_rate != null && !finiteRate(row.account_rate) || row.effective_upstream_rate != null && !finiteRate(row.effective_upstream_rate) ||
      row.rejected_under_min_d != null && typeof row.rejected_under_min_d !== 'boolean' ||
      row.warnings != null && (!Array.isArray(row.warnings) || row.warnings.some(warning => typeof warning !== 'string')) ||
      row.class === 'admitted' && supported && row.effective_upstream_rate == null) return false
    ids.add(row.account_id)
    return true
  })
}
