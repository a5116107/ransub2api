import { apiClient } from '../client'

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
}
export interface ProcurementSourcePage {
  version: 1; observed_at: string; items: ProcurementSourceMapping[]; total: number; page: number; page_size: number
}
export interface CreateProcurementSourceMapping {
  version: 1; site_code: string; site_name: string; platform: string
  external_group_id: string; external_group_name: string; local_group_id: number; account_id: number; confirm: true
}

const record = (value: unknown): value is Record<string, unknown> => !!value && typeof value === 'object' && !Array.isArray(value)
const integer = (value: unknown, min = 1): value is number => Number.isSafeInteger(value) && (value as number) >= min
const instant = (value: unknown): value is string => typeof value === 'string' && Number.isFinite(Date.parse(value))

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
