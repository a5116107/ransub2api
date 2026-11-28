import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import Inventory from '../ProcurementSourceInventory.vue'
import { listProcurementSources, createProcurementSource, getProcurementSource, setProcurementSourceEnabled, deleteProcurementSource, type ProcurementSourceMapping, type ProcurementSourcePage } from '@/api/admin/procurementSources'
import { list as listGroups } from '@/api/admin/groups'
import { list as listAccounts } from '@/api/admin/accounts'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/procurementSources', () => ({ listProcurementSources: vi.fn(), createProcurementSource: vi.fn(), getProcurementSource: vi.fn(), setProcurementSourceEnabled: vi.fn(), deleteProcurementSource: vi.fn() }))
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
