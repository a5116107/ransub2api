import { apiClient } from '../client'
import type { UpstreamBillingApplicabilityEntry } from '@/types'

export interface ProcurementSourceEvidence {
  version: 1; assessed_at: string
  selection: {
    status: 'unavailable' | 'not_observed' | 'failed' | 'invalid' | 'expired' | 'unverified' | 'mismatch' | 'supplier_declared' | 'receipt_confirmed'
    source: 'none' | 'new_api.management' | 'sub2api.key_billing'; group_id: string; observed_at: string | null; expires_at: string | null
  }
  models: {
    status: 'unavailable' | 'not_declared' | 'invalid' | 'expired' | 'declared'
    source: '' | 'new_api.pricing' | 'sub2api.key_billing'; source_version: string; observed_at: string | null; expires_at: string | null
    entries: UpstreamBillingApplicabilityEntry[]
  }
}

export interface ProcurementSourceMapping {
  id: number; site_id: number; site_code: string; site_name: string; site_revision: number
  platform: string; source_group_id: number; external_group_id: string; external_group_name: string
  source_group_enabled: boolean; source_group_revision: number
  local_group_id: number; local_group_name: string; local_group_platform: string; local_group_status: string; local_group_deleted: boolean
  account_id: number; account_name: string; account_platform: string; account_status: string; account_deleted: boolean; in_local_group: boolean
  enabled: boolean; revision: number; updated_at: string
  conclusion: 'pending_verification' | 'paused' | 'blocked' | 'review_required'
  reasons: string[]; route_identity: 'current' | 'changed' | 'unknown'
  source_identity: 'declared'; fixed_group_selection: 'unverified'; dispatchable: false
  evidence?: ProcurementSourceEvidence
}
export interface ProcurementSourcePage {
  version: 1; observed_at: string; items: ProcurementSourceMapping[]; total: number; page: number; page_size: number
}
export interface CreateProcurementSourceMapping {
  version: 1; site_code: string; site_name: string; platform: string
  external_group_id: string; external_group_name: string; local_group_id: number; account_id: number; confirm: true
}

export interface UpstreamGroupQuote { id: string; name: string; personal_rate: number | null; public_rate: number | null }
export interface UpstreamGroupChange { kind: 'group_added' | 'group_removed' | 'group_updated'; group_id: string; at: string; before: UpstreamGroupQuote | null; after: UpstreamGroupQuote | null }
export interface UpstreamGroupCatalogView {
  version: 1; account_id: number; platform: string; user_id: number | null; observed_at: string
  catalog: {
    version: 1; provider: 'new_api' | 'sub2api'; scope: 'management_user' | 'call_key'
    status: 'ok' | 'failed' | 'expired' | 'not_observed' | 'authorization_changed'
    attempted_at: string; received_at: string | null; fresh_until: string | null
    groups: UpstreamGroupQuote[]; changes: UpstreamGroupChange[]
  }
  impact: { sources: Array<{ id: number; name: string }>; products: Array<{ id: number; name: string }>; truncated: boolean }
}

const record = (value: unknown): value is Record<string, unknown> => !!value && typeof value === 'object' && !Array.isArray(value)
const integer = (value: unknown, min = 1): value is number => Number.isSafeInteger(value) && (value as number) >= min
const instant = (value: unknown): value is string => typeof value === 'string' && Number.isFinite(Date.parse(value))

export function validProcurementSourceEvidence(value: unknown): value is ProcurementSourceEvidence {
  if (!record(value) || value.version !== 1 || !instant(value.assessed_at) || !record(value.selection) || !record(value.models)) return false
  const s = value.selection, m = value.models
  const dates = (v: Record<string, unknown>) => (v.observed_at === null || instant(v.observed_at)) && (v.expires_at === null || instant(v.expires_at))
  const live = (v: Record<string, unknown>) => instant(v.observed_at) && instant(v.expires_at) && Date.parse(v.observed_at) <= Date.parse(value.assessed_at as string) && Date.parse(v.expires_at) > Date.parse(value.assessed_at as string)
  const token = (v: unknown, max: number) => typeof v === 'string' && v.length > 0 && v.length <= max && /^[\x21-\x7e]+$/.test(v) && !v.includes('*')
  const selection = ['supplier_declared', 'receipt_confirmed'].includes(String(s.status))
  return ['unavailable', 'not_observed', 'failed', 'invalid', 'expired', 'unverified', 'mismatch', 'supplier_declared', 'receipt_confirmed'].includes(String(s.status)) &&
    ['none', 'new_api.management', 'sub2api.key_billing'].includes(String(s.source)) && typeof s.group_id === 'string' && s.group_id.length <= 256 && dates(s) &&
    (!selection || s.group_id !== '' && live(s) && (s.status === 'supplier_declared' ? s.source === 'new_api.management' : s.source === 'sub2api.key_billing')) &&
    ['unavailable', 'not_declared', 'invalid', 'expired', 'declared'].includes(String(m.status)) && ['', 'new_api.pricing', 'sub2api.key_billing'].includes(String(m.source)) && typeof m.source_version === 'string' && dates(m) &&
    (m.status !== 'declared' || selection && live(m) && token(m.source_version, 128) && (s.source === 'new_api.management' ? m.source === 'new_api.pricing' : m.source === 'sub2api.key_billing')) &&
    Array.isArray(m.entries) && m.entries.length <= 128 && (m.status === 'declared' ? m.entries.length > 0 : m.entries.length === 0) && m.entries.every(e => record(e) &&
      token(e.model, 200) && token(e.protocol, 64) && token(e.service_tier, 64) && token(e.reasoning_effort, 64) && token(e.context, 64) &&
      (e.group_rate_applies === null || typeof e.group_rate_applies === 'boolean') && ['covered', 'excluded', 'unknown', 'conflict', 'expired'].includes(String(e.conclusion)) &&
      (e.reasoning_summary === undefined || ['none', 'auto', 'concise', 'detailed'].includes(String(e.reasoning_summary))) &&
      (e.include_encrypted_content === undefined || typeof e.include_encrypted_content === 'boolean') && (e.text_verbosity === undefined || ['low', 'medium', 'high'].includes(String(e.text_verbosity))))
}

export function validUpstreamGroupCatalog(value: unknown): value is UpstreamGroupCatalogView {
  const rate = (v: unknown) => v === null || typeof v === 'number' && Number.isFinite(v) && v >= 0 && v <= 1e6
  const quote = (v: unknown) => record(v) && typeof v.id === 'string' && v.id.length > 0 && v.id.length <= 256 && typeof v.name === 'string' && rate(v.personal_rate) && rate(v.public_rate)
  if (!record(value) || value.version !== 1 || !integer(value.account_id) || typeof value.platform !== 'string' || !instant(value.observed_at) || !record(value.catalog) || !record(value.impact)) return false
  const c = value.catalog, impact = value.impact
  return c.version === 1 && ['ok', 'failed', 'expired', 'not_observed', 'authorization_changed'].includes(String(c.status)) &&
    (c.provider === 'new_api' && c.scope === 'management_user' && integer(value.user_id) || c.provider === 'sub2api' && c.scope === 'call_key' && value.user_id === null) &&
    instant(c.attempted_at) && (c.received_at === null || instant(c.received_at)) && (c.fresh_until === null || instant(c.fresh_until)) &&
    (c.status !== 'ok' || instant(c.received_at) && instant(c.fresh_until) && Date.parse(c.fresh_until) > Date.parse(c.received_at) && Date.parse(c.fresh_until) > Date.parse(value.observed_at)) &&
    Array.isArray(c.groups) && c.groups.length <= 1024 && c.groups.every(quote) && new Set(c.groups.map(g => g.id)).size === c.groups.length &&
    Array.isArray(c.changes) && c.changes.length <= 100 && c.changes.every(v => record(v) && ['group_added', 'group_removed', 'group_updated'].includes(String(v.kind)) && typeof v.group_id === 'string' && instant(v.at) &&
      (v.before === null || quote(v.before) && record(v.before) && v.before.id === v.group_id) && (v.after === null || quote(v.after) && record(v.after) && v.after.id === v.group_id) &&
      (v.kind === 'group_added' ? v.before === null && v.after !== null : v.kind === 'group_removed' ? v.before !== null && v.after === null : v.before !== null && v.after !== null)) &&
    ['sources', 'products'].every(key => Array.isArray(impact[key]) && impact[key].length <= 100 && impact[key].every(v => record(v) && integer(v.id) && typeof v.name === 'string')) && typeof impact.truncated === 'boolean'
}

export async function getUpstreamGroupCatalog(accountID: number, signal?: AbortSignal): Promise<UpstreamGroupCatalogView> {
  const { data } = await apiClient.get<unknown>(`/admin/accounts/${accountID}/upstream-billing-probe/catalog`, { signal })
  if (!validUpstreamGroupCatalog(data) || data.account_id !== accountID) throw new Error('Invalid upstream group quotation response')
  return data
}

export function validProcurementSourceMapping(value: unknown): value is ProcurementSourceMapping {
  if (!record(value)) return false
  return ['site_id', 'source_group_id', 'site_revision', 'source_group_revision', 'local_group_id'].every(key => integer(value[key])) &&
    ['id', 'account_id', 'revision'].every(key => integer(value[key], 0)) &&
    ['site_code', 'site_name', 'platform', 'external_group_id', 'external_group_name', 'local_group_name', 'local_group_platform', 'local_group_status', 'account_name', 'account_platform', 'account_status'].every(key => typeof value[key] === 'string') &&
    value.site_code !== '' && value.external_group_id !== '' &&
    ['source_group_enabled', 'local_group_deleted', 'account_deleted', 'in_local_group', 'enabled'].every(key => typeof value[key] === 'boolean') &&
    ['pending_verification', 'paused', 'blocked', 'review_required'].includes(String(value.conclusion)) &&
    ['current', 'changed', 'unknown'].includes(String(value.route_identity)) &&
    value.source_identity === 'declared' && value.fixed_group_selection === 'unverified' && value.dispatchable === false &&
    (value.evidence === undefined || validProcurementSourceEvidence(value.evidence)) &&
    (value.evidence === undefined || !['supplier_declared', 'receipt_confirmed'].includes(String((value.evidence as ProcurementSourceEvidence).selection.status)) || (value.evidence as ProcurementSourceEvidence).selection.group_id === value.external_group_id) &&
    Array.isArray(value.reasons) && value.reasons.length > 0 && value.reasons.every(reason => typeof reason === 'string') &&
    instant(value.updated_at) && (value.account_id === 0 ? value.id === 0 && value.revision === 0 : integer(value.id) && integer(value.revision))
}
export function validProcurementSourcePage(value: unknown): value is ProcurementSourcePage {
  return record(value) && value.version === 1 && instant(value.observed_at) && integer(value.page) &&
    integer(value.page_size) && value.page_size <= 100 && integer(value.total, 0) && Array.isArray(value.items) &&
    value.items.length <= value.page_size && value.items.every(validProcurementSourceMapping) && value.total >= value.items.length
}

const base = '/admin/upstream-procurement/source-mappings'
export async function listProcurementSources(params: { search: string; platform: string; page: number; page_size: number }, signal?: AbortSignal): Promise<ProcurementSourcePage> {
  const { data } = await apiClient.get<unknown>(base, { params, signal })
  if (!validProcurementSourcePage(data) || data.page !== params.page || data.page_size !== params.page_size) throw new Error('Invalid source inventory response')
  return data
}
export async function getProcurementSource(id: number): Promise<ProcurementSourceMapping> {
  const { data } = await apiClient.get<unknown>(`${base}/${id}`)
  if (!validProcurementSourceMapping(data) || data.id !== id) throw new Error('Invalid source mapping response')
  return data
}
export async function createProcurementSource(command: CreateProcurementSourceMapping): Promise<ProcurementSourceMapping> {
  const { data } = await apiClient.post<unknown>(base, command)
  if (!validProcurementSourceMapping(data) || data.site_code !== command.site_code || data.external_group_id !== command.external_group_id ||
    data.local_group_id !== command.local_group_id || data.account_id !== command.account_id || data.platform !== command.platform) throw new Error('Invalid source mapping receipt')
  return data
}
export async function setProcurementSourceEnabled(item: ProcurementSourceMapping): Promise<ProcurementSourceMapping> {
  const { data } = await apiClient.put<unknown>(`${base}/${item.id}`, { version: 1, expected_revision: item.revision, enabled: !item.enabled, confirm: true })
  if (!validProcurementSourceMapping(data) || data.id !== item.id || data.revision !== item.revision + 1 || data.enabled !== !item.enabled) throw new Error('Invalid source mapping receipt')
  return data
}
export async function deleteProcurementSource(item: ProcurementSourceMapping): Promise<void> {
  const { data } = await apiClient.delete<{ deleted: number }>(`${base}/${item.id}`, { data: { version: 1, expected_revision: item.revision, confirm: true } })
  if (data?.deleted !== item.id) throw new Error('Invalid deletion receipt')
}
