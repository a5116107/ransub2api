import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AdminGroup, GroupProcurementPolicy } from '@/types'
import GroupsView from '@/views/admin/GroupsView.vue'

const mocks = vi.hoisted(() => ({
  list: vi.fn(), create: vi.fn(), update: vi.fn(), getById: vi.fn(), showError: vi.fn(), showSuccess: vi.fn(),
  getAll: vi.fn(), getModelAllowlistCandidates: vi.fn(), getUsageSummary: vi.fn(), getCapacitySummary: vi.fn(), getLiveCapability: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: {
  groups: { list: mocks.list, create: mocks.create, update: mocks.update, getById: mocks.getById,
    getAll: mocks.getAll, getModelAllowlistCandidates: mocks.getModelAllowlistCandidates,
    getUsageSummary: mocks.getUsageSummary, getCapacitySummary: mocks.getCapacitySummary,
    getLiveCapability: mocks.getLiveCapability, duplicate: vi.fn(), delete: vi.fn(), updateSortOrder: vi.fn() },
  accounts: { list: vi.fn(), getById: vi.fn() }
} }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.showSuccess, showError: mocks.showError }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isSimpleMode: false }) }))
vi.mock('@/stores/onboarding', () => ({ useOnboardingStore: () => ({ isCurrentStep: vi.fn(() => false), nextStep: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))

const savedPolicy: GroupProcurementPolicy = { version: 1, revision: 1, min_rate: '0.02', max_rate: '0.03' }
const base = {
  id: 42, name: 'Fixed sale product', platform: 'openai', rate_multiplier: 0.15, status: 'active', subscription_type: 'standard', is_exclusive: false,
  profit_control_enabled: true, profit_min_margin: 0, profit_safety_buffer: 0, procurement_policy: savedPolicy,
  model_routing: null, model_routing_enabled: false, mcp_xml_inject: true, supported_model_scopes: [],
  created_at: '2026-11-19T00:00:00Z', updated_at: '2026-11-19T00:00:00Z'
} as AdminGroup
const wrappers: ReturnType<typeof mount>[] = []
function mountView(group = base) {
  mocks.list.mockResolvedValue({ items: [group], total: 1, page: 1, page_size: 20, pages: 1 })
  const wrapper = mount(GroupsView, { global: { stubs: {
    AppLayout: defineComponent({ template: '<main><slot /></main>' }),
    TablePageLayout: defineComponent({ template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>' }),
    DataTable: defineComponent({ props: ['data'], template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-actions" :row="row" /></div></div>' }),
    BaseDialog: defineComponent({ props: { show: Boolean }, template: '<div v-if="show"><slot /><slot name="footer" /></div>' }),
    Pagination: true, ConfirmDialog: true, EmptyState: true, Select: true, PlatformIcon: true, Icon: true,
    GroupCapacityBadge: true, GroupRateMultipliersModal: true, GroupRPMOverridesModal: true, VueDraggable: true
  } } })
  wrappers.push(wrapper)
  return wrapper
}
type View = ReturnType<typeof mountView>
const button = (wrapper: View, name: string) => wrapper.findAll('button').find(b => b.text() === name)!
async function edit(wrapper: View) {
  await flushPromises()
  await button(wrapper, 'common.edit').trigger('click')
  await flushPromises()
}
const submit = async (wrapper: View) => { await wrapper.get('#edit-group-form').trigger('submit'); await flushPromises() }
const procurementToggle = (wrapper: View, form: string) => wrapper.get(form).findAll('label')
  .find(label => label.text() === 'admin.groups.profitControl.enable')!.get('input')

describe('product procurement interval in the original Group editor', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.clearAllMocks()
    Object.values(mocks).forEach(mock => mock.mockReset())
    mocks.create.mockResolvedValue(base)
    mocks.update.mockResolvedValue(base)
    mocks.getById.mockResolvedValue(base)
    mocks.getAll.mockResolvedValue([])
    mocks.getModelAllowlistCandidates.mockResolvedValue([])
    mocks.getUsageSummary.mockResolvedValue([])
    mocks.getCapacitySummary.mockResolvedValue([])
    mocks.getLiveCapability.mockResolvedValue({ supported: false })
  })
  afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); vi.restoreAllMocks() })

  it('loads exact bounds and omits the policy on an unchanged save', async () => {
    const wrapper = mountView()
    await edit(wrapper)
    expect((wrapper.get('#edit-procurement-min').element as HTMLInputElement).value).toBe('0.02')
    expect((wrapper.get('#edit-procurement-max').element as HTMLInputElement).value).toBe('0.03')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toBeUndefined()
    expect(mocks.update.mock.calls[0][1].rate_multiplier).toBe(0.15)
  })

  it('sends a complete revision command, and clears bounds without clearing the revision', async () => {
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-min').setValue('')
    await wrapper.get('#edit-procurement-max').setValue('')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({ ...savedPolicy, min_rate: null, max_rate: null })
    expect(savedPolicy).toEqual({ version: 1, revision: 1, min_rate: '0.02', max_rate: '0.03' })
  })

  it('blocks invalid intervals, while allowing the existing gate to be disabled', async () => {
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-min').setValue('0.03')
    expect(wrapper.get('#edit-procurement-min').attributes('aria-invalid')).toBe('true')
    await submit(wrapper)
    expect(mocks.update).not.toHaveBeenCalled()
    expect(mocks.showError).toHaveBeenCalledWith('admin.groups.profitControl.procurement.emptyRange')
    await procurementToggle(wrapper, '#edit-group-form').setValue(false)
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].profit_control_enabled).toBe(false)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toBeUndefined()
  })

  it.each([null, { ...savedPolicy, version: 2 }])('keeps legacy or unsupported policy %j on ordinary saves', async procurement_policy => {
    const wrapper = mountView({ ...base, procurement_policy })
    await edit(wrapper)
    if (procurement_policy) {
      expect(wrapper.get('#edit-procurement-min').attributes('disabled')).toBeDefined()
      expect(wrapper.find('#edit-procurement-sources-mode').exists()).toBe(false)
    }
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toBeUndefined()
  })

  it('preserves the draft after 409 and requires explicit review before rebasing the revision', async () => {
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    mocks.getById.mockResolvedValue({ ...base, procurement_policy: { ...savedPolicy, revision: 2, max_rate: '0.06' } })
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05')
    await submit(wrapper)
    expect(mocks.getById).toHaveBeenCalledWith(42)
    expect((wrapper.get('#edit-procurement-max').element as HTMLInputElement).value).toBe('0.05')
    expect(wrapper.findAll('fieldset').find(field => field.find('#edit-procurement-min').exists())!.text()).toContain('[0.02, 0.06)')
    await submit(wrapper)
    expect(mocks.update).toHaveBeenCalledTimes(1)
    await button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision').trigger('click')
    await submit(wrapper)
    expect(mocks.update).toHaveBeenCalledTimes(2)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...savedPolicy, revision: 2, max_rate: '0.05' })
  })

  it('retains the draft on refresh failure and permits a deliberate retry', async () => {
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    mocks.getById.mockRejectedValueOnce(new Error('fixture unavailable'))
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05')
    await submit(wrapper)
    expect(mocks.showError).toHaveBeenCalledWith('admin.groups.profitControl.procurement.refreshFailed')
    expect(button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision')).toBeUndefined()
    await button(wrapper, 'admin.groups.profitControl.procurement.reloadLatest').trigger('click')
    await flushPromises()
    expect(button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision')).toBeDefined()
    expect((wrapper.get('#edit-procurement-max').element as HTMLInputElement).value).toBe('0.05')
  })

  it('keeps unrelated 409 errors distinct from a procurement revision conflict', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_EXISTS', message: 'Group name already exists' })
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05')
    await submit(wrapper)
    expect(mocks.showError).toHaveBeenCalledWith('Group name already exists')
    expect(mocks.getById).not.toHaveBeenCalled()
    expect(button(wrapper, 'admin.groups.profitControl.procurement.reloadLatest')).toBeUndefined()
    expect((wrapper.get('#edit-procurement-max').element as HTMLInputElement).value).toBe('0.05')
  })

  it('does not adopt an unknown version returned during conflict recovery', async () => {
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    mocks.getById.mockResolvedValue({ ...base, procurement_policy: { ...savedPolicy, version: 2 } })
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05')
    await submit(wrapper)
    expect(button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision').attributes('disabled')).toBeDefined()
    await submit(wrapper)
    expect(mocks.update).toHaveBeenCalledTimes(1)
  })

  it('creates an optional first revision in the ordinary form and resets it after closing', async () => {
    const wrapper = mountView()
    await flushPromises()
    await button(wrapper, 'admin.groups.createGroup').trigger('click')
    await wrapper.get('#create-group-form input[type="text"]').setValue('New product')
    await procurementToggle(wrapper, '#create-group-form').setValue(true)
    await wrapper.get('#create-procurement-min').setValue('0.02')
    await wrapper.get('#create-procurement-max').setValue('0.03')
    await wrapper.get('#create-group-form').trigger('submit')
    await flushPromises()
    expect(mocks.create.mock.calls[0][0].procurement_policy).toEqual({ ...savedPolicy, revision: 0 })
    await button(wrapper, 'admin.groups.createGroup').trigger('click')
    await procurementToggle(wrapper, '#create-group-form').setValue(true)
    expect((wrapper.get('#create-procurement-min').element as HTMLInputElement).value).toBe('')
  })

  it('saves approved sources through the original editor without changing the sale price', async () => {
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-sources-mode').setValue('approved')
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.sourcesEmpty')
    await wrapper.get('#edit-procurement-sources').setValue('901, 902\n903')
    await wrapper.get('#edit-procurement-max').setValue('0.04')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({ ...savedPolicy, version: 2, max_rate: '0.04', sources: { account_ids: [901, 902, 903] } })
    expect(mocks.update.mock.calls[0][1].rate_multiplier).toBe(0.15)
  })

  it('preserves loaded sources during range edits, and requires explicit null to restore the original pool', async () => {
    const sources = { account_ids: [901, 902] }
    const wrapper = mountView({ ...base, procurement_policy: { ...savedPolicy, version: 2, sources } })
    await edit(wrapper)
    expect((wrapper.get('#edit-procurement-sources').element as HTMLTextAreaElement).value).toBe('901, 902')
    await wrapper.get('#edit-procurement-max').setValue('0.04')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy.sources).toEqual(sources)
    expect(sources.account_ids).toEqual([901, 902])
    await edit(wrapper)
    await wrapper.get('#edit-procurement-sources-mode').setValue('original')
    await submit(wrapper)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...savedPolicy, version: 2, sources: { account_ids: null } })
  })

  it('rejects malformed or duplicate IDs without losing input, and persists an intentional empty list', async () => {
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-sources-mode').setValue('approved')
    for (const text of ['901, 901', '1e2', '9007199254740992', 'not-an-id']) {
      await wrapper.get('#edit-procurement-sources').setValue(text)
      expect((wrapper.get('#edit-procurement-sources').element as HTMLTextAreaElement).value).toBe(text)
      await submit(wrapper)
      expect(mocks.update).not.toHaveBeenCalled()
      expect(mocks.showError).toHaveBeenCalledWith('admin.groups.profitControl.procurement.sourceFormat')
      expect(wrapper.findAll('p[role="status"]').some(p => p.text().includes('procurement.sourcesCount'))).toBe(false)
    }
    await wrapper.get('#edit-procurement-sources').setValue('')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy.sources.account_ids).toEqual([])
  })

  it('keeps sources added by a newer revision when explicitly reviewing an older range-only draft', async () => {
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    mocks.getById.mockResolvedValue({ ...base, procurement_policy: { ...savedPolicy, version: 2, revision: 2, sources: { account_ids: [904] } } })
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05')
    await submit(wrapper)
    expect(wrapper.text()).toContain('904')
    await button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision').trigger('click')
    expect((wrapper.get('#edit-procurement-sources').element as HTMLTextAreaElement).value).toBe('904')
    await submit(wrapper)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...savedPolicy, version: 2, revision: 2, max_rate: '0.05', sources: { account_ids: [904] } })
  })

  it('keeps an approved-source draft on save failure and explicitly reviewed conflicts', async () => {
    mocks.update.mockRejectedValueOnce({ status: 503, message: 'fixture_save_unavailable' })
    const wrapper = mountView({ ...base, procurement_policy: { ...savedPolicy, version: 2, sources: { account_ids: [901] } } })
    await edit(wrapper)
    await wrapper.get('#edit-procurement-sources').setValue('902, 903')
    await submit(wrapper)
    expect((wrapper.get('#edit-procurement-sources').element as HTMLTextAreaElement).value).toBe('902, 903')
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    mocks.getById.mockResolvedValue({ ...base, procurement_policy: { ...savedPolicy, version: 2, revision: 2, sources: { account_ids: [905] } } })
    await submit(wrapper)
    expect((wrapper.get('#edit-procurement-sources').element as HTMLTextAreaElement).value).toBe('902, 903')
    expect(wrapper.text()).toContain('905')
    await button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision').trigger('click')
    await submit(wrapper)
    expect(mocks.update.mock.calls[2][1].procurement_policy).toEqual({ ...savedPolicy, version: 2, revision: 2, sources: { account_ids: [902, 903] } })
  })
})
