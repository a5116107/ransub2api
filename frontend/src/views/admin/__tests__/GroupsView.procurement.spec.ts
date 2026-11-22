import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AdminGroup, GroupProcurementPolicy } from '@/types'
import GroupsView from '@/views/admin/GroupsView.vue'

const mocks = vi.hoisted(() => ({
  list: vi.fn(), create: vi.fn(), update: vi.fn(), getById: vi.fn(), showError: vi.fn(), showSuccess: vi.fn(),
  getAll: vi.fn(), getModelAllowlistCandidates: vi.fn(), getUsageSummary: vi.fn(), getCapacitySummary: vi.fn(), getLiveCapability: vi.fn(), catalog: vi.fn()
}))
vi.mock('@/api/admin/procurementCatalog', () => ({ readProcurementCatalog: mocks.catalog }))
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
  mocks.catalog.mockResolvedValue({ catalog: { version: 1, revision: 4, buckets: [{ code: 'low', name: 'Low', min_rate: '0.02', max_rate: '0.03' }, { code: 'standard', name: 'Standard', min_rate: '0.03', max_rate: '0.05' }] }, gaps: [] })
})
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); vi.restoreAllMocks() })

describe('product procurement interval in the original Group editor', () => {
  it('selects approved source groups without self, inactive or foreign-platform sources and preserves other hard gates', async () => {
    mocks.getAll.mockResolvedValue([{ ...base, id: 701, name: 'Approved source A' }, { ...base, id: 702, name: 'Approved source B' }, base, { ...base, id: 703, status: 'inactive' }, { ...base, id: 704, platform: 'anthropic' }])
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-source-groups-mode').setValue('selected'); await flushPromises()
    expect(wrapper.find('input[data-source-group-id="42"]').exists()).toBe(false)
    expect(wrapper.find('input[data-source-group-id="703"]').exists()).toBe(false)
    expect(wrapper.find('input[data-source-group-id="704"]').exists()).toBe(false)
    await wrapper.get('input[data-source-group-id="701"]').setValue(true)
    await wrapper.get('input[data-source-group-id="702"]').setValue(true)
    await wrapper.get('#edit-procurement-availability').setValue('current_state')
    await wrapper.get('#edit-procurement-quality').setValue('current_answer')
    await wrapper.get('#edit-procurement-sources-mode').setValue('approved')
    await wrapper.get('#edit-procurement-sources').setValue('901')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({ ...savedPolicy, version: 6, source_group_ids: [701, 702], availability: 'current_state', quality: 'current_answer', sources: { account_ids: [901] }, buckets: { codes: null } })
    expect(mocks.update.mock.calls[0][1].rate_multiplier).toBe(.15)
  })
  it('preserves saved source-group restrictions on read failure and shows missing sources after recovery', async () => {
    mocks.getAll.mockRejectedValue(new Error('fixture source load failure'))
    const policy = { ...savedPolicy, version: 6, source_group_ids: [701], availability: 'current_state', quality: 'current_answer', sources: { account_ids: null }, buckets: { codes: null } }
    const wrapper = mountView({ ...base, procurement_policy: policy }); await edit(wrapper)
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.sourceGroupsReadFailed')
    mocks.getAll.mockResolvedValue([])
    await button(wrapper, 'admin.groups.profitControl.procurement.sourceGroupsReload').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.sourceGroupsMissing')
    await wrapper.get('#edit-procurement-max').setValue('0.04'); await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({ ...policy, max_rate: '0.04' })
  })
  it('inherits newly added source-group restrictions only after explicit conflict review', async () => {
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    const policy = { ...savedPolicy, version: 6, revision: 2, source_group_ids: [701], availability: 'current_state', quality: 'current_answer', sources: { account_ids: [904] }, buckets: { codes: ['low'] } }
    mocks.getById.mockResolvedValue({ ...base, procurement_policy: policy })
    const wrapper = mountView(); await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05'); await submit(wrapper)
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.sourceGroupsLatest')
    await button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision').trigger('click'); await flushPromises()
    await wrapper.get('#edit-procurement-availability').setValue('not_required'); await submit(wrapper)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...policy, max_rate: '0.05', availability: 'not_required' })
  })
  it('explicitly removes one missing source without clearing other approved sources', async () => {
    mocks.getAll.mockResolvedValue([{ ...base, id: 702, name: 'Still approved source' }])
    const policy = { ...savedPolicy, version: 6, source_group_ids: [701, 702], availability: 'current_state', quality: 'current_answer', sources: { account_ids: null }, buckets: { codes: null } }
    const wrapper = mountView({ ...base, procurement_policy: policy }); await edit(wrapper)
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.sourceGroupsMissing')
    await wrapper.get('button[data-remove-source-group-id="701"]').trigger('click')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({ ...policy, source_group_ids: [702] })
    expect(mocks.update.mock.calls[0][1].rate_multiplier).toBe(.15)
  })
  it('distinguishes explicitly empty approved source groups from explicit null', async () => {
    const wrapper = mountView(); await edit(wrapper)
    await wrapper.get('#edit-procurement-source-groups-mode').setValue('selected'); await flushPromises()
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.sourceGroupsEmpty')
    await submit(wrapper); expect(mocks.update.mock.calls[0][1].procurement_policy.source_group_ids).toEqual([])
    await edit(wrapper); await wrapper.get('#edit-procurement-source-groups-mode').setValue('original')
    await submit(wrapper); expect(mocks.update.mock.calls[1][1].procurement_policy.source_group_ids).toBeNull()
  })

  it('preserves v5 latest-state requirements while editing quality, sources and buckets', async () => {
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-availability').setValue('current_state')
    await wrapper.get('#edit-procurement-quality').setValue('current_answer')
    await wrapper.get('#edit-procurement-buckets-mode').setValue('selected')
    await flushPromises()
    await wrapper.get('input[data-bucket-code="low"]').setValue(true)
    await wrapper.get('#edit-procurement-sources-mode').setValue('approved')
    await wrapper.get('#edit-procurement-sources').setValue('901')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({ ...savedPolicy, version: 5, availability: 'current_state', quality: 'current_answer', sources: { account_ids: [901] }, buckets: { codes: ['low'] } })
  })
  it('keeps the latest-state constraint on explicit 409 review and only disables it explicitly', async () => {
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    const policy = { ...savedPolicy, version: 5, revision: 2, sources: { account_ids: [904] }, buckets: { codes: ['low'] }, quality: 'current_answer', availability: 'current_state' }
    mocks.getById.mockResolvedValue({ ...base, procurement_policy: policy })
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05')
    await submit(wrapper)
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.latestAvailability')
    await button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision').trigger('click')
    expect((wrapper.get('#edit-procurement-availability').element as HTMLSelectElement).value).toBe('current_state')
    await wrapper.get('#edit-procurement-quality').setValue('not_required')
    await submit(wrapper)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...policy, max_rate: '0.05', quality: 'not_required' })
    await edit(wrapper)
    await wrapper.get('#edit-procurement-availability').setValue('not_required')
    await submit(wrapper)
    expect(mocks.update.mock.calls[2][1].procurement_policy.availability).toBe('not_required')
  })
  it('saves an explicit answer gate without downgrading it when changing buckets or sources', async () => {
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-quality').setValue('current_answer')
    await wrapper.get('#edit-procurement-buckets-mode').setValue('selected')
    await flushPromises()
    await wrapper.get('input[data-bucket-code="low"]').setValue(true)
    await wrapper.get('#edit-procurement-sources-mode').setValue('approved')
    await wrapper.get('#edit-procurement-sources').setValue('901')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({ ...savedPolicy, version: 4, quality: 'current_answer', sources: { account_ids: [901] }, buckets: { codes: ['low'] } })
  })

  it('inherits a concurrent quality constraint only after explicit revision review', async () => {
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    mocks.getById.mockResolvedValue({ ...base, procurement_policy: { ...savedPolicy, version: 4, revision: 2, sources: { account_ids: [904] }, buckets: { codes: ['low'] }, quality: 'current_answer' } })
    const wrapper = mountView()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05')
    await submit(wrapper)
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.latestQuality')
    await button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision').trigger('click')
    expect((wrapper.get('#edit-procurement-quality').element as HTMLSelectElement).value).toBe('current_answer')
    await submit(wrapper)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...savedPolicy, version: 4, revision: 2, max_rate: '0.05', sources: { account_ids: [904] }, buckets: { codes: ['low'] }, quality: 'current_answer' })
  })

  it('ordinary edits preserve v4, and disabling the answer gate is explicit', async () => {
    const policy = { ...savedPolicy, version: 4, quality: 'current_answer', sources: { account_ids: null }, buckets: { codes: null } }
    const wrapper = mountView({ ...base, procurement_policy: policy })
    await edit(wrapper)
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toBeUndefined()
    await edit(wrapper)
    await wrapper.get('#edit-procurement-quality').setValue('not_required')
    await submit(wrapper)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...policy, quality: 'not_required' })
  })

  it('selects latest catalog buckets through the original editor and preserves source/range constraints', async () => {
    const wrapper = mountView()
    await edit(wrapper)
    expect(mocks.catalog).not.toHaveBeenCalled()
    await wrapper.get('#edit-procurement-buckets-mode').setValue('selected')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.bucketsEmpty')
    await wrapper.get('input[data-bucket-code="low"]').setValue(true)
    await wrapper.get('#edit-procurement-sources-mode').setValue('approved')
    await wrapper.get('#edit-procurement-sources').setValue('901')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({ ...savedPolicy, version: 3, sources: { account_ids: [901] }, buckets: { codes: ['low'] } })
    expect(mocks.update.mock.calls[0][1].rate_multiplier).toBe(.15)
  })

  it('preserves bucket constraints across catalog read failure and requires explicit null to remove them', async () => {
    const loaded = { ...savedPolicy, version: 3, sources: { account_ids: [901] }, buckets: { codes: ['low'] } }
    mocks.catalog.mockRejectedValueOnce(new Error('fixture catalog unavailable'))
    const wrapper = mountView({ ...base, procurement_policy: loaded })
    await edit(wrapper)
    expect(wrapper.text()).toContain('admin.groups.profitControl.procurement.bucketsReadFailed')
    await wrapper.get('#edit-procurement-max').setValue('0.04')
    await submit(wrapper)
    expect(mocks.update.mock.calls[0][1].procurement_policy.buckets).toEqual({ codes: ['low'] })
    expect(loaded.buckets.codes).toEqual(['low'])
    await edit(wrapper)
    await wrapper.get('#edit-procurement-buckets-mode').setValue('original')
    await submit(wrapper)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...loaded, buckets: { codes: null } })
  })

  it('explicit review preserves bucket constraints added after a source-only draft', async () => {
    mocks.update.mockRejectedValueOnce({ status: 409, reason: 'GROUP_PROCUREMENT_POLICY_CONFLICT' })
    mocks.getById.mockResolvedValue({ ...base, procurement_policy: { ...savedPolicy, version: 3, revision: 2, sources: { account_ids: [904] }, buckets: { codes: ['low'] } } })
    const wrapper = mountView({ ...base, procurement_policy: { ...savedPolicy, version: 2, sources: { account_ids: [901] } } })
    await edit(wrapper)
    await wrapper.get('#edit-procurement-max').setValue('0.05')
    await submit(wrapper)
    await button(wrapper, 'admin.groups.profitControl.procurement.useLatestRevision').trigger('click')
    await flushPromises()
    expect((wrapper.get('input[data-bucket-code="low"]').element as HTMLInputElement).checked).toBe(true)
    await submit(wrapper)
    expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({ ...savedPolicy, version: 3, revision: 2, max_rate: '0.05', sources: { account_ids: [901] }, buckets: { codes: ['low'] } })
  })

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


describe('pinned evidence templates in the original Group form',()=>{
 const fixed={...savedPolicy,version:7,sources:{account_ids:null},buckets:{codes:null},quality:'current_answer',availability:'current_state',source_group_ids:[701],evidence_template:{version:1,code:'answer_availability'}}
 it('requires an explicit approved source and saves the existing evidence gates together',async()=>{
  mocks.getAll.mockResolvedValue([{...base,id:701,name:'Approved supplier'}])
  const wrapper=mountView();await edit(wrapper)
  await wrapper.get('#edit-procurement-evidence-template').setValue('answer_availability');await flushPromises()
  expect(wrapper.get('#edit-procurement-quality').attributes('disabled')).toBeDefined();expect(wrapper.get('#edit-procurement-availability').attributes('disabled')).toBeDefined()
  expect(wrapper.get('#edit-procurement-source-groups-mode').attributes('disabled')).toBeDefined()
  await submit(wrapper);expect(mocks.update).not.toHaveBeenCalled();expect(mocks.showError).toHaveBeenCalledWith('admin.groups.profitControl.procurement.templateSources')
  await wrapper.get('input[data-source-group-id="701"]').setValue(true);await submit(wrapper)
  expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual(fixed)
 })
 it('returns to custom without silently disabling evidence or downgrading the policy version',async()=>{
  mocks.getAll.mockResolvedValue([{...base,id:701,name:'Approved supplier'}])
  const wrapper=mountView({...base,procurement_policy:fixed});await edit(wrapper)
  expect((wrapper.get('#edit-procurement-evidence-template').element as HTMLSelectElement).value).toBe('answer_availability')
  await wrapper.get('#edit-procurement-evidence-template').setValue('custom');await wrapper.get('#edit-procurement-quality').setValue('not_required');await wrapper.get('#edit-procurement-source-groups-mode').setValue('original');await submit(wrapper)
  expect(mocks.update.mock.calls[0][1].procurement_policy).toEqual({...fixed,quality:'not_required',source_group_ids:null,evidence_template:{version:1,code:'custom'}})
  expect(fixed.quality).toBe('current_answer')
 })
 it('retains the draft and explicitly inherits a newer pinned template after a revision conflict',async()=>{
  mocks.getAll.mockResolvedValue([{...base,id:701,name:'Approved supplier'}])
  const previous={...fixed,version:6,evidence_template:undefined,quality:'not_required',availability:'not_required'}
  mocks.update.mockRejectedValueOnce({status:409,reason:'GROUP_PROCUREMENT_POLICY_CONFLICT'});mocks.getById.mockResolvedValue({...base,procurement_policy:{...fixed,revision:2}})
  const wrapper=mountView({...base,procurement_policy:previous});await edit(wrapper);await wrapper.get('#edit-procurement-max').setValue('0.06');await submit(wrapper)
  await button(wrapper,'admin.groups.profitControl.procurement.reloadLatest').trigger('click');await flushPromises()
  expect(wrapper.get('[data-testid="procurement-latest-template"]').text()).toContain('templateMonitored')
  await button(wrapper,'admin.groups.profitControl.procurement.useLatestRevision').trigger('click');await submit(wrapper)
  expect(mocks.update.mock.calls[1][1].procurement_policy).toEqual({...fixed,revision:2,max_rate:'0.06'})
 })
})
