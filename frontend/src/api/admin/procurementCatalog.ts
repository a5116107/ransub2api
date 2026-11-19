import { apiClient } from '../client'

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

export async function readProcurementCatalog(signal?: AbortSignal): Promise<ProcurementCatalogSnapshot> {
  return (await apiClient.get<ProcurementCatalogSnapshot>('/admin/upstream-procurement/catalog', { signal })).data
}
export async function saveProcurementCatalog(catalog: ProcurementCatalog, signal?: AbortSignal): Promise<ProcurementCatalogSnapshot> {
  return (await apiClient.put<ProcurementCatalogSnapshot>('/admin/upstream-procurement/catalog', catalog, { signal })).data
}
export async function previewProcurementScopes(command: ProcurementPreviewCommand, signal?: AbortSignal): Promise<ProcurementPreview> {
  return (await apiClient.post<ProcurementPreview>('/admin/upstream-procurement/preview', command, { signal })).data
}
