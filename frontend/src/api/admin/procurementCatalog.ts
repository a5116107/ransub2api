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
}
export interface ProcurementPreview extends ProcurementCatalogSnapshot { observed_at: string; results: ProcurementClassification[] }

export async function readProcurementCatalog(signal?: AbortSignal): Promise<ProcurementCatalogSnapshot> {
  return (await apiClient.get<ProcurementCatalogSnapshot>('/admin/upstream-procurement/catalog', { signal })).data
}
export async function saveProcurementCatalog(catalog: ProcurementCatalog, signal?: AbortSignal): Promise<ProcurementCatalogSnapshot> {
  return (await apiClient.put<ProcurementCatalogSnapshot>('/admin/upstream-procurement/catalog', catalog, { signal })).data
}
export async function previewProcurementScopes(requests: ProcurementRequestScope[], signal?: AbortSignal): Promise<ProcurementPreview> {
  return (await apiClient.post<ProcurementPreview>('/admin/upstream-procurement/preview', { requests }, { signal })).data
}
