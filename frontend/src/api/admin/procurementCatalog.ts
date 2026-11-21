import { apiClient } from '../client'
import type { MonitoringProbeCoverage } from '@/types/monitoringProbeBudget'

export interface ProcurementBucket { code: string; name: string; min_rate: string | null; max_rate: string | null }
export interface ProcurementCatalog { version: number; revision: number; buckets: ProcurementBucket[] }
export interface ProcurementCatalogSnapshot { catalog: ProcurementCatalog; gaps: Array<{ min_rate: string | null; max_rate: string | null }> }
export interface ProcurementRequestScope {
  account_id: number
  public_model: string
  scope: {
    version: number; protocol: string; stream: boolean; model: string; service_tier: string; reasoning_effort: string; capability: string
    reasoning_summary?: string; include_encrypted_content?: boolean; text_verbosity?: string
    thinking_mode?: string; thinking_budget?: number; protocol_options?: string
  }
}
export interface ProcurementClassification extends ProcurementRequestScope {
  state: string; reason?: string; applicability: string; rate_source: string; effective_rate: string | null
  bucket: ProcurementBucket | null; qualification: string
  quality_evidence?: QualityScopeEvidence
  availability_evidence?: AvailabilityScopeEvidence
  product_procurement?: {
    price_valid_until?: string | null
    account_controls?: { version: number; state: string; reason: string; evaluated_at: string; valid_until: string | null }
    evaluated_gates?: { version: number; state: string; reason: string; passed_gates: number; required_gates: number; evaluated_at: string; valid_until: string | null }
    answer_quality?: { version: number; required: boolean; state: string }
    availability?: { version: number; required: boolean; state: string }
    version: number; group_id: number; policy_revision: number; price_basis: string; default_d: number; threshold: number
    state: string; price_class: string; reason?: string; catalog_revision?: number
  }
}
export interface AvailabilityScopeEvidence {
  version: number; status: string; reason: string; evaluated_at: string; valid_until: string | null
  rule_count: number; confirmed_rules: number
}
export interface QualityScopeRuleEvidence {
  plan_id: number; status: string; reason: string; samples: number; valid_samples: number; valid_until: string | null
}
export interface QualityScopeEvidence {
  version: number; status: string; reason: string; model_identity: string; evaluated_at: string; valid_until: string | null
  rule_count: number; confirmed_rules: number; samples: number; valid_samples: number; other_scope_rules: number
  rules: QualityScopeRuleEvidence[]
}
export interface ProcurementPreviewCommand { requests: ProcurementRequestScope[]; product_group_id?: number }
export interface ProcurementPreview extends ProcurementCatalogSnapshot { observed_at: string; results: ProcurementClassification[] }

export interface ProcurementProductImpact {
  version: number
  observed_at: string
  group_status: string
  gate_enabled: boolean
  bound_accounts: number
  considered_accounts: number
  user_override_count: number
  management_review?: ProcurementManagementReview | null
  monitoring_recipe?: ProcurementMonitoringRecipe | null
  report: {
    group_id: number; group_name: string; platform: string; effective_gate: boolean; assumed_enabled?: boolean
    default_d: number; min_effective_d: number; threshold_default: number; threshold_min_d: number; model_counts_scope: string
    verdicts: Array<{
      account_id: number; name: string; platform: string; class: string; account_rate?: number; effective_upstream_rate?: number
      rate_source: string; warnings?: string[]; rejected_under_min_d?: boolean
      estimated_cost_rate: number; estimated_cost_source: string
    }>
  }
}

export interface ProcurementManagementReview {
  version: number; mode: string; scope: string; inventory_complete: boolean
  rules_examined: number; conflicting_rules: number; protection_rules: number; unknown_rules: number; judge_dependency_rules: number
  rules: Array<{
    plan_id: number; account_id: number; account_name: string; enabled: boolean; action: string; risk: string
    writes_product: boolean; retained_ownership: boolean; saved_product_ownership: boolean; judge_depends_on_product: boolean
  }>
  source_inventory?: ProcurementSourceInventory | null
  writer_review?: ProcurementWriterReview | null
  budget: {
    state: string; revision: number; enforced: boolean; inventory_complete: boolean
    minimum_requests_per_day: number; regular_day_capacity: number; unestimated_sources: number
    reasons: string[]; warnings: string[]; window_start: string; window_end: string
  }
}

export interface ProcurementSourceInventory {
  version: number; state: string; policy_revision: number
  approved_accounts: number | null; unbound_approved_accounts: number | null
  groups: Array<{ id: number; name: string; state: string }>
  accounts: Array<{ account_id: number; name: string; platform: string; status: string; schedulable: boolean; in_product: boolean; in_approved_sources: boolean | null }>
}

export interface ProcurementWriterReview {
  version: number; scope: string; inventory_complete: boolean; accounts_examined: number
  membership_conflicts: number; account_protections: number; historical_only: number; unknown: number
  writers: Array<{
    kind: string; account_id: number | null; account_name: string; enabled: boolean | null; risk: string
    writes_product: boolean; retained_ownership: boolean; reasons: string[]
  }>
}

export interface ProcurementMonitoringRecipe {
  version: number; preset_id: number; preset_name: string; preset_updated_at: string; source_policy_revision: number
  state: string; accounts: number | null
  demand?: {
    additional_plans: number; current_minimum_requests_per_day: number; delta_minimum_requests_per_day: number
    coverage: MonitoringProbeCoverage
  }
}

export async function previewProcurementProductImpact(id: number, signal?: AbortSignal, presetID?: number): Promise<ProcurementProductImpact> {
  return (await apiClient.get<ProcurementProductImpact>(`/admin/upstream-procurement/products/${id}/impact`, {
    signal, ...(presetID == null ? {} : { params: { monitoring_preset_id: presetID } })
  })).data
}

export async function readProcurementCatalog(signal?: AbortSignal): Promise<ProcurementCatalogSnapshot> {
  return (await apiClient.get<ProcurementCatalogSnapshot>('/admin/upstream-procurement/catalog', { signal })).data
}

export interface ProcurementManagementState {
  version: 1; group_id: number; revision: number; mode: 'manual' | 'review'
  current_owned: number; retained_owned: number; manual_memberships: number
}
export interface ProcurementMembershipReview {
  account_id: number; account_name: string; ownership: { version: 1; kind: 'procurement'; revision: number } | null
  claimable: boolean; reasons: string[]; protections: string[]
}
export interface ProcurementManagementPreview {
  version: 1; state: ProcurementManagementState; observed_at: string; expires_at: string; fingerprint: string
  can_confirm: boolean; memberships: ProcurementMembershipReview[]; review: ProcurementManagementReview
}
export type ProcurementManagementCommand = { version: 1; mode: 'manual'; expected_revision: number } |
  { version: 1; mode: 'review'; expected_revision: number; account_ids: number[]; observed_at: string; fingerprint: string }
export async function readProcurementManagement(id: number, signal?: AbortSignal): Promise<ProcurementManagementState> {
  return (await apiClient.get<ProcurementManagementState>(`/admin/upstream-procurement/products/${id}/management`, { signal })).data
}
export async function previewProcurementManagement(id: number, account_ids: number[], signal?: AbortSignal): Promise<ProcurementManagementPreview> {
  return (await apiClient.post<ProcurementManagementPreview>(`/admin/upstream-procurement/products/${id}/management/preview`, { account_ids }, { signal })).data
}
export async function saveProcurementManagement(id: number, command: ProcurementManagementCommand, signal?: AbortSignal): Promise<ProcurementManagementState> {
  return (await apiClient.put<ProcurementManagementState>(`/admin/upstream-procurement/products/${id}/management`, command, { signal })).data
}
export async function saveProcurementCatalog(catalog: ProcurementCatalog, signal?: AbortSignal): Promise<ProcurementCatalogSnapshot> {
  return (await apiClient.put<ProcurementCatalogSnapshot>('/admin/upstream-procurement/catalog', catalog, { signal })).data
}
export async function previewProcurementScopes(command: ProcurementPreviewCommand, signal?: AbortSignal): Promise<ProcurementPreview> {
  return (await apiClient.post<ProcurementPreview>('/admin/upstream-procurement/preview', command, { signal })).data
}
