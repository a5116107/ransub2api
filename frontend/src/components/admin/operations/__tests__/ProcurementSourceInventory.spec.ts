import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import Inventory from '../ProcurementSourceInventory.vue'
import { listProcurementSources, createProcurementSource, getProcurementSource, setProcurementSourceEnabled, deleteProcurementSource, getUpstreamGroupCatalog, type ProcurementSourceMapping, type ProcurementSourcePage, type UpstreamGroupCatalogView } from '@/api/admin/procurementSources'
import { list as listGroups } from '@/api/admin/groups'
import { list as listAccounts } from '@/api/admin/accounts'

vi.mock('vue-i18n', async importOriginal => ({ ...(await importOriginal<typeof import('vue-i18n')>()), useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/procurementSources', () => ({ listProcurementSources: vi.fn(), createProcurementSource: vi.fn(), getProcurementSource: vi.fn(), setProcurementSourceEnabled: vi.fn(), deleteProcurementSource: vi.fn(), getUpstreamGroupCatalog: vi.fn() }))
vi.mock('@/api/admin/groups', () => ({ list: vi.fn() }))
vi.mock('@/api/admin/accounts', () => ({ list: vi.fn() }))
const row = (): ProcurementSourceMapping => ({
  id: 1, site_id: 2, site_code: 'supplier-a', site_name: 'Supplier A', site_revision: 2, platform: 'openai',
  source_group_id: 3, source_group_enabled: true, source_group_revision: 2, external_group_id: 'remote-0042', external_group_name: 'Purchase A',
  local_group_id: 7, local_group_name: 'Local source', local_group_platform: 'openai', local_group_status: 'active', local_group_deleted: false,
  account_id: 9, account_name: 'Route A', account_platform: 'openai', account_status: 'active', account_deleted: false, in_local_group: true,
  enabled: true, revision: 1, updated_at: '2026-11-28T00:00:00Z', conclusion: 'pending_verification', reasons: ['fixed_group_selection_unverified'],
  route_identity: 'current', source_identity: 'declared', fixed_group_selection: 'unverified', dispatchable: false
})
const page = (): ProcurementSourcePage => ({ version: 1, observed_at: '2026-11-28T00:00:00Z', page: 1, page_size: 20, total: 1, items: [row()] })
const quotation = (): UpstreamGroupCatalogView => ({ version: 1, account_id: 9, platform: 'openai', user_id: 7, observed_at: '2026-11-28T00:00:00Z', catalog: {
  version: 1, provider: 'new_api', scope: 'management_user', status: 'ok', attempted_at: '2026-11-28T00:00:00Z', received_at: '2026-11-28T00:00:00Z', fresh_until: '2026-11-28T01:00:00Z',
  groups: [{ id: 'vip', name: 'My VIP', personal_rate: 0, public_rate: 1 }], changes: []
}, impact: { sources: [{ id: 1, name: 'Supplier / VIP' }], products: [{ id: 2, name: 'Product A' }], truncated: false } })
function evidencePage(): ProcurementSourcePage {
  const next = page()
  next.items[0]!.external_group_id = 'vip'
  next.items[0]!.evidence = { version: 1, assessed_at: '2026-11-28T00:01:00Z',
    selection: { status: 'supplier_declared', source: 'new_api.management', group_id: 'vip', observed_at: '2026-11-28T00:00:00Z', expires_at: '2026-11-28T01:00:00Z' },
    models: { status: 'declared', source: 'new_api.pricing', source_version: 'fixture-1', observed_at: '2026-11-28T00:00:00Z', expires_at: '2026-11-28T00:10:00Z', entries: [
      { model: 'gpt-fixture', protocol: 'responses_http', service_tier: 'unspecified', reasoning_effort: 'high', context: 'all', group_rate_applies: true, conclusion: 'covered' },
      { model: 'gpt-fixture', protocol: 'responses_http', service_tier: 'priority', reasoning_effort: 'high', context: 'all', group_rate_applies: false, conclusion: 'excluded' }
    ] }
  }
  return next
}
const SelectStub = defineComponent({
  props: ['modelValue', 'options', 'id', 'disabled'], emits: ['update:modelValue', 'open', 'search'],
  template: '<select :id="id" :value="modelValue" :disabled="disabled" @focus="$emit(\'open\')" @change="$emit(\'update:modelValue\', Number($event.target.value))"><option value="0"></option><option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option></select>'
})
const Confirmation = defineComponent({ props: ['show'], emits: ['confirm', 'cancel'], template: '<div v-if="show"><button data-testid="confirm-source" @click="$emit(\'confirm\')">Confirm</button></div>' })
const wrappers: ReturnType<typeof mount>[] = []
beforeEach(() => {
  vi.resetAllMocks()
  vi.mocked(listProcurementSources).mockResolvedValue(page())
  vi.mocked(createProcurementSource).mockResolvedValue(row())
  vi.mocked(getProcurementSource).mockResolvedValue(row())
  vi.mocked(listGroups).mockResolvedValue({ items: [{ id: 7, name: 'Local source', platform: 'openai' }], total: 1 } as never)
  vi.mocked(listAccounts).mockResolvedValue({ items: [{ id: 9, name: 'Route A', platform: 'openai' }], total: 1 } as never)
})
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()) })
async function view() {
  const w = mount(Inventory, { attachTo: document.body, global: { stubs: { Select: SelectStub, ConfirmDialog: Confirmation } } })
  wrappers.push(w)
  ;(w.element as HTMLDetailsElement).open = true
  await w.trigger('toggle'); await flushPromises()
  return w
}
async function draft(w: ReturnType<typeof mount>) {
  await w.findAll('button').find(button => button.text() === 'upstreamOps.sources.register')!.trigger('click'); await flushPromises()
  await w.get('[data-testid="source-site-name"]').setValue('Supplier A')
  await w.get('[data-testid="source-site-code"]').setValue('supplier-a')
  await w.get('[data-testid="source-external-id"]').setValue('remote-0042')
  await w.get('#source-local-group').setValue('7')
  await w.get('#source-account').trigger('focus'); await flushPromises()
  await w.get('#source-account').setValue('9'); await flushPromises()
  await w.get('[data-testid="source-consent"]').setValue(true)
}

describe('source registration in the existing operations page', () => {
  it('distinguishes matching supplier declarations from business proof and shows per-tier exclusions', async () => {
    vi.mocked(listProcurementSources).mockResolvedValue(evidencePage())
    const w = await view()
    expect(w.get('[data-testid="source-selection"]').text()).toContain('upstreamOps.sources.evidence.selections.supplier_declared')
    expect(w.get('[data-testid="source-conclusion"]').text()).toBe('upstreamOps.sources.states.pending_verification')
    expect(w.findAll('[data-testid="source-model-scope"]')).toHaveLength(2)
    expect(w.findAll('[data-testid="source-model-verdict"]').map(row => row.text())).toEqual(['upstreamOps.sources.evidence.verdicts.covered', 'upstreamOps.sources.evidence.verdicts.excluded'])
    const detail = w.get('[data-testid="source-model-evidence"]')
    await detail.get('input').setValue('priority')
    expect(w.findAll('[data-testid="source-model-scope"]')).toHaveLength(1)
    expect(w.get('[data-testid="source-model-verdict"]').text()).toContain('excluded')
    expect(getUpstreamGroupCatalog).not.toHaveBeenCalled()
  })
  it.each(['mismatch', 'expired', 'failed', 'unavailable'] as const)('does not show previous model conclusions when the source evidence is %s', async status => {
    const next = evidencePage(), e = next.items[0]!.evidence!
    e.selection.status = status
    e.models = { status: 'unavailable', source: '', source_version: '', observed_at: null, expires_at: null, entries: [] }
    vi.mocked(listProcurementSources).mockResolvedValue(next)
    const w = await view()
    expect(w.get('[data-testid="source-selection"]').text()).toContain(status)
    expect(w.findAll('[data-testid="source-model-scope"]')).toHaveLength(0)
    expect(w.text()).toContain('upstreamOps.sources.evidence.noCurrentScopes')
  })
  it('paginates model scopes and resets the filter on a fresh inventory read', async () => {
    const next = evidencePage(), entries = next.items[0]!.evidence!.models.entries
    next.items[0]!.evidence!.models.entries = Array.from({ length: 23 }, (_, i) => ({ ...entries[0]!, model: `model-${i}` }))
    vi.mocked(listProcurementSources).mockResolvedValue(next)
    const w = await view(), detail = w.get('[data-testid="source-model-evidence"]')
    expect(w.findAll('[data-testid="source-model-scope"]')).toHaveLength(10)
    await detail.get('input').setValue('model-22')
    expect(w.findAll('[data-testid="source-model-scope"]')).toHaveLength(1)
    await w.get('[data-testid="source-refresh"]').trigger('click'); await flushPromises()
    expect(w.findAll('[data-testid="source-model-scope"]')).toHaveLength(10)
  })
  it('reads the saved quotation only when opened, preserving zero and separate public prices', async () => {
    const w = await view()
    expect(getUpstreamGroupCatalog).not.toHaveBeenCalled()
    vi.mocked(getUpstreamGroupCatalog).mockResolvedValue(quotation())
    const detail = w.get('[data-testid="source-quote-9"]')
    ;(detail.element as HTMLDetailsElement).open = true
    await detail.trigger('toggle'); await flushPromises()
    expect(getUpstreamGroupCatalog).toHaveBeenCalledTimes(1)
    expect(w.get('[data-testid="source-personal-rate"]').text()).toBe('0')
    expect(w.get('[data-testid="source-public-rate"]').text()).toBe('1')
    expect(w.text()).toContain('Product A')
    expect(w.text()).toContain('upstreamOps.sources.catalog.impactScope')
    vi.mocked(getUpstreamGroupCatalog).mockRejectedValueOnce(new Error('private upstream body'))
    await w.get('[data-testid="source-quote-refresh"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="source-personal-rate"]').exists()).toBe(false)
    expect(w.text()).toContain('upstreamOps.sources.catalog.failed')
    expect(w.text()).not.toContain('private upstream body')
  })
  it.each(['failed', 'expired', 'authorization_changed'] as const)('labels previous quotation values as %s instead of qualified', async status => {
    const q = quotation(); q.catalog.status = status
    vi.mocked(getUpstreamGroupCatalog).mockResolvedValue(q)
    const w = await view(), detail = w.get('[data-testid="source-quote-9"]')
    ;(detail.element as HTMLDetailsElement).open = true
    await detail.trigger('toggle'); await flushPromises()
    expect(w.get('[data-testid="source-quote-status"]').text()).toBe('upstreamOps.sources.catalog.states.' + status)
    expect(w.get('[data-testid="source-personal-rate"]').text()).toBe('0')
    expect(w.get('[data-testid="source-conclusion"]').text()).toBe('upstreamOps.sources.states.pending_verification')
  })
  it('discards a late quotation response after refreshing the inventory', async () => {
    let finish!: (value: UpstreamGroupCatalogView) => void
    vi.mocked(getUpstreamGroupCatalog).mockReturnValueOnce(new Promise(resolve => { finish = resolve })).mockReturnValue(new Promise(() => undefined))
    const w = await view(), detail = w.get('[data-testid="source-quote-9"]')
    ;(detail.element as HTMLDetailsElement).open = true
    await detail.trigger('toggle')
    await w.get('[data-testid="source-refresh"]').trigger('click'); await flushPromises()
    expect(vi.mocked(getUpstreamGroupCatalog).mock.calls[0]?.[1]?.aborted).toBe(true)
    finish(quotation()); await flushPromises()
    expect(w.find('[data-testid="source-personal-rate"]').exists()).toBe(false)
  })
  it('filters and pages a large directory without losing explicit unknown values', async () => {
    const q = quotation()
    q.catalog.groups = Array.from({ length: 23 }, (_, i) => ({ id: `g-${i}`, name: `Group ${i}`, personal_rate: i === 22 ? null : i, public_rate: null }))
    vi.mocked(getUpstreamGroupCatalog).mockResolvedValue(q)
    const w = await view(), detail = w.get('[data-testid="source-quote-9"]')
    ;(detail.element as HTMLDetailsElement).open = true
    await detail.trigger('toggle'); await flushPromises()
    expect(w.findAll('[data-testid="source-personal-rate"]')).toHaveLength(20)
    await detail.get('input').setValue('g-22')
    expect(w.findAll('[data-testid="source-personal-rate"]')).toHaveLength(1)
    expect(w.get('[data-testid="source-personal-rate"]').text()).toBe('—')
  })
  it('does not read hidden inventory or issue a probe on mount', () => {
    const w = mount(Inventory); wrappers.push(w)
    expect(listProcurementSources).not.toHaveBeenCalled()
    expect(createProcurementSource).not.toHaveBeenCalled()
  })
  it('shows site → provider → purchase group, with a pending conclusion rather than green eligibility', async () => {
    const w = await view()
    expect(w.text()).toContain('Supplier A')
    expect(w.text()).toContain('Purchase A')
    expect(w.text()).toContain('Local source')
    expect(w.get('[data-testid="source-conclusion"]').text()).toBe('upstreamOps.sources.states.pending_verification')
    expect(w.text()).toContain('upstreamOps.sources.reasons.fixed_group_selection_unverified')
  })
  it('saves an explicit mapping, checks a separate GET, then refreshes the inventory', async () => {
    const w = await view(); await draft(w)
    expect(listAccounts).toHaveBeenCalledWith(1, 100, expect.objectContaining({ group: '7', platform: 'openai' }), expect.anything())
    await w.get('[data-testid="source-form"]').trigger('submit'); await flushPromises()
    expect(createProcurementSource).toHaveBeenCalledWith(expect.objectContaining({ site_code: 'supplier-a', external_group_id: 'remote-0042', local_group_id: 7, account_id: 9, confirm: true }))
    expect(getProcurementSource).toHaveBeenCalledWith(1)
    expect(w.get('[data-testid="source-saved"]').text()).toContain('upstreamOps.sources.saved')
    expect(listProcurementSources).toHaveBeenCalledTimes(2)
  })
  it.each(['read-failed', 'changed-revision', 'wrong-site'])('does not claim a save when read-back is %s and preserves the draft', async mode => {
    const w = await view(); await draft(w)
    if (mode === 'read-failed') vi.mocked(getProcurementSource).mockRejectedValueOnce(new Error('network'))
    else vi.mocked(getProcurementSource).mockResolvedValueOnce({ ...row(), ...(mode === 'wrong-site' ? { site_code: 'other' } : { revision: 2 }) })
    await w.get('[data-testid="source-form"]').trigger('submit'); await flushPromises()
    expect(w.find('[data-testid="source-saved"]').exists()).toBe(false)
    expect(w.findAll('[role="alert"]').some(element => element.text().includes('saveUnconfirmed'))).toBe(true)
    expect((w.get('[data-testid="source-site-name"]').element as HTMLInputElement).value).toBe('Supplier A')
    expect(w.get('[data-testid="source-save"]').attributes('disabled')).toBeDefined()
    expect(createProcurementSource).toHaveBeenCalledTimes(1)
    await w.get('[data-testid="source-refresh"]').trigger('click'); await flushPromises()
    expect(createProcurementSource).toHaveBeenCalledTimes(1)
  })
  it('handles 409 without discarding or replaying the draft', async () => {
    const w = await view(); await draft(w)
    vi.mocked(createProcurementSource).mockRejectedValueOnce({ status: 409 })
    await w.get('[data-testid="source-form"]').trigger('submit'); await flushPromises()
    expect(w.text()).toContain('upstreamOps.sources.conflict')
    expect((w.get('[data-testid="source-external-id"]').element as HTMLInputElement).value).toBe('remote-0042')
    expect(createProcurementSource).toHaveBeenCalledTimes(1)
  })
  it('hides old conclusions on read failure and supports explicit recovery', async () => {
    const w = await view()
    vi.mocked(listProcurementSources).mockRejectedValueOnce(new Error('SQL failure'))
    await w.get('[data-testid="source-refresh"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="source-row"]').exists()).toBe(false)
    expect(w.text()).toContain('upstreamOps.sources.failed')
    await w.get('[data-testid="source-refresh"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="source-row"]').exists()).toBe(true)
  })
  it('reads search and pagination from the server without broadening the source scope', async () => {
    const data = page(); data.total = 25
    vi.mocked(listProcurementSources).mockResolvedValue(data)
    const w = await view()
    await w.get('[data-testid="source-search"]').setValue('Supplier B')
    await w.findAll('form')[0]!.trigger('submit')
    await flushPromises()
    expect(listProcurementSources).toHaveBeenLastCalledWith(expect.objectContaining({ search: 'Supplier B', page: 1 }), expect.anything())
    await w.get('[data-testid="source-next"]').trigger('click'); await flushPromises()
    expect(listProcurementSources).toHaveBeenLastCalledWith(expect.objectContaining({ search: 'Supplier B', page: 2, page_size: 20 }), expect.anything())
  })
  it('confirms removal independently and preserves the original account link', async () => {
    const w = await view()
    vi.mocked(deleteProcurementSource).mockResolvedValue()
    vi.mocked(getProcurementSource).mockRejectedValueOnce({ status: 404 })
    vi.mocked(listProcurementSources).mockResolvedValueOnce({ ...page(), total: 0, items: [] })
    await w.findAll('button').find(button => button.text() === 'upstreamOps.sources.remove')!.trigger('click')
    await w.get('[data-testid="confirm-source"]').trigger('click'); await flushPromises()
    expect(deleteProcurementSource).toHaveBeenCalledWith(row())
    expect(getProcurementSource).toHaveBeenCalledWith(1)
    expect(w.text()).toContain('upstreamOps.sources.empty')
  })
  it('resuming a registration still shows changed-route review, never dispatchable', async () => {
    const old = { ...row(), enabled: false, conclusion: 'paused' as const }
    vi.mocked(listProcurementSources).mockResolvedValue({ ...page(), items: [old] })
    const w = await view()
    const changed = { ...row(), revision: 2, route_identity: 'changed' as const, conclusion: 'review_required' as const, reasons: ['source_account_route_changed'] }
    vi.mocked(setProcurementSourceEnabled).mockResolvedValue(changed)
    vi.mocked(getProcurementSource).mockResolvedValue(changed)
    vi.mocked(listProcurementSources).mockResolvedValue({ ...page(), items: [changed] })
    await w.findAll('button').find(button => button.text() === 'upstreamOps.sources.resume')!.trigger('click')
    await w.get('[data-testid="confirm-source"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="source-conclusion"]').text()).toContain('review_required')
    expect(w.text()).toContain('upstreamOps.sources.reasons.source_account_route_changed')
  })
})
