import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import View from '../ProcurementSharedMonitoringPreview.vue'
import Select from '@/components/common/Select.vue'
import { configureProcurementSharedMonitoringMaintenance, readProcurementSharedMonitoring, applyProcurementSharedMonitoring, previewProcurementSharedMonitoring, type ProcurementSharedMonitoringPreview } from '@/api/admin/procurementCatalog'
import type { MonitoringProbeCoverage } from '@/types/monitoringProbeBudget'
import { validProcurementSharedMonitoringPreview, validProcurementSharedMonitoringState } from '@/utils/procurementProductImpact'

vi.mock('vue-i18n', async importOriginal => ({ ...(await importOriginal<typeof import('vue-i18n')>()), useI18n: () => ({ t: (key: string, values?: Record<string, unknown>) => `${key}${values ? ' ' + JSON.stringify(values) : ''}` }) }))
vi.mock('@/api/admin/procurementCatalog', () => ({ configureProcurementSharedMonitoringMaintenance: vi.fn(), readProcurementSharedMonitoring: vi.fn(), applyProcurementSharedMonitoring: vi.fn(), previewProcurementSharedMonitoring: vi.fn() }))
const selections = [{ product_id: 2, preset_id: 7 }, { product_id: 3, preset_id: 8 }]
const fixture = (): ProcurementSharedMonitoringPreview => ({
  version: 1, scope: 'approved_sources_only', observed_at: '2026-11-21T09:00:00Z', state: 'compatible',
  products: selections.map(s => ({ ...s, name: `Product ${s.product_id}`, source_revision: 1, state: 'approved_sources', accounts: 1 })),
  accounts: [{ account_id: 9, name: 'Shared supply', product_ids: [2, 3], models: ['a', 'b', 'c'], cron_expression: '0,30 * * * *', state: 'compatible', reason: 'compatible' }],
  requirements: 2, shared_accounts: 1, current_minimum_requests_per_day: 0, delta_minimum_requests_per_day: 288,
  coverage: { observed_at: '2026-11-21T09:00:00Z', budget_revision: 2, minimum_total_requests_per_day: 288, regular_day_capacity: 1000, enforced: true, can_save: true,
    blocking_reasons: [], warnings: [], inventory_complete: true, plan_inventory_complete: true,
    schedule_window_start: '2026-11-21T00:00:00Z', schedule_window_end: '2026-11-22T00:00:00Z', unestimated_demand_sources: [] } as unknown as MonitoringProbeCoverage
})
const wrappers: ReturnType<typeof mount>[] = []
function view() {
  const w = mount(View, { props: { products: [{ id: 2, name: 'Product 2' }, { id: 3, name: 'Product 3' }], presets: [{ id: 7, name: 'Preset 7' }, { id: 8, name: 'Preset 8' }] as never, loading: false, loadError: false, disabled: false } })
  wrappers.push(w); return w
}
async function choose(w: ReturnType<typeof mount>) {
  (w.get('details').element as HTMLDetailsElement).open = true
  await w.get('details').trigger('toggle'); await flushPromises()
  w.findAllComponents(Select)[0]!.vm.$emit('update:modelValue', 2)
  w.findAllComponents(Select)[1]!.vm.$emit('update:modelValue', 3)
  await w.get('#shared-preset-0').setValue(7); await w.get('#shared-preset-1').setValue(8)
  await nextTick()
}
beforeEach(() => { vi.mocked(readProcurementSharedMonitoring).mockResolvedValue({ version: 1, revision: 0, observed_at: '2026-11-21T09:00:00Z', products: [], plans: [] }) })
async function preview(w: ReturnType<typeof mount>) { await w.get('form').trigger('submit'); await flushPromises() }
afterEach(() => { for (const w of wrappers.splice(0)) w.unmount(); vi.useRealTimers(); vi.resetAllMocks() })

describe('shared monitoring preview', () => {
  it('requires explicit complete selections, requests no API on mount and renders structured conclusions', async () => {
    const w = view(); expect(previewProcurementSharedMonitoring).not.toHaveBeenCalled()
    expect(w.get('[data-testid="shared-preview"]').attributes('disabled')).toBeDefined()
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue(fixture())
    await choose(w); await preview(w)
    expect(previewProcurementSharedMonitoring).toHaveBeenCalledWith(selections, expect.any(AbortSignal), false)
    expect(w.get('[data-testid="shared-conclusion"]').text()).toContain('states.compatible')
    expect(w.get('[data-testid="shared-shared"]').text()).toBe('1')
    expect(w.get('[data-testid="shared-accounts"]').text()).toBe('1')
    expect(w.get('[data-testid="shared-account-result"]').text()).toContain('a, b, c')
    expect(w.get('[data-testid="shared-account-result"]').text()).toContain('0,30 * * * *')
    expect(w.get('[data-testid="shared-budget"]').text()).toContain('budget.fits')
    expect(w.findAll('[data-testid="shared-product-result"]')).toHaveLength(2)
  })
  it('blocks duplicate products and resets old results when edited', async () => {
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue(fixture())
    const w = view(); await choose(w); await preview(w)
    w.findAllComponents(Select)[1]!.vm.$emit('update:modelValue', 2); await nextTick()
    expect(w.find('[data-testid="shared-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="shared-preview"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('sharedMonitoring.duplicate')
    await w.get('form').trigger('submit'); expect(previewProcurementSharedMonitoring).toHaveBeenCalledTimes(1)
  })
  it.each(['blocked', 'off', 'partial', 'unknown', 'conflict', 'incompatible'])('keeps %s conclusions separate', async kind => {
    const data = fixture()
    if (kind === 'blocked') { data.coverage!.can_save = false; data.coverage!.blocking_reasons = ['known_demand_exceeds_day_capacity'] }
    if (kind === 'off') data.coverage!.enforced = false
    if (kind === 'partial') data.coverage!.warnings = ['unestimated']
    if (kind === 'unknown') {
      data.products[1]!.state = 'sources_unavailable'; data.products[1]!.accounts = null; data.accounts[0]!.product_ids = [2]
      data.requirements = 1; data.shared_accounts = 0; data.state = 'incomplete'; data.coverage = undefined
      data.current_minimum_requests_per_day = null; data.delta_minimum_requests_per_day = null
    }
    if (kind === 'conflict') { data.state = 'conflict'; Object.assign(data.accounts[0]!, { state: 'conflict', reason: 'existing_observation_plan', existing_plan_id: 11, existing_enabled: false }) }
    if (kind === 'incompatible') {
      data.state = 'conflict'; Object.assign(data.accounts[0]!, { state: 'conflict', reason: 'observation_settings_conflict', models: [], cron_expression: '' })
      data.coverage = undefined; data.current_minimum_requests_per_day = null; data.delta_minimum_requests_per_day = null
    }
    expect(validProcurementSharedMonitoringPreview(data, selections)).toBe(true)
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue(data)
    const w = view(); await choose(w); await preview(w)
    expect(w.find('[data-testid="shared-error"]').exists()).toBe(false)
    if (kind === 'conflict') { expect(w.get('[data-testid="shared-conclusion"]').text()).toContain('states.conflict'); expect(w.text()).toContain('sharedMonitoring.paused') }
    else expect(w.get('[data-testid="shared-budget"]').text()).toContain(`budget.${kind === 'incompatible' ? 'unknown' : kind}`)
  })
  it.each([403, 503])('retains selections after %s and supports explicit retry', async status => {
    vi.mocked(previewProcurementSharedMonitoring).mockRejectedValueOnce({ status }).mockResolvedValue(fixture())
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="shared-error"]').text()).toContain(status === 403 ? 'forbidden' : 'failed')
    expect(w.find('[data-testid="shared-result"]').exists()).toBe(false)
    await preview(w); expect(w.find('[data-testid="shared-error"]').exists()).toBe(false)
    expect(previewProcurementSharedMonitoring).toHaveBeenLastCalledWith(selections, expect.any(AbortSignal), false)
  })
  it('ignores late responses after an edit and aborts on unmount', async () => {
    let resolve!: (r: ProcurementSharedMonitoringPreview) => void
    vi.mocked(previewProcurementSharedMonitoring).mockReturnValue(new Promise(r => { resolve = r }))
    const w = view(); await choose(w); await w.get('form').trigger('submit')
    const signal = vi.mocked(previewProcurementSharedMonitoring).mock.calls[0]![1]!
    expect(w.get('fieldset').attributes('disabled')).toBeDefined()
    w.findAllComponents(Select)[1]!.vm.$emit('update:modelValue', 2); await nextTick()
    expect(signal.aborted).toBe(true); resolve(fixture()); await flushPromises()
    expect(w.find('[data-testid="shared-result"]').exists()).toBe(false)
    w.unmount()
  })
  it('expires from request start, including a slow response', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] })
    let resolve!: (r: ProcurementSharedMonitoringPreview) => void
    vi.mocked(previewProcurementSharedMonitoring).mockReturnValue(new Promise(r => { resolve = r }))
    const w = view(); await choose(w); await w.get('form').trigger('submit')
    await vi.advanceTimersByTimeAsync(31000); resolve(fixture()); await flushPromises()
    expect(w.get('[data-testid="shared-conclusion"]').text()).toContain('states.expired')
    expect(w.get('[data-testid="shared-budget"]').text()).toContain('budget.expired')
    expect(w.get('[data-testid="shared-account-result"]').text()).toContain('reasons.expired')
  })
  it('bounds rows at twenty, preserves selected names across search and supports removal', async () => {
    const w = view(); await choose(w)
    await w.setProps({ products: [{ id: 4, name: 'Other result' }] })
    expect(w.findAllComponents(Select)[0]!.props('options')).toEqual([{ value: 4, label: 'Other result' }])
    expect(w.get('#shared-product-0').text()).toContain('Product 2')
    for (let i = 0; i < 18; i++) await w.get('[data-testid="shared-add"]').trigger('click')
    expect(w.findAll('[data-testid="shared-selection"]')).toHaveLength(20)
    expect(w.get('[data-testid="shared-add"]').attributes('disabled')).toBeDefined()
    await w.findAll('[data-testid="shared-remove"]')[0]!.trigger('click')
    expect(w.findAll('[data-testid="shared-selection"]')).toHaveLength(19)
  })
  it('reports invalid contracts without exposing stale results', async () => {
    const data = fixture(); data.version = 2
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue(data)
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="shared-error"]').text()).toContain('failed')
    expect(w.find('[data-testid="shared-result"]').exists()).toBe(false)
  })
})

describe('shared monitoring contract', () => {
  it.each([
    (r: ProcurementSharedMonitoringPreview) => { r.version = 2 },
    r => { r.scope = 'all_accounts' }, r => { r.observed_at = 'invalid' }, r => { r.state = 'compatible-but-unchecked' },
    r => { r.products[0]!.preset_id = 99 }, r => { r.products.reverse() }, r => { r.products[1]!.product_id = 2 },
    r => { r.products[1]!.accounts = null }, r => { r.products[1]!.state = 'legacy' }, r => { r.accounts.push({ ...r.accounts[0]! }) },
    r => { r.accounts[0]!.product_ids = [2, 2] }, r => { r.accounts[0]!.product_ids = [2, 8] }, r => { r.accounts[0]!.models = ['a', 'a'] },
    r => { r.accounts[0]!.cron_expression = '' }, r => { r.accounts[0]!.reason = 'unknown' }, r => { r.accounts[0]!.existing_plan_id = 9 },
    r => { r.requirements++ }, r => { r.shared_accounts++ }, r => { r.current_minimum_requests_per_day = null }, r => { r.delta_minimum_requests_per_day = -1 },
    r => { r.coverage!.minimum_total_requests_per_day = 0 }, r => { r.coverage!.can_save = false },
    r => { r.coverage!.schedule_window_end = r.coverage!.schedule_window_start }, r => { r.coverage!.observed_at = '2026-11-21T09:00:01Z' }
  ] satisfies Array<(r: ProcurementSharedMonitoringPreview) => void>)('rejects malformed or contradictory results %#', mutate => {
    const r = fixture(); mutate(r); expect(validProcurementSharedMonitoringPreview(r, selections)).toBe(false)
  })
})

function writable(): ProcurementSharedMonitoringPreview {
  return { ...fixture(), version: 2, application_review: {
    revision: 0, observed_at: '2026-11-21T09:00:00Z', expires_at: '2026-11-21T09:02:00Z', fingerprint: 'a'.repeat(64), can_confirm: true, release_ownership: false,
    changes: [{ account_id: 9, operation: 'create', reason: 'compatible' }]
  } }
}
const saved = () => ({ version: 1, revision: 1, observed_at: '2026-11-21T09:00:01Z', products: selections, plans: [{ account_id: 9, plan_id: 11, state: 'managed', enabled: true }] })
describe('shared monitoring persistence', () => {
  it.each([
    (r: ProcurementSharedMonitoringPreview) => { r.application_review!.revision = -1 },
    r => { r.application_review!.expires_at = r.observed_at }, r => { r.application_review!.observed_at = 'invalid' },
    r => { r.application_review!.fingerprint = 'A'.repeat(64) }, r => { r.application_review!.changes = [] },
    r => { r.application_review!.changes[0]!.operation = 'pause' }, r => { r.application_review!.changes[0]!.plan_id = 11 },
    r => { r.application_review!.changes[0]!.account_id = 10 }, r => { r.application_review!.changes.push({ ...r.application_review!.changes[0]! }) },
    r => { r.application_review!.release_ownership = true }, r => { r.coverage!.enforced = false }, r => { r.version = 3 }
  ] satisfies Array<(r: ProcurementSharedMonitoringPreview) => void>)('rejects malformed writable contracts %#', mutate => {
    const data = writable(); mutate(data); expect(validProcurementSharedMonitoringPreview(data, selections)).toBe(false)
  })
  it('accepts negative demand deltas only on verified version-two updates', () => {
    const data = writable(); data.current_minimum_requests_per_day = 500; data.delta_minimum_requests_per_day = -212
    expect(validProcurementSharedMonitoringPreview(data, selections)).toBe(true)
    data.version = 1; delete data.application_review
    expect(validProcurementSharedMonitoringPreview(data, selections)).toBe(false)
  })
  it.each([
    (s: ReturnType<typeof saved>) => { s.version = 2 }, s => { s.revision = 0 }, s => { s.observed_at = 'invalid' },
    s => { s.products.reverse() }, s => { s.products[0]!.preset_id = 0 }, s => { s.plans[0]!.plan_id = 0 },
    s => { s.plans[0]!.state = 'unknown-but-good' }, s => { s.plans[0]!.enabled = false }, s => { s.plans.push({ ...s.plans[0]! }) }
  ])('rejects malformed durable state %#', mutate => {
    const state = { ...saved(), products: selections.map(s => ({ ...s })) }; mutate(state)
    expect(validProcurementSharedMonitoringState(state)).toBe(false)
  })
  it('requires loading durable participation before any preview or save', async () => {
    vi.mocked(readProcurementSharedMonitoring).mockRejectedValue({ status: 503 })
    const w = view(); (w.get('details').element as HTMLDetailsElement).open = true
    await w.get('details').trigger('toggle'); await flushPromises(); await preview(w)
    expect(previewProcurementSharedMonitoring).not.toHaveBeenCalled()
    expect(applyProcurementSharedMonitoring).not.toHaveBeenCalled()
    expect(w.get('[data-testid="shared-error"]').text()).toContain('readFailed')
  })
  it('loads the complete saved participation, including products outside the search page', async () => {
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValue(saved())
    const w = view(); (w.get('details').element as HTMLDetailsElement).open = true
    await w.get('details').trigger('toggle'); await flushPromises()
    expect(w.findAll('[data-testid="shared-selection"]')).toHaveLength(2)
    expect(w.findAllComponents(Select).map(s => s.props('modelValue'))).toEqual([2, 3])
    expect(w.get('[data-testid="shared-saved-state"]').text()).toContain('"revision":1')
    expect(applyProcurementSharedMonitoring).not.toHaveBeenCalled()
  })
  it('saves once after consent, serializes edits and independently reads back exact ownership', async () => {
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue(writable())
    let finish!: (v: ReturnType<typeof saved>) => void
    vi.mocked(applyProcurementSharedMonitoring).mockReturnValue(new Promise(r => { finish = r }))
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="shared-save"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="shared-consent"]').setValue(true)
    await w.get('[data-testid="shared-save"]').trigger('click')
    expect(w.get('fieldset').attributes('disabled')).toBeDefined()
    expect(applyProcurementSharedMonitoring).toHaveBeenCalledTimes(1)
    expect(applyProcurementSharedMonitoring).toHaveBeenCalledWith({ version: 2, products: selections, release_ownership: false, revision: 0, observed_at: '2026-11-21T09:00:00Z', fingerprint: 'a'.repeat(64), confirm: true })
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValue(saved()); finish(saved()); await flushPromises()
    expect(readProcurementSharedMonitoring).toHaveBeenCalledTimes(2)
    expect(w.get('[data-testid="shared-success"]').text()).toContain('saved')
    expect(w.find('[data-testid="shared-save"]').exists()).toBe(false)
  })
  it.each(['lost', 'read', 'mismatch', 'conflict'])('requires a new read after %s without replaying the write', async mode => {
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue(writable())
    vi.mocked(applyProcurementSharedMonitoring).mockResolvedValue(saved())
    const w = view(); await choose(w); await preview(w)
    if (mode === 'lost' || mode === 'conflict') vi.mocked(applyProcurementSharedMonitoring).mockRejectedValue({ status: mode === 'conflict' ? 409 : 503 })
    else if (mode === 'read') vi.mocked(readProcurementSharedMonitoring).mockRejectedValue({ status: 503 })
    else vi.mocked(readProcurementSharedMonitoring).mockResolvedValue({ ...saved(), revision: 2 })
    await w.get('[data-testid="shared-consent"]').setValue(true); await w.get('[data-testid="shared-save"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="shared-success"]').exists()).toBe(false)
    expect(w.get('[data-testid="shared-error"]').text()).toContain(mode === 'conflict' ? 'conflict' : mode === 'lost' ? 'saveUnknown' : 'savedReadFailed')
    expect(w.find('[data-testid="shared-saved-state"]').exists()).toBe(false)
    expect(w.find('[data-testid="shared-state-unknown"]').exists()).toBe(true)
    await preview(w); expect(applyProcurementSharedMonitoring).toHaveBeenCalledTimes(1)
    expect(previewProcurementSharedMonitoring).toHaveBeenCalledTimes(1)
  })
  it('clears all participation only explicitly, and previews pause then release', async () => {
    const w = view(); await choose(w)
    await w.get('[data-testid="shared-clear"]').trigger('click')
    expect(w.findAll('[data-testid="shared-selection"]')).toHaveLength(0)
    await w.get('[data-testid="shared-release"]').setValue(true)
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue({ ...writable(), products: [], accounts: [], requirements: 0, shared_accounts: 0 })
    await preview(w)
    expect(previewProcurementSharedMonitoring).toHaveBeenCalledWith([], expect.any(AbortSignal), true)
  })
  it('clears consent on edit and expiry, and never enables saving a legacy preview', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] })
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue(writable())
    const w = view(); await choose(w); await preview(w)
    await w.get('[data-testid="shared-consent"]').setValue(true); await vi.advanceTimersByTimeAsync(31000)
    expect(w.get('[data-testid="shared-save"]').attributes('disabled')).toBeDefined()
    expect((w.get('[data-testid="shared-consent"]').element as HTMLInputElement).checked).toBe(false)
    vi.mocked(previewProcurementSharedMonitoring).mockResolvedValue(fixture()); await preview(w)
    expect(w.find('[data-testid="shared-save"]').exists()).toBe(false)
  })
})

function maintained(enabled = true, state: 'waiting' | 'current' | 'updated' | 'blocked' | 'manual' = 'waiting', reason = 'awaiting_tick') {
  return { ...saved(), revision: 2, maintenance: { enabled, state, reason, changed_at: '2026-11-21T09:00:02Z', created: 0, updated: 0, paused: 0 } }
}
describe('shared continuous maintenance', () => {
  it('requires consent and independently reads back the saved mode', async () => {
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValueOnce(saved()).mockResolvedValue(maintained())
    vi.mocked(configureProcurementSharedMonitoringMaintenance).mockResolvedValue(maintained())
    const w = view(); (w.get('details').element as HTMLDetailsElement).open = true
    await w.get('details').trigger('toggle'); await flushPromises()
    expect(w.get('[data-testid="shared-maintenance-toggle"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="shared-maintenance-consent"]').setValue(true)
    await w.get('[data-testid="shared-maintenance-toggle"]').trigger('click'); await flushPromises()
    expect(configureProcurementSharedMonitoringMaintenance).toHaveBeenCalledWith(true, 1)
    expect(w.get('[data-testid="shared-maintenance"]').attributes('data-state')).toBe('waiting')
    expect(w.get('[data-testid="shared-maintenance-verified"]').exists()).toBe(true)
    expect(readProcurementSharedMonitoring).toHaveBeenCalledTimes(2)
  })
  it('shows a concrete blocked outcome and disables future maintenance without pausing tests', async () => {
    const disabled = { ...maintained(false, 'manual', 'manual'), revision: 3 }
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValueOnce(maintained(true, 'blocked', 'budget_exceeded')).mockResolvedValue(disabled)
    vi.mocked(configureProcurementSharedMonitoringMaintenance).mockResolvedValue(disabled)
    const w = view(); (w.get('details').element as HTMLDetailsElement).open = true
    await w.get('details').trigger('toggle'); await flushPromises()
    expect(w.get('[data-testid="shared-maintenance-reason"]').text()).toContain('budget_exceeded')
    expect(w.find('[data-testid="shared-maintenance-consent"]').exists()).toBe(false)
    await w.get('[data-testid="shared-maintenance-toggle"]').trigger('click'); await flushPromises()
    expect(configureProcurementSharedMonitoringMaintenance).toHaveBeenCalledWith(false, 2)
    expect(w.get('[data-testid="shared-maintenance"]').attributes('data-state')).toBe('manual')
    expect(applyProcurementSharedMonitoring).not.toHaveBeenCalled()
  })
  it.each(['conflict', 'lost', 'read', 'contradiction'])('does not replay or claim success after %s', async mode => {
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValueOnce(saved())
    vi.mocked(configureProcurementSharedMonitoringMaintenance).mockResolvedValue(maintained())
    if (mode === 'conflict' || mode === 'lost') vi.mocked(configureProcurementSharedMonitoringMaintenance).mockRejectedValue({ status: mode === 'conflict' ? 409 : 503 })
    else if (mode === 'read') vi.mocked(readProcurementSharedMonitoring).mockRejectedValue({ status: 503 })
    else vi.mocked(readProcurementSharedMonitoring).mockResolvedValue(maintained(false, 'manual', 'manual'))
    const w = view(); (w.get('details').element as HTMLDetailsElement).open = true
    await w.get('details').trigger('toggle'); await flushPromises()
    await w.get('[data-testid="shared-maintenance-consent"]').setValue(true)
    await w.get('[data-testid="shared-maintenance-toggle"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="shared-maintenance-verified"]').exists()).toBe(false)
    expect(w.find('[data-testid="shared-maintenance-toggle"]').exists()).toBe(false)
    expect(w.find('[data-testid="shared-state-unknown"]').exists()).toBe(true)
    expect(configureProcurementSharedMonitoringMaintenance).toHaveBeenCalledTimes(1)
    expect(w.get('[data-testid="shared-error"]').exists()).toBe(true)
  })
  it.each([
    { enabled: 'true' }, { state: 'success' }, { reason: 'secret-upstream-response' }, { changed_at: 'invalid' },
    { created: -1 }, { updated: null }, { paused: 1001 }, { enabled: false },
  ])('rejects incompatible maintenance metadata %j', bad => {
    const v = maintained(); Object.assign(v.maintenance, bad)
    expect(validProcurementSharedMonitoringState(v)).toBe(false)
  })
})


describe('shared maintenance diagnosis and uncertain state recovery', () => {
  async function open(w: ReturnType<typeof mount>) {
    (w.get('details').element as HTMLDetailsElement).open = true
    await w.get('details').trigger('toggle'); await flushPromises()
  }
  it.each(['lost', 'conflict', 'forbidden', 'read', 'mismatch', 'invalid receipt'])('withdraws the previous current mode after %s and recovers only by reading', async mode => {
    const before = maintained(true, 'current', 'compatible')
    const after = { ...maintained(false, 'manual', 'manual'), revision: 3 }
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValueOnce(before).mockResolvedValue(after)
    vi.mocked(configureProcurementSharedMonitoringMaintenance).mockResolvedValue(after)
    if (['lost', 'conflict', 'forbidden'].includes(mode)) vi.mocked(configureProcurementSharedMonitoringMaintenance).mockRejectedValue({ status: mode === 'conflict' ? 409 : mode === 'forbidden' ? 403 : 503 })
    else if (mode === 'read') vi.mocked(readProcurementSharedMonitoring).mockRejectedValueOnce({ status: 503 }).mockResolvedValue(after)
    else if (mode === 'mismatch') vi.mocked(readProcurementSharedMonitoring).mockResolvedValueOnce(before).mockResolvedValue(after)
    else vi.mocked(configureProcurementSharedMonitoringMaintenance).mockResolvedValue(before)
    const w = view(); await open(w)
    expect(w.get('[data-testid="shared-maintenance"]').attributes('data-state')).toBe('current')
    await w.findAll('select')[0]!.setValue(8)
    await w.get('[data-testid="shared-maintenance-toggle"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="shared-maintenance"]').exists()).toBe(false)
    expect(w.find('[data-testid="shared-saved-state"]').exists()).toBe(false)
    expect(w.find('[data-testid="shared-state-unknown"]').exists()).toBe(true)
    expect(w.findAll('select')[0]!.element).toHaveProperty('value', '8')
    expect(w.get('fieldset').attributes('disabled')).toBeDefined()
    expect(configureProcurementSharedMonitoringMaintenance).toHaveBeenCalledTimes(1)
    expect(applyProcurementSharedMonitoring).not.toHaveBeenCalled()
    expect(previewProcurementSharedMonitoring).not.toHaveBeenCalled()
    await w.get('[data-testid="shared-reload"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="shared-state-unknown"]').exists()).toBe(false)
    expect(w.get('[data-testid="shared-maintenance"]').attributes('data-state')).toBe('manual')
    expect(w.get('[data-testid="shared-saved-state"]').text()).toContain('"revision":3')
    expect(configureProcurementSharedMonitoringMaintenance).toHaveBeenCalledTimes(1)
  })
  it('shows exact conflicting accounts and native plans with bounded pagination and zero writes', async () => {
    const state = maintained(true, 'blocked', 'managed_plan_changed')
    state.plans = Array.from({ length: 23 }, (_, i) => ({ account_id: i + 1, plan_id: 100 + i, state: ['managed_plan_changed', 'managed_plan_missing', 'managed_plan_running'][i % 3]!, enabled: i % 3 !== 1 }))
    state.plans.push({ account_id: 24, plan_id: 123, state: 'managed', enabled: true }, { account_id: 25, plan_id: 124, state: 'paused', enabled: false })
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValueOnce(state).mockResolvedValue({ ...state, plans: [{ account_id: 24, plan_id: 123, state: 'managed', enabled: true }] })
    const w = view(); await open(w)
    expect(w.get('[data-testid="shared-plan-issues"]').text()).toContain('"count":23')
    expect(w.get('[data-testid="shared-issues-observed"]').attributes('datetime')).toBe(state.observed_at)
    expect(w.findAll('[data-testid="shared-plan-issue"]')).toHaveLength(20)
    expect(w.get('[data-testid="shared-plan-issue"] a').attributes('href')).toBe('/admin/account-quality?plan_id=100')
    expect(w.get('[data-testid="shared-plan-issue"]').attributes('data-account-id')).toBe('1')
    expect(w.get('[data-testid="shared-plan-issue"]').attributes('data-state')).toBe('managed_plan_changed')
    await w.get('[data-testid="shared-issues-next"]').trigger('click')
    expect(w.findAll('[data-testid="shared-plan-issue"]')).toHaveLength(3)
    expect(w.get('[data-testid="shared-plan-issue"]').attributes('data-account-id')).toBe('21')
    expect(w.get('[data-testid="shared-issues-next"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="shared-issues-prev"]').trigger('click')
    expect(w.get('[data-testid="shared-plan-issue"]').attributes('data-account-id')).toBe('1')
    await w.get('[data-testid="shared-reload"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="shared-plan-issues"]').exists()).toBe(false)
    expect(configureProcurementSharedMonitoringMaintenance).not.toHaveBeenCalled()
    expect(applyProcurementSharedMonitoring).not.toHaveBeenCalled()
    expect(previewProcurementSharedMonitoring).not.toHaveBeenCalled()
  })
  it.each([
    ['budget_disabled', '/admin/settings?tab=gateway#settings-section-upstream-billing-probe'],
    ['budget_exceeded', '/admin/settings?tab=gateway#settings-section-upstream-billing-probe'],
    ['invalid_preset', '/admin/account-quality'], ['observation_settings_conflict', '/admin/account-quality'],
    ['native_model_limit', '/admin/account-quality'], ['schedule_timezones_conflict', '/admin/account-quality'],
    ['sources_unavailable', '/admin/groups'], ['product_missing', '/admin/groups'],
    ['too_many_accounts', '/admin/groups'], ['invalid_requirements', '/admin/groups'],
  ])('routes %s to existing recovery configuration', async (reason, href) => {
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValue(maintained(true, 'blocked', reason))
    const w = view(); await open(w)
    expect(w.get('[data-testid="shared-maintenance-recovery"] a').attributes('href')).toBe(href)
    expect(w.get('[data-testid="shared-maintenance-recovery"]').text()).toContain('maintenance.recovery')
    expect(configureProcurementSharedMonitoringMaintenance).not.toHaveBeenCalled()
    expect(applyProcurementSharedMonitoring).not.toHaveBeenCalled()
  })
  it('explains a transient retry without inventing missing objects or replaying writes', async () => {
    vi.mocked(readProcurementSharedMonitoring).mockResolvedValue(maintained(true, 'blocked', 'inputs_changed'))
    const w = view(); await open(w)
    expect(w.get('[data-testid="shared-maintenance-recovery"]').text()).toContain('maintenance.recovery.retry')
    expect(w.find('[data-testid="shared-maintenance-recovery"] a').exists()).toBe(false)
    expect(w.find('[data-testid="shared-plan-issues"]').exists()).toBe(false)
    expect(configureProcurementSharedMonitoringMaintenance).not.toHaveBeenCalled()
  })
})
