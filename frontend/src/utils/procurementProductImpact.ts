import type { ProcurementManagementPreview, ProcurementManagementState, ProcurementManagementReview, ProcurementProductImpact, ProcurementWriterReview, ProcurementMonitoringSelection, ProcurementSharedMonitoringPreview } from '@/api/admin/procurementCatalog'
import type { QualityRulePreset } from '@/api/admin/accountQuality'
import type { MonitoringProbeCoverage } from '@/types/monitoringProbeBudget'

const impactPriceClasses = [
  'admitted', 'rejected_threshold', 'rejected_procurement_range', 'rejected_procurement_source',
  'rejected_procurement_bucket', 'rejected_procurement_catalog_unavailable', 'rejected_procurement_policy_unavailable',
  'rejected_invalid_account_rate', 'rejected_upstream_rate_unavailable', 'rejected_account_sale_rate_mismatch'
] as const

export function validProcurementManagementState(value: ProcurementManagementState, groupID: number): boolean {
  return !!value && value.version === 1 && value.group_id === groupID &&
    [value.revision, value.current_owned, value.retained_owned, value.manual_memberships].every(count) &&
    ['manual', 'review'].includes(value.mode) && (value.mode !== 'manual' || value.current_owned === 0) &&
    (value.revision !== 0 || value.mode === 'manual')
}

export function validProcurementManagementPreview(value: ProcurementManagementPreview, groupID: number, ids: number[]): boolean {
  if (!value || value.version !== 1 || !validProcurementManagementState(value.state, groupID) ||
    !/^[a-f0-9]{64}$/.test(value.fingerprint) || !Number.isFinite(Date.parse(value.observed_at)) ||
    Date.parse(value.expires_at) - Date.parse(value.observed_at) !== 120000 || typeof value.can_confirm !== 'boolean' ||
    !Array.isArray(value.memberships) || value.memberships.length !== ids.length || !ids.length || ids.length > 1000 ||
    new Set(ids).size !== ids.length || !value.review?.source_inventory || !Array.isArray(value.review.source_inventory.accounts) || !value.review.writer_review ||
    !validManagementReview(value.review, groupID, value.review.source_inventory.accounts.filter(a => a.in_product).length) ||
    value.review.mode !== value.state.mode) return false
  const reasons = ['membership_missing', 'source_not_approved', 'inventory_incomplete', 'quality_membership_conflict', 'quality_unknown', 'native_membership_conflict', 'native_unknown', 'ownership_unknown']
  const protections = ['quality_protection', 'judge_dependency', 'native_protection', 'manual_restriction']
  const seen = new Set<number>()
  for (const row of value.memberships) {
    if (!row || !ids.includes(row.account_id) || seen.has(row.account_id) || typeof row.account_name !== 'string' || typeof row.claimable !== 'boolean' ||
      !Array.isArray(row.reasons) || row.reasons.some(r => !reasons.includes(r)) || !Array.isArray(row.protections) || row.protections.some(r => !protections.includes(r)) ||
      row.claimable !== (row.reasons.length === 0)) return false
    if (row.ownership !== null && (!row.ownership || row.ownership.version !== 1 || row.ownership.kind !== 'procurement' || !count(row.ownership.revision) || !row.ownership.revision)) return false
    const account = value.review.source_inventory.accounts.find(a => a.account_id === row.account_id)
    if (row.claimable && (!account?.in_product || account.in_approved_sources !== true || !value.review.inventory_complete || !value.review.writer_review.inventory_complete)) return false
    seen.add(row.account_id)
  }
  return value.can_confirm === value.memberships.every(m => m.claimable)
}
const finiteRate = (value: unknown): value is number => typeof value === 'number' && Number.isFinite(value) && value >= 0
const count = (value: unknown): value is number => Number.isSafeInteger(value) && Number(value) >= 0

function validWriterReview(review: ProcurementWriterReview): boolean {
  if (!review || review.version !== 1 || review.scope !== 'supply_candidates_and_explicit_writer_targets' ||
    typeof review.inventory_complete !== 'boolean' || !count(review.accounts_examined) || review.accounts_examined > 1000 ||
    ![review.membership_conflicts, review.account_protections, review.historical_only, review.unknown].every(count) ||
    !Array.isArray(review.writers) || review.writers.length > 4002) return false
  const keys = new Set<string>(), accounts = new Set<number>()
  const kinds = ['excel_bps_403', 'oauth_degradation', 'astra_scheduling', 'two_fa_admission', 'account_configuration']
  const reasons = ['configuration_unreadable', 'saved_state_unreadable', 'admission_plan_mismatch', 'not_applicable',
    'moves_all_memberships', 'native_history_not_exclusion', 'removes_product_on_trigger', 'native_account_controls',
    'astra_membership_owner', 'paused_enrollment_can_resume']
  for (const row of review.writers) {
    if (!row || !kinds.includes(row.kind) || typeof row.account_name !== 'string' ||
      row.account_id !== null && (!count(row.account_id) || !row.account_id) ||
      row.enabled !== null && typeof row.enabled !== 'boolean' ||
      !['membership_conflict', 'account_protection', 'historical_only', 'unknown'].includes(row.risk) ||
      ![row.writes_product, row.retained_ownership].every(v => typeof v === 'boolean') ||
      !Array.isArray(row.reasons) || !row.reasons.length || row.reasons.some(r => !reasons.includes(r)) || new Set(row.reasons).size !== row.reasons.length ||
      row.risk !== 'unknown' && row.enabled === null || row.enabled !== true && row.writes_product ||
      row.risk === 'membership_conflict' && !row.writes_product && !row.retained_ownership ||
      row.risk !== 'membership_conflict' && row.writes_product || row.risk === 'historical_only' && row.retained_ownership ||
      row.account_id === null && (!['oauth_degradation', 'astra_scheduling'].includes(row.kind) || row.risk !== 'unknown' || row.enabled !== null || row.account_name !== '' || row.retained_ownership) ||
      row.kind === 'account_configuration' && row.risk !== 'unknown') return false
    const key = `${row.kind}:${row.account_id}`
    if (keys.has(key)) return false
    keys.add(key)
    if (row.account_id !== null) accounts.add(row.account_id)
  }
  return accounts.size <= review.accounts_examined &&
    review.writers.filter(r => r.risk === 'membership_conflict').length === review.membership_conflicts &&
    review.writers.filter(r => r.risk === 'account_protection').length === review.account_protections &&
    review.writers.filter(r => r.risk === 'historical_only').length === review.historical_only &&
    review.writers.filter(r => r.risk === 'unknown').length === review.unknown
}

function validManagementReview(review: ProcurementManagementReview, groupID: number, boundAccounts: number): boolean {
  if (!review || review.version !== 1 || !['manual', 'review'].includes(review.mode) || !['current_bindings_and_saved_rule_targets', 'current_bindings_and_approved_sources_and_saved_rule_targets'].includes(review.scope) ||
    typeof review.inventory_complete !== 'boolean' || !count(review.rules_examined) || review.rules_examined > 1000 ||
    ![review.conflicting_rules, review.protection_rules, review.unknown_rules, review.judge_dependency_rules].every(count) ||
    !Array.isArray(review.rules) || review.rules.length !== review.rules_examined ||
    review.writer_review != null && !validWriterReview(review.writer_review)) return false
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
export function observationRecipePreset(preset: QualityRulePreset): boolean {
  const config = preset?.pelican_config
  return !!preset && count(preset.id) && preset.id > 0 && preset.enabled === true && typeof preset.name === 'string' &&
    !!config && ['candy', 'random_reasoning'].includes(config.question_kind ?? '') &&
    (!config.test_channel || config.test_channel === 'account') && config.quality?.action === 'observe_only'
}

function validMonitoringRecipe(value: ProcurementProductImpact, presetID?: number): boolean {
  const r = value.monitoring_recipe, source = value.management_review?.source_inventory
  if (presetID == null) return r == null
  if (!r || ![1, 2, 3].includes(r.version) || r.preset_id !== presetID || typeof r.preset_name !== 'string' ||
    !Number.isFinite(Date.parse(r.preset_updated_at)) || !count(r.source_policy_revision) || !source ||
    r.source_policy_revision !== source.policy_revision) return false
  if (r.version === 1) {
    if (r.distribution_review != null) return false
  } else {
    const d = r.distribution_review
    if (!d || !['ready', 'conflict', 'unknown'].includes(d.state) || !count(d.examined_accounts) || d.examined_accounts > 1000 ||
      !Array.isArray(d.conflicts) || d.conflicts.length > d.examined_accounts) return false
    if (d.state === 'unknown') {
      if (d.examined_accounts !== 0 || d.conflicts.length !== 0) return false
    } else {
      if (source.state !== 'approved_sources' || !count(r.accounts) || !r.accounts || d.examined_accounts !== r.accounts ||
        (d.state === 'conflict') !== (d.conflicts.length > 0)) return false
      const plans = new Set<number>()
      let previousAccount = 0
      for (const row of d.conflicts) {
        if (!row || !count(row.account_id) || row.account_id <= previousAccount || !count(row.plan_id) || !row.plan_id ||
          plans.has(row.plan_id) || typeof row.enabled !== 'boolean' ||
          !source.accounts.some(a => a.account_id === row.account_id && a.in_approved_sources === true)) return false
        previousAccount = row.account_id
        plans.add(row.plan_id)
      }
    }
  }
  const application = r.application_review
  if (r.version < 3 && application != null) return false
  if (source.state !== 'approved_sources') return r.state === 'sources_unavailable' && r.accounts === null && r.demand == null && application == null
  if (r.accounts !== source.approved_accounts || !count(r.accounts)) return false
  if (r.accounts === 0) return r.state === 'no_approved_accounts' && r.demand == null && application == null
  const d = r.demand, c = d?.coverage
  if (r.state !== 'previewed' || !d || !c || d.additional_plans !== r.accounts ||
    ![d.current_minimum_requests_per_day, d.delta_minimum_requests_per_day, c.minimum_total_requests_per_day, c.regular_day_capacity, c.budget_revision].every(count) ||
    c.minimum_total_requests_per_day !== d.current_minimum_requests_per_day + d.delta_minimum_requests_per_day ||
    !validProcurementRecipeCoverage(c)) return false
  if (r.version === 3) {
    if (!application || typeof application.can_confirm !== 'boolean' || !/^[a-f0-9]{64}$/.test(application.fingerprint) ||
      !Number.isFinite(Date.parse(application.observed_at)) || Date.parse(application.expires_at) - Date.parse(application.observed_at) !== 120000 ||
      Date.parse(application.observed_at) !== Date.parse(c.observed_at) || Date.parse(value.observed_at) < Date.parse(application.observed_at) ||
      application.can_confirm !== (value.group_status === 'active' && r.distribution_review?.state === 'ready' && c.enforced && c.can_save)) return false
  }
  return true
}

function validProcurementRecipeCoverage(c: MonitoringProbeCoverage): boolean {
  return !!c && [c.minimum_total_requests_per_day, c.regular_day_capacity, c.budget_revision].every(count) &&
    typeof c.enforced === 'boolean' && typeof c.can_save === 'boolean' && Array.isArray(c.blocking_reasons) && Array.isArray(c.warnings) &&
    [...c.blocking_reasons, ...c.warnings].every(v => typeof v === 'string') &&
    c.can_save === (!c.enforced || c.blocking_reasons.length === 0) && Number.isFinite(Date.parse(c.observed_at)) &&
    !!c.schedule_window_start && !!c.schedule_window_end && Date.parse(c.schedule_window_end) - Date.parse(c.schedule_window_start) === 86400000
}

export function validProcurementSharedMonitoringPreview(v: ProcurementSharedMonitoringPreview, selections: ProcurementMonitoringSelection[]): boolean {
  if (!v || v.version !== 1 || v.scope !== 'approved_sources_only' || !Number.isFinite(Date.parse(v.observed_at)) ||
    !Array.isArray(v.products) || !v.products.length || v.products.length !== selections.length || v.products.length > 20 ||
    !Array.isArray(v.accounts) || v.accounts.length > 1000 || !count(v.requirements) || !count(v.shared_accounts)) return false
  const productCounts = new Map<number, number>()
  let previous = 0
  for (const row of v.products) {
    if (!row || !count(row.product_id) || row.product_id <= previous || typeof row.name !== 'string' || !count(row.source_revision) ||
      !selections.some(s => s.product_id === row.product_id && s.preset_id === row.preset_id) ||
      !['approved_sources', 'sources_unavailable', 'no_approved_accounts', 'product_inactive'].includes(row.state) ||
      row.accounts !== null && (!count(row.accounts) || row.accounts > 1000) ||
      row.state === 'approved_sources' && (!count(row.accounts) || row.accounts === 0) ||
      row.state === 'no_approved_accounts' && row.accounts !== 0 || row.state === 'sources_unavailable' && row.accounts !== null) return false
    productCounts.set(row.product_id, 0); previous = row.product_id
  }
  let requirements = 0, shared = 0
  previous = 0
  const reasons = ['compatible', 'invalid_requirements', 'invalid_preset', 'observation_settings_conflict', 'native_model_limit', 'schedule_timezones_conflict', 'invalid_merged_plan', 'existing_observation_plan']
  for (const row of v.accounts) {
    if (!row || !count(row.account_id) || row.account_id <= previous || typeof row.name !== 'string' ||
      !Array.isArray(row.product_ids) || !row.product_ids.length || row.product_ids.length > 20 ||
      !row.product_ids.every((id, i) => productCounts.has(id) && (i === 0 || id > row.product_ids[i - 1]!)) ||
      !['compatible', 'conflict'].includes(row.state) || !reasons.includes(row.reason) ||
      (row.state === 'compatible') !== (row.reason === 'compatible') ||
      !Array.isArray(row.models) || row.models.length > 50 || new Set(row.models).size !== row.models.length || !row.models.every(m => typeof m === 'string' && m.length > 0) ||
      typeof row.cron_expression !== 'string' || (row.models.length > 0) !== (row.cron_expression.length > 0) ||
      row.state === 'compatible' && !row.models.length ||
      row.reason !== 'existing_observation_plan' && (row.existing_plan_id != null || row.existing_enabled != null) ||
      row.reason === 'existing_observation_plan' && (!count(row.existing_plan_id) || !row.existing_plan_id || typeof row.existing_enabled !== 'boolean')) return false
    for (const id of row.product_ids) productCounts.set(id, productCounts.get(id)! + 1)
    requirements += row.product_ids.length; shared += Number(row.product_ids.length > 1); previous = row.account_id
  }
  if (v.products.some(p => (p.accounts ?? 0) !== productCounts.get(p.product_id)) || requirements !== v.requirements || shared !== v.shared_accounts) return false
  const incomplete = v.products.some(p => p.state !== 'approved_sources')
  if (v.state !== (incomplete ? 'incomplete' : v.accounts.some(a => a.state === 'conflict') ? 'conflict' : 'compatible')) return false
  const computable = !incomplete && v.accounts.length > 0 && v.accounts.every(a => a.models.length > 0)
  if (!computable) return v.coverage == null && v.current_minimum_requests_per_day === null && v.delta_minimum_requests_per_day === null
  const c = v.coverage
  return !!c && validProcurementRecipeCoverage(c) && Date.parse(c.observed_at) === Date.parse(v.observed_at) &&
    count(v.current_minimum_requests_per_day) && count(v.delta_minimum_requests_per_day) &&
    c.minimum_total_requests_per_day === v.current_minimum_requests_per_day + v.delta_minimum_requests_per_day
}

export function validProcurementProductImpact(value: ProcurementProductImpact, groupID: number, presetID?: number): boolean {
  if (!value || value.version !== 1 || !Number.isFinite(Date.parse(value.observed_at)) ||
    !['active', 'inactive'].includes(value.group_status) || typeof value.gate_enabled !== 'boolean' ||
    !count(value.bound_accounts) || value.bound_accounts > 1000 || !count(value.considered_accounts) ||
    value.considered_accounts > value.bound_accounts || !count(value.user_override_count) ||
    value.management_review != null && !validManagementReview(value.management_review, groupID, value.bound_accounts) ||
    !validMonitoringRecipe(value, presetID)) return false
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
