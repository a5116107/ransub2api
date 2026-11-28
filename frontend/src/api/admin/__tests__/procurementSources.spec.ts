import { beforeEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../../client'
import { listProcurementSources, getProcurementSource, createProcurementSource, validProcurementSourceMapping, validProcurementSourcePage, setProcurementSourceEnabled, deleteProcurementSource, type ProcurementSourceMapping } from '../procurementSources'

vi.mock('../../client', () => ({ apiClient: { get: vi.fn(), post: vi.fn(), put: vi.fn(), delete: vi.fn() } }))
export const sourceFixture = (): ProcurementSourceMapping => ({
  id: 1, site_id: 2, site_code: 'supplier-a', site_name: 'Supplier A', site_revision: 3, platform: 'openai',
  source_group_id: 4, source_group_enabled: true, source_group_revision: 2, external_group_id: '9007199254740993', external_group_name: 'Group A',
  local_group_id: 7, local_group_name: 'Local source', local_group_platform: 'openai', local_group_status: 'active', local_group_deleted: false,
  account_id: 9, account_name: 'Route A', account_platform: 'openai', account_status: 'active', account_deleted: false, in_local_group: true,
  enabled: true, revision: 1, updated_at: '2026-11-28T00:00:00Z', conclusion: 'pending_verification', reasons: ['fixed_group_selection_unverified'],
  route_identity: 'current', source_identity: 'declared', fixed_group_selection: 'unverified', dispatchable: false
})
const page = () => ({ version: 1, observed_at: '2026-11-28T00:00:00Z', items: [sourceFixture()], total: 1, page: 1, page_size: 20 })
beforeEach(() => vi.resetAllMocks())

describe('administrator-only source declaration contract', () => {
  it('reads the explicit site/group mapping without coercing external IDs', async () => {
    vi.mocked(apiClient.get).mockResolvedValue({ data: page() })
    const signal = new AbortController().signal
    const params = { search: 'Supplier A', platform: 'openai', page: 1, page_size: 20 }
    expect((await listProcurementSources(params, signal)).items[0]?.external_group_id).toBe('9007199254740993')
    expect(apiClient.get).toHaveBeenCalledWith('/admin/upstream-procurement/source-mappings', { params, signal })
  })
  it.each(['future', 'missing', 'dispatchable', 'verified', 'unsafe-id', 'numeric-external', 'invalid-date', 'unknown-status', 'missing-reasons'])('rejects incompatible or false green responses: %s', async mode => {
    const next = page()
    if (mode === 'future') next.version = 2
    if (mode === 'missing') delete (next as Partial<typeof next>).observed_at
    if (mode === 'dispatchable') Object.assign(next.items[0]!, { dispatchable: true })
    if (mode === 'verified') Object.assign(next.items[0]!, { fixed_group_selection: 'verified' })
    if (mode === 'unsafe-id') next.items[0]!.id = 9007199254740992
    if (mode === 'numeric-external') Object.assign(next.items[0]!, { external_group_id: 42 })
    if (mode === 'invalid-date') next.items[0]!.updated_at = ''
    if (mode === 'unknown-status') Object.assign(next.items[0]!, { conclusion: 'ready' })
    if (mode === 'missing-reasons') next.items[0]!.reasons = []
    vi.mocked(apiClient.get).mockResolvedValue({ data: next })
    expect(validProcurementSourcePage(next)).toBe(false)
    await expect(listProcurementSources({ search: '', platform: '', page: 1, page_size: 20 })).rejects.toThrow()
  })
  it('checks single-row identity and revision after writes', async () => {
    const item = sourceFixture()
    const command = { version: 1 as const, site_code: item.site_code, site_name: item.site_name, platform: item.platform, external_group_id: item.external_group_id, external_group_name: item.external_group_name, local_group_id: item.local_group_id, account_id: item.account_id, confirm: true as const }
    vi.mocked(apiClient.post).mockResolvedValue({ data: item })
    expect((await createProcurementSource(command)).id).toBe(1)
    vi.mocked(apiClient.get).mockResolvedValue({ data: item })
    expect((await getProcurementSource(1)).id).toBe(1)
    await expect(getProcurementSource(2)).rejects.toThrow()
    vi.mocked(apiClient.put).mockResolvedValue({ data: { ...item, enabled: false, revision: 2 } })
    await setProcurementSourceEnabled(item)
    expect(apiClient.put).toHaveBeenCalledWith('/admin/upstream-procurement/source-mappings/1', { version: 1, expected_revision: 1, enabled: false, confirm: true })
    vi.mocked(apiClient.put).mockResolvedValue({ data: { ...item, enabled: false } })
    await expect(setProcurementSourceEnabled(item)).rejects.toThrow()
    vi.mocked(apiClient.delete).mockResolvedValue({ data: { deleted: 1 } })
    await deleteProcurementSource(item)
    expect(apiClient.delete).toHaveBeenCalledWith('/admin/upstream-procurement/source-mappings/1', { data: { version: 1, expected_revision: 1, confirm: true } })
  })
  it('accepts an explicitly unmapped group only as blocked, not a source account', () => {
    expect(validProcurementSourceMapping({ ...sourceFixture(), id: 0, account_id: 0, revision: 0, conclusion: 'blocked' })).toBe(true)
    expect(validProcurementSourceMapping({ ...sourceFixture(), id: 0, account_id: 9, revision: 0 })).toBe(false)
  })
})
