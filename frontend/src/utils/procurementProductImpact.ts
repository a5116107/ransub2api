import type { ProcurementManagementReview, ProcurementProductImpact } from '@/api/admin/procurementCatalog'

const impactPriceClasses = [
  'admitted', 'rejected_threshold', 'rejected_procurement_range', 'rejected_procurement_source',
  'rejected_procurement_bucket', 'rejected_procurement_catalog_unavailable', 'rejected_procurement_policy_unavailable',
  'rejected_invalid_account_rate', 'rejected_upstream_rate_unavailable', 'rejected_account_sale_rate_mismatch'
] as const
const finiteRate = (value: unknown): value is number => typeof value === 'number' && Number.isFinite(value) && value >= 0
const count = (value: unknown): value is number => Number.isSafeInteger(value) && Number(value) >= 0

function validManagementReview(review: ProcurementManagementReview, groupID: number, boundAccounts: number): boolean {
  if (!review || review.version !== 1 || review.mode !== 'manual' || !['current_bindings_and_saved_rule_targets', 'current_bindings_and_approved_sources_and_saved_rule_targets'].includes(review.scope) ||
    typeof review.inventory_complete !== 'boolean' || !count(review.rules_examined) || review.rules_examined > 1000 ||
    ![review.conflicting_rules, review.protection_rules, review.unknown_rules, review.judge_dependency_rules].every(count) ||
    !Array.isArray(review.rules) || review.rules.length !== review.rules_examined) return false
  const inventory = review.source_inventory
  const includesApproved = inventory?.state === 'approved_sources'
  if (review.scope !== (includesApproved ? 'current_bindings_and_approved_sources_and_saved_rule_targets' : 'current_bindings_and_saved_rule_targets')) return false
  if (inventory != null) {
    if (inventory.version !== 1 || !['original_pool', 'approved_sources', 'unavailable'].includes(inventory.state) || !count(inventory.policy_revision) ||
      !Array.isArray(inventory.groups) || inventory.groups.length > 64 || !Array.isArray(inventory.accounts) || inventory.accounts.length > 1000) return false
    const groupIDs = new Set<number>(), accountIDs = new Set<number>()
    for (const group of inventory.groups) {
      if (!group || !count(group.id) || !group.id || groupIDs.has(group.id) || typeof group.name !== 'string' ||
        !['active', 'inactive', 'unavailable', 'self_source', 'platform_mismatch'].includes(group.state) ||
        includesApproved && (group.state !== 'active' || group.id === groupID)) return false
      groupIDs.add(group.id)
    }
    for (const account of inventory.accounts) {
      if (!account || !count(account.account_id) || !account.account_id || accountIDs.has(account.account_id) || typeof account.name !== 'string' ||
        typeof account.platform !== 'string' || typeof account.status !== 'string' || typeof account.schedulable !== 'boolean' || typeof account.in_product !== 'boolean' ||
        (includesApproved ? typeof account.in_approved_sources !== 'boolean' : account.in_approved_sources !== null) ||
        !account.in_product && (!includesApproved || !account.in_approved_sources)) return false
      accountIDs.add(account.account_id)
    }
    if (inventory.accounts.filter(a => a.in_product).length !== boundAccounts) return false
    if (includesApproved) {
      if (!inventory.policy_revision || !count(inventory.approved_accounts) || !count(inventory.unbound_approved_accounts) ||
        inventory.approved_accounts > 0 && !inventory.groups.length ||
        inventory.accounts.filter(a => a.in_approved_sources).length !== inventory.approved_accounts ||
        inventory.accounts.filter(a => a.in_approved_sources && !a.in_product).length !== inventory.unbound_approved_accounts) return false
    } else if (inventory.approved_accounts !== null || inventory.unbound_approved_accounts !== null ||
      inventory.state === 'original_pool' && inventory.groups.length > 0) return false
  }
  const ids = new Set<number>(), actions = ['connectivity', 'remove_groups', 'isolate_models', 'disable_scheduling', 'remove_models', 'enable_bps', 'observe_only', 'openai_two_fa_admission']
  for (const rule of review.rules) {
    if (!rule || !count(rule.plan_id) || !rule.plan_id || ids.has(rule.plan_id) || !count(rule.account_id) || !rule.account_id || typeof rule.account_name !== 'string' ||
      ![rule.enabled, rule.writes_product, rule.retained_ownership, rule.saved_product_ownership, rule.judge_depends_on_product].every(v => typeof v === 'boolean') ||
      !['membership_conflict', 'account_protection', 'observation', 'unknown'].includes(rule.risk) ||
      (rule.risk === 'unknown' ? rule.action !== 'unknown' : !actions.includes(rule.action)) ||
      !rule.enabled && (rule.writes_product || rule.judge_depends_on_product) || rule.saved_product_ownership && !rule.retained_ownership ||
      rule.writes_product && !['remove_groups', 'isolate_models', 'unknown'].includes(rule.action) ||
      rule.risk === 'membership_conflict' && !rule.writes_product && !rule.saved_product_ownership ||
      ['account_protection', 'observation'].includes(rule.risk) && (rule.writes_product || rule.saved_product_ownership) ||
      rule.risk === 'observation' && rule.retained_ownership) return false
    ids.add(rule.plan_id)
  }
  if (review.rules.filter(r => r.risk === 'membership_conflict').length !== review.conflicting_rules ||
    review.rules.filter(r => r.risk === 'account_protection').length !== review.protection_rules ||
    review.rules.filter(r => r.risk === 'unknown').length !== review.unknown_rules ||
    review.rules.filter(r => r.judge_depends_on_product).length !== review.judge_dependency_rules) return false
  const b = review.budget
  if (!b || ![b.revision, b.minimum_requests_per_day, b.regular_day_capacity, b.unestimated_sources].every(count) || !b.regular_day_capacity ||
    typeof b.enforced !== 'boolean' || typeof b.inventory_complete !== 'boolean' ||
    !Array.isArray(b.reasons) || !Array.isArray(b.warnings) || [...b.reasons, ...b.warnings].some(r => typeof r !== 'string') ||
    !Number.isFinite(Date.parse(b.window_start)) || !Number.isFinite(Date.parse(b.window_end)) || Date.parse(b.window_end) - Date.parse(b.window_start) !== 86400000) return false
  const state = !b.enforced ? 'not_enforced' : b.reasons.length ? 'insufficient' : !b.inventory_complete || b.unestimated_sources || b.warnings.length ? 'partial' : 'known_lower_bound_fits'
  return b.state === state
}

// Fail closed on an unknown/inconsistent contract; never turn a missing result
// into a zero-cost supplier or a green eligibility badge.
export function validProcurementProductImpact(value: ProcurementProductImpact, groupID: number): boolean {
  if (!value || value.version !== 1 || !Number.isFinite(Date.parse(value.observed_at)) ||
    !['active', 'inactive'].includes(value.group_status) || typeof value.gate_enabled !== 'boolean' ||
    !count(value.bound_accounts) || value.bound_accounts > 1000 || !count(value.considered_accounts) ||
    value.considered_accounts > value.bound_accounts || !count(value.user_override_count) ||
    value.management_review != null && !validManagementReview(value.management_review, groupID, value.bound_accounts)) return false
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
