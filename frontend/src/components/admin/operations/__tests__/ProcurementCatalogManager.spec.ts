import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProcurementCatalogManager from '../ProcurementCatalogManager.vue'
import catalogSource from '../ProcurementCatalogManager.vue?raw'
import zhOperations from '@/i18n/locales/zh/upstreamOps'
import enOperations from '@/i18n/locales/en/upstreamOps'
import { readProcurementCatalog, saveProcurementCatalog, previewProcurementScopes, type ProcurementCatalogSnapshot, type ProcurementPreview, type ProcurementRequestScope } from '@/api/admin/procurementCatalog'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/procurementCatalog', () => ({ readProcurementCatalog: vi.fn(), saveProcurementCatalog: vi.fn(), previewProcurementScopes: vi.fn() }))
const fixture = (): ProcurementCatalogSnapshot => ({ catalog: { version: 1, revision: 1, buckets: [{ code: 'low', name: 'Low', min_rate: '0.02', max_rate: '0.03' }] }, gaps: [{ min_rate: '0', max_rate: '0.02' }, { min_rate: '0.03', max_rate: null }] })
const wrappers: ReturnType<typeof mount>[] = []
type View = ReturnType<typeof mount>
const prefix = 'upstreamOps.procurement.'
async function open() {
  const w = mount(ProcurementCatalogManager); wrappers.push(w)
  expect(readProcurementCatalog).not.toHaveBeenCalled()
  ;(w.element as HTMLDetailsElement).open = true
  await w.trigger('toggle'); await flushPromises()
  return w
}
async function setupProbe(w: View) {
  await w.get('[data-testid="procurement-account"]').setValue(7)
  await w.get('[data-testid="procurement-public-model"]').setValue('public-model')
  await w.get('[data-testid="procurement-wire-model"]').setValue('wire-model')
}
const submitSave = (w: View) => w.findAll('form')[0]!.trigger('submit')
const submitPreview = (w: View) => w.findAll('form')[1]!.trigger('submit')
beforeEach(() => {
  vi.resetAllMocks()
  vi.mocked(readProcurementCatalog).mockResolvedValue(fixture())
  vi.mocked(saveProcurementCatalog).mockImplementation(async value => ({ catalog: { ...structuredClone(value), revision: value.revision + 1 }, gaps: [] }))
  vi.mocked(previewProcurementScopes).mockImplementation(async ({ requests }) => ({ ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(r => ({ ...r, state: 'classified', applicability: 'covered', rate_source: 'upstream_probe_sync', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated' })) }))
})
afterEach(() => { for (const w of wrappers.splice(0)) w.unmount() })

describe('catalog editing and scope classification in the original operations page', () => {
  it('previews product constraints separately and clears them when the selected product changes', async () => {
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockResolvedValueOnce({ ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: [{
      account_id: 7, public_model: 'public-model', scope: { version: 2, model: 'wire-model', protocol: 'responses_http', stream: false, service_tier: 'unspecified', reasoning_effort: 'unspecified', capability: 'text' },
      state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated',
      product_procurement: { version: 1, group_id: 42, policy_revision: 3, price_basis: 'group_default', default_d: 0.15, threshold: 0.12, state: 'blocked', reason: 'rejected_procurement_source', price_class: 'rejected_procurement_source' }
    }] })
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="product-procurement-state"]').text()).toContain(prefix + 'productStates.blocked')
    expect(w.text()).toContain('rejected_procurement_source')
    expect(w.text()).toContain(prefix + 'productDefaultBasis')
    await w.get('[data-testid="procurement-product"]').setValue('43')
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
  })
  it('resolves every static catalog label in both languages instead of displaying raw translation keys', () => {
    const labels = [...catalogSource.matchAll(/\$\{p\}\.([a-zA-Z]+)`/g)].map(match => match[1]!)
    expect(labels.length).toBeGreaterThan(30)
    for (const messages of [zhOperations.procurement, enOperations.procurement]) {
      for (const label of labels) expect(typeof (messages as Record<string, unknown>)[label], label).toBe('string')
      for (const state of ['constraints_passed', 'blocked', 'disabled', 'product_inactive', 'not_associated', 'not_evaluated']) {
        expect(typeof (messages.productStates as Record<string, unknown>)[state], state).toBe('string')
      }
    }
  })
  it('omits an unselected product and rejects invalid product IDs without sending a preview', async () => {
    const w = await open(); await setupProbe(w)
    for (const value of ['0', '-1', '1e2', '1.5', '9007199254740992']) {
      await w.get('[data-testid="procurement-product"]').setValue(value)
      await submitPreview(w)
      expect(previewProcurementScopes).not.toHaveBeenCalled()
      expect(w.get('[data-testid="catalog-preview"]').attributes('disabled')).toBeDefined()
    }
    await w.get('[data-testid="procurement-product"]').setValue('')
    await submitPreview(w); await flushPromises()
    expect(vi.mocked(previewProcurementScopes).mock.calls[0]![0]).not.toHaveProperty('product_group_id')
  })
  it.each(['missing', 'future', 'wrong_product', 'unknown_state', 'wrong_basis', 'invalid_revision', 'invalid_rate', 'contradictory_pass'])('does not accept unknown product contracts (%s)', async kind => {
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => {
      const projection = { version: 1, group_id: 42, policy_revision: 3, price_basis: 'group_default', default_d: .15, threshold: .12, state: 'constraints_passed', price_class: 'admitted' }
      if (kind === 'future') projection.version = 99
      if (kind === 'wrong_product') projection.group_id = 43
      if (kind === 'unknown_state') projection.state = 'unknown_future_pass'
      if (kind === 'wrong_basis') projection.price_basis = 'user_price'
      if (kind === 'invalid_revision') projection.policy_revision = -1
      if (kind === 'invalid_rate') projection.default_d = NaN
      if (kind === 'contradictory_pass') projection.price_class = 'rejected_procurement_source'
      return { ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request, state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated', ...(kind === 'missing' ? {} : { product_procurement: projection }) })) }
    })
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'previewFailed')
  })
  it('loads on expansion, displays structured gaps, preserves decimal strings and disables previews for unsaved drafts', async () => {
    const w = await open()
    expect(w.get('[data-testid="procurement-gaps"]').text()).toContain('[0.03, ∞)')
    await w.get('[data-testid="bucket-max"]').setValue('0.05000000')
    expect(w.findAll('fieldset')[1]!.attributes('disabled')).toBeDefined()
    await submitPreview(w); expect(previewProcurementScopes).not.toHaveBeenCalled()
    await submitSave(w); await flushPromises()
    expect(saveProcurementCatalog).toHaveBeenCalledWith({ version: 1, revision: 1, buckets: [{ code: 'low', name: 'Low', min_rate: '0.02', max_rate: '0.05000000' }] }, expect.any(AbortSignal))
    expect(w.text()).toContain(prefix + 'saved')
    expect(w.get('[data-testid="catalog-save"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="catalog-reload"]').trigger('click'); await flushPromises()
    expect((w.get('[data-testid="bucket-max"]').element as HTMLInputElement).value).toBe('0.03')
  })
  it('requires an explicit open upper bound; empty upper inputs never silently allow every rate', async () => {
    const w = await open()
    await w.get('[data-testid="bucket-max"]').setValue('')
    expect(w.get('[data-testid="procurement-validation"]').text()).toContain(prefix + 'invalid')
    await submitSave(w); expect(saveProcurementCatalog).not.toHaveBeenCalled()
    await w.get('[data-testid="bucket-unbounded"]').setValue(true)
    await submitSave(w); await flushPromises()
    expect(vi.mocked(saveProcurementCatalog).mock.calls[0]![0].buckets[0]!.max_rate).toBeNull()
  })
  it('retains a conflicting draft, survives latest-read failure and requires explicit review before rebasing', async () => {
    const w = await open()
    await w.get('[data-testid="bucket-name"]').setValue('My draft')
    vi.mocked(saveProcurementCatalog).mockRejectedValueOnce({ status: 409, reason: 'PROCUREMENT_CATALOG_CONFLICT' })
    await submitSave(w); await flushPromises()
    expect(w.find('[data-testid="catalog-conflict"]').exists()).toBe(true)
    await submitSave(w); expect(saveProcurementCatalog).toHaveBeenCalledTimes(1)
    vi.mocked(readProcurementCatalog).mockRejectedValueOnce(new Error('offline'))
    await w.get('[data-testid="catalog-read-latest"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="catalog-review"]').exists()).toBe(false)
    expect((w.get('[data-testid="bucket-name"]').element as HTMLInputElement).value).toBe('My draft')
    const next = fixture(); next.catalog.revision = 8; next.catalog.buckets[0]!.name = 'Other administrator'
    vi.mocked(readProcurementCatalog).mockResolvedValue(next)
    await w.get('[data-testid="catalog-read-latest"]').trigger('click'); await flushPromises()
    expect(w.text()).toContain('Other administrator')
    expect((w.get('[data-testid="bucket-name"]').element as HTMLInputElement).value).toBe('My draft')
    await w.get('[data-testid="catalog-review"]').trigger('click')
    await submitSave(w); await flushPromises()
    expect(vi.mocked(saveProcurementCatalog).mock.calls[1]![0]).toMatchObject({ revision: 8, buckets: [{ name: 'My draft' }] })
  })
  it.each([{ status: 409, reason: 'OTHER_CONFLICT' }, { status: 503 }, { response: { status: 409 }, reason: 'PROCUREMENT_CATALOG_CONFLICT' }])('preserves draft without misclassifying other failures (%o)', async failure => {
    const w = await open(); await w.get('[data-testid="bucket-name"]').setValue('Preserve')
    vi.mocked(saveProcurementCatalog).mockRejectedValueOnce(failure)
    await submitSave(w); await flushPromises()
    expect(w.find('[data-testid="catalog-conflict"]').exists()).toBe(false)
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'saveFailed')
    expect((w.get('[data-testid="bucket-name"]').element as HTMLInputElement).value).toBe('Preserve')
  })
  it('uses explicit wire scope, displays conclusions without awarding quality, and clears stale results on input changes', async () => {
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-verbosity"]').setValue('medium')
    await submitPreview(w); await flushPromises()
    expect(vi.mocked(previewProcurementScopes).mock.calls[0]![0].requests[0]).toMatchObject({ account_id: 7, public_model: 'public-model', scope: { version: 4, model: 'wire-model', protocol: 'responses_http', stream: false, text_verbosity: 'medium' } })
    expect(w.get('[data-testid="catalog-preview-state"]').text()).toBe(prefix + 'states.classified')
    expect(w.get('[data-testid="catalog-preview-result"]').text()).toContain(prefix + 'notQualified')
    await w.get('[data-testid="procurement-tier"]').setValue('priority')
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
    await w.get('[data-testid="procurement-protocol"]').setValue('responses_ws_v2')
    await submitPreview(w); await flushPromises()
    expect(vi.mocked(previewProcurementScopes).mock.calls[1]![0].requests[0]!.scope.stream).toBe(true)
    await w.get('[data-testid="procurement-protocol"]').setValue('chat_http')
    await submitPreview(w); await flushPromises()
    expect(vi.mocked(previewProcurementScopes).mock.calls[2]![0].requests[0]!.scope).not.toHaveProperty('text_verbosity')
  })
  it('hides late previews and unknown catalog responses instead of treating them as current', async () => {
    const w = await open(); await setupProbe(w)
    let resolve!: (value: ProcurementPreview) => void
    let input!: ProcurementRequestScope[]
    vi.mocked(previewProcurementScopes).mockImplementationOnce(({ requests }) => { input = requests; return new Promise<ProcurementPreview>(done => { resolve = done }) })
    await submitPreview(w)
    await w.get('[data-testid="procurement-tier"]').setValue('priority')
    resolve({ ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: [{ ...input[0]!, state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated' }] })
    await flushPromises(); expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
    const future = fixture(); future.catalog.version = 99
    vi.mocked(readProcurementCatalog).mockResolvedValueOnce(future)
    await w.get('[data-testid="catalog-reload"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'readFailed')
    expect(w.findAll('[data-testid="procurement-bucket"]')).toHaveLength(1)
  })
  it('explicitly clears all buckets without editing any account or sale-group fields', async () => {
    const w = await open()
    await w.findAll('button').find(b => b.text() === prefix + 'remove')!.trigger('click')
    await submitSave(w); await flushPromises()
    expect(vi.mocked(saveProcurementCatalog).mock.calls[0]![0]).toEqual({ version: 1, revision: 1, buckets: [] })
  })
})
