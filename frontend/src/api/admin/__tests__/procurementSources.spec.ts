import { beforeEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../../client'
import { listProcurementSources, getProcurementSource, createProcurementSource, validProcurementSourceMapping, validProcurementSourcePage, setProcurementSourceEnabled, deleteProcurementSource, getUpstreamGroupCatalog, validUpstreamGroupCatalog, validProcurementSourceEvidence, validProcurementUnitQuote, type ProcurementSourceUnitQuote, type ProcurementSourceEvidence, type UpstreamGroupCatalogView, type ProcurementSourceMapping } from '../procurementSources'

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
const quotation = (): UpstreamGroupCatalogView => ({ version: 1, account_id: 9, platform: 'openai', user_id: 7, observed_at: '2026-11-28T00:00:00Z', catalog: {
  version: 1, provider: 'new_api', scope: 'management_user', status: 'ok', attempted_at: '2026-11-28T00:00:00Z', received_at: '2026-11-28T00:00:00Z', fresh_until: '2026-11-28T01:00:00Z', groups: [{ id: 'vip', name: 'VIP', personal_rate: 0, public_rate: 0.5 }], changes: []
}, impact: { sources: [], products: [], truncated: false } })
beforeEach(() => vi.resetAllMocks())

export const evidenceFixture = (): ProcurementSourceEvidence => ({ version: 1, assessed_at: '2026-11-28T00:01:00Z',
  selection: { status: 'supplier_declared', source: 'new_api.management', group_id: '9007199254740993', observed_at: '2026-11-28T00:00:00Z', expires_at: '2026-11-28T01:00:00Z' },
  models: { status: 'declared', source: 'new_api.pricing', source_version: 'v1', observed_at: '2026-11-28T00:00:00Z', expires_at: '2026-11-28T00:10:00Z', entries: [
    { model: 'gpt-fixture', protocol: 'responses_http', service_tier: 'priority', reasoning_effort: 'high', context: 'all', group_rate_applies: false, conclusion: 'excluded' }
  ] }
})

describe('administrator-only source declaration contract', () => {
  const unit = (): ProcurementSourceUnitQuote => ({ version: 1, status: 'estimated', currency: 'USD', unit: 'per_1m_tokens_base_context', baseline_hash: 'a'.repeat(64), catalog_updated_at: '2026-11-28T00:00:00Z', expires_at: '2026-11-28T00:10:00Z', effective_group_rate: 0.5,
    components: [{ code: 'input', status: 'estimated', amount: 1 }, { code: 'output', status: 'estimated', amount: 5 }, { code: 'cache_read', status: 'estimated', amount: 0 }, { code: 'cache_write', status: 'estimated', amount: 1.25 }],
    pending: ['supplier_baseline_unverified', 'supplier_components_unverified', 'other_components_unreviewed'] })
  it('accepts bounded component estimates and preserves a stated zero, never qualification', () => {
    const e = evidenceFixture(), q = unit()
    expect(validProcurementUnitQuote(q)).toBe(true)
    e.selection.effective_group_rate = 0.5
    e.models.entries[0]!.conclusion = 'covered'
    e.models.entries[0]!.unit_quote = q
    expect(validProcurementSourceEvidence(e)).toBe(true)
    q.components[2] = { code: 'cache_read', status: 'not_declared', amount: null }; q.status = 'partial'
    expect(validProcurementSourceEvidence(e)).toBe(true)
    expect(q.components[2].amount).toBeNull()
  })
  it.each(['version', 'negative', 'nonfinite', 'currency', 'unit', 'duplicate-component', 'unknown-as-zero', 'missing-pending', 'hash', 'expired', 'wrong-rate', 'excluded-scope'])('rejects malformed or mis-scoped unit prices: %s', mode => {
    const e = evidenceFixture(), q = unit(); e.selection.effective_group_rate = 0.5
    e.models.entries[0]!.conclusion = 'covered'; e.models.entries[0]!.unit_quote = q
    if (mode === 'version') Object.assign(q, { version: 2 })
    if (mode === 'negative') q.components[0]!.amount = -1
    if (mode === 'nonfinite') q.components[0]!.amount = Infinity
    if (mode === 'currency') Object.assign(q, { currency: 'OTHER' })
    if (mode === 'unit') Object.assign(q, { unit: 'per_request' })
    if (mode === 'duplicate-component') q.components[1]!.code = 'input'
    if (mode === 'unknown-as-zero') { q.status = 'partial'; q.components[2]!.status = 'not_declared' }
    if (mode === 'missing-pending') q.pending.pop()
    if (mode === 'hash') q.baseline_hash = ''
    if (mode === 'expired') q.expires_at = '2026-11-27T00:00:00Z'
    if (mode === 'wrong-rate') q.effective_group_rate = .1
    if (mode === 'excluded-scope') e.models.entries[0]!.conclusion = 'excluded'
    expect(validProcurementSourceEvidence(e)).toBe(false)
  })
  it('accepts a declaration-level group/model read model without upgrading dispatch eligibility', () => {
    const evidence = evidenceFixture()
    expect(validProcurementSourceEvidence(evidence)).toBe(true)
    expect(validProcurementSourceMapping({ ...sourceFixture(), evidence })).toBe(true)
    expect(validProcurementSourceMapping({ ...sourceFixture(), evidence, dispatchable: true })).toBe(false)
    expect(validProcurementSourceMapping({ ...sourceFixture(), evidence, fixed_group_selection: 'verified' })).toBe(false)
    expect(validProcurementSourceMapping({ ...sourceFixture(), evidence, external_group_id: 'vip' })).toBe(false)
  })
  it.each(['version', 'proof-upgrade', 'wrong-source', 'wrong-model-source', 'negative-ttl', 'missing-entry', 'invalid-option', 'stale-entries', 'null-evidence'])('rejects malformed source evidence: %s', mode => {
    const e = evidenceFixture()
    if (mode === 'version') Object.assign(e, { version: 2 })
    if (mode === 'proof-upgrade') Object.assign(e.selection, { status: 'business_verified' })
    if (mode === 'wrong-source') e.selection.source = 'sub2api.key_billing'
    if (mode === 'wrong-model-source') e.models.source = 'sub2api.key_billing'
    if (mode === 'negative-ttl') e.selection.expires_at = e.selection.observed_at
    if (mode === 'missing-entry') e.models.entries = []
    if (mode === 'invalid-option') Object.assign(e.models.entries[0]!, { text_verbosity: 'private' })
    if (mode === 'stale-entries') e.models.status = 'expired'
    expect(validProcurementSourceMapping({ ...sourceFixture(), evidence: mode === 'null-evidence' ? null : e })).toBe(false)
  })
  it('reads persisted quotation data with a cancellation signal and keeps explicit zero', async () => {
    const signal = new AbortController().signal
    vi.mocked(apiClient.get).mockResolvedValue({ data: quotation() })
    expect((await getUpstreamGroupCatalog(9, signal)).catalog.groups[0]?.personal_rate).toBe(0)
    expect(apiClient.get).toHaveBeenCalledWith('/admin/accounts/9/upstream-billing-probe/catalog', { signal })
    await expect(getUpstreamGroupCatalog(10)).rejects.toThrow()
  })
  it.each(['negative', 'nonfinite', 'missing-price', 'duplicate-id', 'future-version', 'unknown-state', 'wrong-scope', 'missing-user', 'expired-ttl'])('rejects malformed quotation results: %s', async mode => {
    const q = quotation()
    if (mode === 'negative') q.catalog.groups[0]!.personal_rate = -1
    if (mode === 'nonfinite') q.catalog.groups[0]!.personal_rate = Infinity
    if (mode === 'missing-price') delete (q.catalog.groups[0] as Partial<typeof q.catalog.groups[0]>)!.personal_rate
    if (mode === 'duplicate-id') q.catalog.groups.push({ ...q.catalog.groups[0]! })
    if (mode === 'future-version') Object.assign(q.catalog, { version: 2 })
    if (mode === 'unknown-state') Object.assign(q.catalog, { status: 'qualified' })
    if (mode === 'wrong-scope') q.catalog.scope = 'call_key'
    if (mode === 'missing-user') q.user_id = null
    if (mode === 'expired-ttl') q.catalog.fresh_until = q.catalog.received_at
    expect(validUpstreamGroupCatalog(q)).toBe(false)
    vi.mocked(apiClient.get).mockResolvedValue({ data: q })
    await expect(getUpstreamGroupCatalog(9)).rejects.toThrow()
  })
  it('keeps failed last-observed quotations distinct from a current successful zero', () => {
    const q = quotation()
    q.catalog.status = 'failed'
    expect(validUpstreamGroupCatalog(q)).toBe(true)
    q.catalog.groups[0]!.personal_rate = null
    expect(validUpstreamGroupCatalog(q)).toBe(true)
  })
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
