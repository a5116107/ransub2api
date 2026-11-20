import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProcurementProductImpactView from '../ProcurementProductImpact.vue'
import Select from '@/components/common/Select.vue'
import { list as listGroups } from '@/api/admin/groups'
import { previewProcurementProductImpact, type ProcurementProductImpact } from '@/api/admin/procurementCatalog'
import { validProcurementProductImpact } from '@/utils/procurementProductImpact'
import zh from '@/i18n/locales/zh/upstreamOps'
import en from '@/i18n/locales/en/upstreamOps'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, values?: Record<string, unknown>) => `${key}${values ? ' ' + JSON.stringify(values) : ''}` }) }))
vi.mock('@/api/admin/groups', () => ({ list: vi.fn() }))
vi.mock('@/api/admin/procurementCatalog', () => ({ previewProcurementProductImpact: vi.fn() }))
const p = 'upstreamOps.productImpact.'
const wrappers: ReturnType<typeof mount>[] = []
const product = (id = 2) => ({ id, name: `Sales ${id}`, platform: 'openai', status: 'active' })
const fixture = (): ProcurementProductImpact => ({
  version: 1, observed_at: '2026-11-20T04:30:00Z', group_status: 'active', gate_enabled: false,
  bound_accounts: 3, considered_accounts: 2, user_override_count: 1,
  report: { group_id: 2, group_name: 'Fixed sale', platform: 'openai', effective_gate: true, assumed_enabled: true,
    default_d: .2, min_effective_d: .1, threshold_default: .17, threshold_min_d: .085, model_counts_scope: 'account_price_only',
    verdicts: [9, 14].map(id => ({ account_id: id, name: 'Supplier ' + id, platform: 'openai', class: 'rejected_threshold',
      account_rate: 1, effective_upstream_rate: 1, rate_source: 'manual', warnings: ['manual_rate_1_suspected_unmaintained'],
      estimated_cost_rate: .1, estimated_cost_source: 'saved' })) }
})
function view() { const w = mount(ProcurementProductImpactView); wrappers.push(w); return w }
async function choose(w: ReturnType<typeof mount>, id = 2) {
  w.getComponent(Select).vm.$emit('open'); await flushPromises()
  w.getComponent(Select).vm.$emit('update:modelValue', id); await flushPromises()
}
async function preview(w: ReturnType<typeof mount>) { await w.get('form').trigger('submit'); await flushPromises() }
beforeEach(() => {
  vi.resetAllMocks()
  vi.mocked(listGroups).mockResolvedValue({ items: [product()], total: 1 } as never)
  vi.mocked(previewProcurementProductImpact).mockResolvedValue(fixture())
})
afterEach(() => { wrappers.splice(0).forEach(w => w.unmount()) })

describe('administrator product price impact in the existing operations journey', () => {
  it('uses product names, checks read-only activation and separates U from C and D', async () => {
    const w = view()
    expect(listGroups).not.toHaveBeenCalled()
    expect(previewProcurementProductImpact).not.toHaveBeenCalled()
    await choose(w)
    expect(listGroups).toHaveBeenCalledWith(1, 100, { search: '', sort_by: 'name', sort_order: 'asc' }, { signal: expect.any(AbortSignal) })
    expect(w.getComponent(Select).props('options')).toEqual([{ value: 2, label: 'Sales 2 · openai #2' }])
    await preview(w)
    expect(previewProcurementProductImpact).toHaveBeenCalledWith(2, expect.any(AbortSignal))
    expect(w.get('[data-testid="product-impact-gate"]').text()).toContain(p + 'gateOff')
    expect(w.get('[data-testid="product-impact-conclusion"]').text()).toContain(p + 'allRejected')
    expect(w.findAll('[data-testid="product-impact-u"]').map(row => row.text())).toEqual(['1', '1'])
    expect(w.findAll('[data-testid="product-impact-c"]').map(row => row.text())).toEqual(['0.1', '0.1'])
    expect(w.text()).toContain(p + 'saleD')
    expect(w.text()).toContain(p + 'minimumD')
    expect(w.get('[data-testid="product-impact-boundary"]').text()).toContain(p + 'boundary')
  })
  it('keeps explicit zero distinct from missing U and flags lowest-user-price exclusion', async () => {
    const next = fixture(), first = next.report.verdicts[0]!, second = next.report.verdicts[1]!
    Object.assign(first, { effective_upstream_rate: 0, estimated_cost_rate: 0, class: 'admitted', rejected_under_min_d: true })
    delete second.effective_upstream_rate; second.class = 'rejected_upstream_rate_unavailable'
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.findAll('[data-testid="product-impact-u"]').map(row => row.text())).toEqual(['0', 'upstreamOps.unknown'])
    expect(w.get('[data-testid="product-impact-conclusion"]').text()).toContain(p + 'priceRemaining')
    expect(w.text()).toContain(p + 'overrideEmpty')
  })
  it.each(['empty', 'inactive', 'unsupported'])('explains the %s product without claiming live supply', async state => {
    const next = fixture()
    if (state === 'empty') { next.bound_accounts = 0; next.considered_accounts = 0; next.report.verdicts = [] }
    if (state === 'inactive') next.group_status = 'inactive'
    if (state === 'unsupported') Object.assign(next.report, { platform: 'composite', effective_gate: false, assumed_enabled: false })
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.text()).toContain(p + state)
    if (state === 'unsupported') expect(w.find('[data-testid="product-impact-conclusion"]').exists()).toBe(false)
  })
  it.each([403, 404, 503, 400])('retains selection after HTTP %s and recovers on retry', async status => {
    vi.mocked(previewProcurementProductImpact).mockRejectedValueOnce({ status, reason: status === 400 ? 'PROCUREMENT_PRODUCT_IMPACT_TOO_LARGE' : undefined })
    const w = view(); await choose(w); await preview(w)
    expect(w.find('[data-testid="product-impact-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="product-impact-error"]').text()).toContain(p + ({ 403: 'forbidden', 404: 'notFound', 503: 'failed', 400: 'tooLarge' }[status]))
    expect(w.getComponent(Select).props('modelValue')).toBe(2)
    await preview(w)
    expect(w.find('[data-testid="product-impact-result"]').exists()).toBe(true)
  })
  it('retains selected names and newer search results when an older request finishes late', async () => {
    const w = view(); await choose(w)
    let resolveOld!: (value: unknown) => void
    vi.mocked(listGroups).mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve as never }))
    w.getComponent(Select).vm.$emit('search', 'old'); await flushPromises()
    vi.mocked(listGroups).mockResolvedValueOnce({ items: [product(3)], total: 120 } as never)
    w.getComponent(Select).vm.$emit('search', 'new'); await flushPromises()
    resolveOld({ items: [product(99)], total: 1 }); await flushPromises()
    expect(w.getComponent(Select).props('options')).toEqual([
      { value: 2, label: 'Sales 2 · openai #2' }, { value: 3, label: 'Sales 3 · openai #3' }
    ])
    expect(w.text()).toContain(p + 'searchMore')
  })
  it('clears a prior result on product change and ignores the old in-flight preview', async () => {
    let resolve!: (value: ProcurementProductImpact) => void
    vi.mocked(previewProcurementProductImpact).mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const w = view(); await choose(w); await w.get('form').trigger('submit')
    expect(w.get('[data-testid="product-impact-preview"]').attributes('disabled')).toBeDefined()
    w.getComponent(Select).vm.$emit('update:modelValue', 3); await flushPromises()
    resolve(fixture()); await flushPromises()
    expect(w.find('[data-testid="product-impact-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="product-impact-preview"]').attributes('disabled')).toBeUndefined()
  })
  it('explains empty/failed product lists and retries without paid probing', async () => {
    vi.mocked(listGroups).mockRejectedValueOnce(new Error('fixture'))
    const w = view(); await choose(w)
    expect(w.text()).toContain(p + 'productsFailed')
    vi.mocked(listGroups).mockResolvedValueOnce({ items: [], total: 0 } as never)
    w.getComponent(Select).vm.$emit('search', 'none'); await flushPromises()
    expect(w.text()).toContain(p + 'noProducts')
    expect(previewProcurementProductImpact).not.toHaveBeenCalled()
  })
  it.each(['version', 'group', 'counts', 'nan', 'unknown-class', 'missing-u', 'missing-c', 'gate', 'string-flag', 'duplicate', 'scope'])('rejects an inconsistent %s response instead of rendering a pass', async kind => {
    const next = fixture()
    if (kind === 'version') next.version++
    if (kind === 'group') next.report.group_id++
    if (kind === 'counts') next.considered_accounts++
    if (kind === 'nan') next.report.default_d = NaN
    if (kind === 'unknown-class') next.report.verdicts[0]!.class = 'future-pass'
    if (kind === 'missing-u') { next.report.verdicts[0]!.class = 'admitted'; delete next.report.verdicts[0]!.effective_upstream_rate }
    if (kind === 'missing-c') delete (next.report.verdicts[0]! as Partial<typeof next.report.verdicts[0]>).estimated_cost_rate
    if (kind === 'gate') next.report.assumed_enabled = false
    if (kind === 'string-flag') Object.assign(next.report, { assumed_enabled: 'true' })
    if (kind === 'duplicate') next.report.verdicts[1]!.account_id = 9
    if (kind === 'scope') next.report.model_counts_scope = 'full_qualification'
    expect(validProcurementProductImpact(next, 2)).toBe(false)
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.find('[data-testid="product-impact-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="product-impact-error"]').text()).toContain(p + 'failed')
  })
  it('paginates all returned accounts without changing global totals', async () => {
    const next = fixture(); next.bound_accounts = 21; next.considered_accounts = 21
    next.report.verdicts = Array.from({ length: 21 }, (_, index) => ({ ...next.report.verdicts[0]!, account_id: index + 1 }))
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.findAll('[data-testid="product-impact-account"]')).toHaveLength(20)
    await w.get('nav button:last-child').trigger('click')
    expect(w.findAll('[data-testid="product-impact-account"]')).toHaveLength(1)
    expect(w.get('[data-testid="product-impact-conclusion"]').text()).toContain('"total":21')
  })
  it('has equivalent Chinese/English product terminology without conflating cost and admission', () => {
    expect(Object.keys(zh.productImpact).sort()).toEqual(Object.keys(en.productImpact).sort())
    expect(zh.productImpact.boundary).toContain('C 不会替代 U')
    expect(en.productImpact.boundary).toContain('C never replaces U')
    expect(Object.keys(zh.productImpact.classes).sort()).toEqual(Object.keys(en.productImpact.classes).sort())
  })
})
