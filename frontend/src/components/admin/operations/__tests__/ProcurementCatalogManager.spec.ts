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
afterEach(() => { for (const w of wrappers.splice(0)) w.unmount(); vi.useRealTimers() })

describe('catalog editing and scope classification in the original operations page', () => {
  it.each(['pass', 'fail', 'unknown', 'not_required'])('intersects explicitly evaluated product gates (%s)', async state => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-19T00:00:00Z'))
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => productPreview(requests, state))
    await submitPreview(w); await flushPromises()
    const want = state === 'not_required' ? 'pass' : state
    expect(w.get('[data-testid="product-evaluated-gates-status"]').text()).toContain(prefix + 'evaluatedGatesStates.' + want)
    expect(w.get('[data-testid="product-evaluated-gates"]').text()).toContain(prefix + 'evaluatedGatesBoundary')
    expect(w.text()).toContain(prefix + 'notQualified')
    await vi.advanceTimersByTimeAsync(300000)
    expect(w.get('[data-testid="product-evaluated-gates-status"]').text()).toContain(prefix + 'evaluatedGatesStates.unknown')
  })
  it.each(['scheduling_disabled', 'performance_paused', 'account_inactive', 'account_expired', 'account_overloaded', 'account_rate_limited', 'account_cooldown', 'account_quota_exceeded'])('keeps price and protection independent, then expires the protection snapshot (%s)', async reason => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-19T00:00:00Z'))
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => {
      const preview = productPreview(requests, 'unknown'), product = preview.results[0]!.product_procurement!
      Object.assign(product.account_controls!, { state: 'fail', reason })
      Object.assign(product.evaluated_gates!, { state: 'fail', reason: 'account_controls_failed', passed_gates: 1 })
      return preview
    })
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="product-procurement-state"]').text()).toContain(prefix + 'productStates.constraints_passed')
    expect(w.get('[data-testid="product-account-controls"]').text()).toContain(prefix + 'accountControlsStates.fail')
    expect(w.get('[data-testid="product-account-controls-reason"]').text()).toContain(prefix + 'accountControlsReasons.' + reason)
    expect(w.get('[data-testid="product-evaluated-gates-status"]').text()).toContain(prefix + 'evaluatedGatesStates.fail')
    await vi.advanceTimersByTimeAsync(30000)
    expect(w.get('[data-testid="product-account-controls"]').text()).toContain(prefix + 'accountControlsStates.unknown')
    expect(w.get('[data-testid="product-account-controls-reason"]').text()).toContain(prefix + 'accountControlsReasons.expired')
    expect(w.get('[data-testid="product-evaluated-gates-status"]').text()).toContain(prefix + 'evaluatedGatesStates.unknown')
  })
  it.each(['account_unavailable', 'account_state_unconfirmed'])('keeps missing or future account state unknown (%s)', async reason => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-19T00:00:00Z'))
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => {
      const preview = productPreview(requests, 'not_required'), p = preview.results[0]!.product_procurement!
      Object.assign(p.account_controls!, { state: 'unknown', reason, valid_until: null })
      Object.assign(p.evaluated_gates!, { state: 'unknown', reason: 'account_controls_unavailable', passed_gates: 1, valid_until: null })
      return preview
    })
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="product-account-controls"]').text()).toContain(prefix + 'accountControlsStates.unknown')
    expect(w.get('[data-testid="product-account-controls-reason"]').text()).toContain(prefix + 'accountControlsReasons.' + reason)
    expect(w.get('[data-testid="product-evaluated-gates-status"]').text()).toContain(prefix + 'evaluatedGatesStates.unknown')
  })
  it('does not upgrade a legacy price/evidence intersection into checked account controls', async () => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-19T00:00:00Z'))
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => {
      const preview = productPreview(requests, 'not_required'), p = preview.results[0]!.product_procurement!
      delete p.account_controls
      Object.assign(p.evaluated_gates!, { version: 1, passed_gates: 1, required_gates: 1, valid_until: null })
      return preview
    })
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(true)
    expect(w.get('[data-testid="product-evaluated-gates-status"]').text()).toContain(prefix + 'evaluatedGatesStates.unknown')
    expect(w.get('[data-testid="product-evaluated-gates"]').text()).toContain(prefix + 'evaluatedGatesLegacy')
    expect(w.get('[data-testid="product-account-controls-reason"]').text()).toContain(prefix + 'accountControlsReasons.not_checked')
  })
  it.each(['missing', 'version', 'time', 'deadline', 'too-long', 'reason', 'contradiction', 'missing-expiry', 'mixed-versions'])('rejects malformed account controls and recovers on retry (%s)', async kind => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-19T00:00:00Z'))
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => {
      const preview = productPreview(requests, 'pass'), p = preview.results[0]!.product_procurement!, c = p.account_controls!
      if (kind === 'missing') delete p.account_controls
      if (kind === 'version') c.version++
      if (kind === 'time') c.evaluated_at = '2999-01-01T00:00:00Z'
      if (kind === 'deadline') c.valid_until = c.evaluated_at
      if (kind === 'too-long') c.valid_until = '2026-11-19T00:00:31Z'
      if (kind === 'reason') c.reason = 'toString'
      if (kind === 'contradiction') c.state = 'fail'
      if (kind === 'missing-expiry') c.valid_until = null
      if (kind === 'mixed-versions') p.evaluated_gates!.version = 1
      return preview
    })
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'previewFailed')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => productPreview(requests, 'pass'))
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="product-account-controls"]').text()).toContain(prefix + 'accountControlsStates.pass')
  })
  it.each(['version', 'state', 'reason', 'count', 'required', 'time', 'deadline', 'missing-gate'])('rejects inconsistent product gate intersections (%s)', async kind => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-19T00:00:00Z'))
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => {
      const preview = productPreview(requests, 'pass'), product = preview.results[0]!.product_procurement!, gate = product.evaluated_gates!
      if (kind === 'version') gate.version++
      if (kind === 'state') gate.state = 'fail'
      if (kind === 'reason') gate.reason = 'availability_failed'
      if (kind === 'count') gate.passed_gates++
      if (kind === 'required') gate.required_gates++
      if (kind === 'time') gate.evaluated_at = '2999-01-01T00:00:00Z'
      if (kind === 'deadline') gate.valid_until = '2026-11-19T00:06:00Z'
      if (kind === 'missing-gate') delete product.availability
      return preview
    })
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'previewFailed')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => productPreview(requests, 'pass'))
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="product-evaluated-gates-status"]').text()).toContain(prefix + 'evaluatedGatesStates.pass')
  })
  it('expires answer evidence, rules and the required product gate at the deadline', async () => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-19T00:00:00Z'))
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    const quality = qualityFixture('pass')
    quality.valid_until = quality.rules[0]!.valid_until = '2026-11-19T00:00:01Z'
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({
      ...fixture(), observed_at: quality.evaluated_at, results: requests.map(request => ({ ...request,
        state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated', quality_evidence: quality,
        product_procurement: { version: 1, group_id: 42, policy_revision: 3, price_basis: 'group_default', default_d: .15, threshold: .12, state: 'constraints_passed', price_class: 'admitted', answer_quality: { version: 1, required: true, state: 'pass' } }
      }))
    }))
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="quality-scope-status"]').text()).toContain(prefix + 'qualityStates.pass')
    await vi.advanceTimersByTimeAsync(1000)
    expect(w.get('[data-testid="quality-scope-status"]').text()).toContain(prefix + 'qualityStates.unknown')
    expect(w.get('[data-testid="product-answer-quality"]').text()).toContain(prefix + 'answerQualityStates.unknown')
    expect(w.get('[data-testid="quality-scope-rules"]').text()).not.toContain(prefix + 'qualityStates.pass')
    expect(w.get('[data-testid="quality-scope-rules"]').text()).toContain(prefix + 'qualityReasons.expired')
  })

  it('expires a displayed pass and the required product gate without reloading', async () => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-19T00:00:00Z'))
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    const evidence = { ...availabilityFixture('pass'), valid_until: '2026-11-19T00:00:01Z' }
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({
      ...fixture(), observed_at: evidence.evaluated_at, results: requests.map(request => ({ ...request,
        state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated', availability_evidence: evidence,
        product_procurement: { version: 1, group_id: 42, policy_revision: 3, price_basis: 'group_default', default_d: .15, threshold: .12, state: 'constraints_passed', price_class: 'admitted', availability: { version: 1, required: true, state: 'pass' } }
      }))
    }))
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="availability-scope-status"]').text()).toContain(prefix + 'availabilityStates.pass')
    expect(w.get('[data-testid="product-availability"]').text()).toContain(prefix + 'availabilityGateStates.pass')
    await vi.advanceTimersByTimeAsync(1000)
    expect(w.get('[data-testid="availability-scope-status"]').text()).toContain(prefix + 'availabilityStates.unknown')
    expect(w.get('[data-testid="product-availability"]').text()).toContain(prefix + 'availabilityGateStates.unknown')
  })

  it.each(['pass', 'fail', 'unknown', 'not_configured', 'legacy'])('shows independent latest availability conclusions (%s)', async status => {
    const w = await open(); await setupProbe(w)
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({
      ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request,
        state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated',
        ...(status === 'legacy' ? {} : { availability_evidence: availabilityFixture(status) })
      }))
    }))
    await submitPreview(w); await flushPromises()
    const expected = status === 'legacy' || status === 'pass' || status === 'fail' ? 'unknown' : status
    expect(w.get('[data-testid="availability-scope-status"]').text()).toContain(prefix + 'availabilityStates.' + expected)
    expect(w.get('[data-testid="availability-scope-evidence"]').text()).toContain(prefix + 'availabilityBoundary')
    if (status !== 'legacy') expect(w.get('[data-testid="availability-scope-evidence"]').text()).toContain('2026-11-19T00:00:00Z')
    expect(w.text()).toContain(prefix + 'notQualified')
  })
  it.each(['version', 'future', 'count', 'empty-pass', 'missing-deadline', 'expired', 'reason', 'contradictory-fail', 'malformed'])('rejects inconsistent availability evidence (%s)', async kind => {
    const w = await open(); await setupProbe(w)
    const evidence = availabilityFixture('pass')
    if (kind === 'version') evidence.version++
    if (kind === 'future') evidence.evaluated_at = '2999-01-01T00:00:00Z'
    if (kind === 'count') evidence.confirmed_rules++
    if (kind === 'empty-pass') evidence.rule_count = evidence.confirmed_rules = 0
    if (kind === 'missing-deadline') evidence.valid_until = null
    if (kind === 'expired') evidence.valid_until = evidence.evaluated_at
    if (kind === 'reason') evidence.reason = 'toString'
    if (kind === 'contradictory-fail') evidence.status = 'fail'
    if (kind === 'malformed') evidence.valid_until = 'not-a-date'
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({
      ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request,
        state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated', availability_evidence: evidence
      }))
    }))
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'previewFailed')
  })
  it('rejects a product availability pass contradicted by scope failure', async () => {
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({
      ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request,
        state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated', availability_evidence: availabilityFixture('fail'),
        product_procurement: { version: 1, group_id: 42, policy_revision: 3, price_basis: 'group_default', default_d: .15, threshold: .12, state: 'constraints_passed', price_class: 'admitted', availability: { version: 1, required: true, state: 'pass' } }
      }))
    }))
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
  })

  it.each(['pass', 'fail', 'unknown', 'not_required'])('shows the product answer gate independently of price and full qualification (%s)', async state => {
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({
      ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request,
        state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated',
        quality_evidence: qualityFixture(state === 'not_required' ? 'unknown' : state),
        product_procurement: { version: 1, group_id: 42, policy_revision: 3, price_basis: 'group_default', default_d: .15, threshold: .12, state: 'constraints_passed', price_class: 'admitted',
          answer_quality: { version: 1, required: state !== 'not_required', state } }
      }))
    }))
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="product-answer-quality"]').text()).toContain(prefix + 'answerQualityStates.' + (state === 'pass' || state === 'fail' ? 'unknown' : state))
    expect(w.get('[data-testid="product-evaluated-gates-status"]').text()).toContain(prefix + 'evaluatedGatesStates.unknown')
    expect(w.get('[data-testid="product-procurement-state"]').text()).toContain(prefix + 'productStates.constraints_passed')
    expect(w.text()).toContain(prefix + 'notQualified')
  })

  it('rejects a product answer gate that contradicts the same scope evidence', async () => {
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({
      ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request,
        state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated', quality_evidence: qualityFixture('fail'),
        product_procurement: { version: 1, group_id: 42, policy_revision: 3, price_basis: 'group_default', default_d: .15, threshold: .12, state: 'constraints_passed', price_class: 'admitted', answer_quality: { version: 1, required: true, state: 'pass' } }
      }))
    }))
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'previewFailed')
  })
  it.each(['pass', 'fail', 'unknown', 'not_configured', 'legacy'])('shows scope answer evidence separately from model identity (%s)', async status => {
    const w = await open(); await setupProbe(w)
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({
      ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request,
        state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated',
        ...(status === 'legacy' ? {} : { quality_evidence: qualityFixture(status) })
      }))
    }))
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="quality-scope-status"]').text()).toContain(prefix + 'qualityStates.' + (['legacy', 'pass', 'fail'].includes(status) ? 'unknown' : status))
    expect(w.get('[data-testid="quality-scope-evidence"]').text()).toContain(prefix + 'modelIdentityUnknown')
    if (['pass', 'fail', 'unknown'].includes(status)) {
      expect(w.get('[data-testid="quality-scope-rules"]').text()).toContain('#12')
      expect(w.get('[data-testid="quality-scope-evidence"]').text()).toContain('2026-11-19T00:00:00Z')
    }
    expect(w.text()).toContain(prefix + 'notQualified')
  })
  it.each(['future-version', 'identity-pass', 'future-read', 'stale-read', 'unknown-status', 'unknown-reason', 'counts', 'zero-rules-pass', 'rule-count', 'duplicate-rule', 'unknown-rule', 'invalid-date', 'expired-pass', 'missing-window', 'failed-rule-pass', 'partial-pass', 'unknown-top-pass-reason', 'unknown-rule-pass-reason'])('hides contradictory quality evidence rather than showing green (%s)', async kind => {
    const w = await open(); await setupProbe(w)
    const q = qualityFixture('pass')
    if (kind === 'future-version') q.version++
    if (kind === 'identity-pass') q.model_identity = 'pass'
    if (kind === 'future-read') q.evaluated_at = '2999-01-01T00:00:00Z'
    if (kind === 'stale-read') q.evaluated_at = '2026-11-18T00:00:00Z'
    if (kind === 'unknown-status') q.status = 'toString'
    if (kind === 'unknown-reason') q.reason = 'future_passing_reason'
    if (kind === 'counts') q.valid_samples++
    if (kind === 'zero-rules-pass') { q.rules = []; q.rule_count = 0; q.confirmed_rules = 0; q.samples = 0; q.valid_samples = 0 }
    if (kind === 'rule-count') q.rule_count++
    if (kind === 'duplicate-rule') q.rules.push(q.rules[0]!)
    if (kind === 'unknown-rule') q.rules[0]!.status = 'constructor'
    if (kind === 'invalid-date') q.valid_until = 'invalid'
    if (kind === 'expired-pass') q.valid_until = q.evaluated_at
    if (kind === 'missing-window') q.rules[0]!.valid_until = null
    if (kind === 'failed-rule-pass') q.rules[0]!.status = 'fail'
    if (kind === 'partial-pass') q.rules[0]!.valid_samples = 0
    if (kind === 'unknown-top-pass-reason' || kind === 'unknown-rule-pass-reason') {
      Object.assign(q, qualityFixture('unknown'))
      if (kind === 'unknown-top-pass-reason') q.reason = 'all_rules_passed'
      else q.rules[0]!.reason = 'all_samples_passed'
    }
    vi.mocked(previewProcurementScopes).mockImplementationOnce(async ({ requests }) => ({ ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request, state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated', quality_evidence: q })) }))
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="catalog-preview-result"]').exists()).toBe(false)
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'previewFailed')
  })
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
  it.each(['rejected_procurement_bucket', 'rejected_procurement_catalog_unavailable'])('shows a readable conclusion and matching catalog revision for %s', async reason => {
    const w = await open(); await setupProbe(w)
    await w.get('[data-testid="procurement-product"]').setValue('42')
    const request = { account_id: 7, public_model: 'public-model', scope: { version: 2, model: 'wire-model', protocol: 'responses_http', stream: false, service_tier: 'unspecified', reasoning_effort: 'unspecified', capability: 'text' } }
    const response = { ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: [{ ...request, state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated', product_procurement: { version: 1, group_id: 42, policy_revision: 3, catalog_revision: 1, price_basis: 'group_default', default_d: .15, threshold: .12, state: 'blocked', reason, price_class: reason } }] }
    vi.mocked(previewProcurementScopes).mockResolvedValueOnce(response)
    await submitPreview(w); await flushPromises()
    expect(w.get('[data-testid="product-procurement"]').text()).toContain(prefix + (reason === 'rejected_procurement_bucket' ? 'bucketRejected' : 'catalogUnavailable'))
    response.results[0]!.product_procurement.catalog_revision = 2
    vi.mocked(previewProcurementScopes).mockResolvedValueOnce(response)
    await submitPreview(w); await flushPromises()
    expect(w.find('[data-testid="product-procurement"]').exists()).toBe(false)
    expect(w.get('[data-testid="procurement-error"]').text()).toContain(prefix + 'previewFailed')
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

function productPreview(requests: ProcurementRequestScope[], state: string): ProcurementPreview {
  const required = state !== 'not_required', resultState = required ? state : 'pass'
  const until = '2026-11-19T00:00:30Z'
  return { ...fixture(), observed_at: '2026-11-19T00:00:00Z', results: requests.map(request => ({ ...request,
    state: 'classified', applicability: 'covered', rate_source: 'manual', effective_rate: '0.025', bucket: fixture().catalog.buckets[0]!, qualification: 'not_evaluated',
    quality_evidence: qualityFixture(required ? state : 'unknown'),
    product_procurement: { version: 1, group_id: 42, policy_revision: 3, price_basis: 'group_default', default_d: .15, threshold: .12, state: 'constraints_passed', price_class: 'admitted',
      answer_quality: { version: 1, required, state }, availability: { version: 1, required: false, state: 'not_required' },
      account_controls: { version: 1, state: 'pass', reason: 'account_controls_passed', evaluated_at: '2026-11-19T00:00:00Z', valid_until: until },
      evaluated_gates: { version: 2, state: resultState, reason: resultState === 'pass' ? 'all_evaluated_gates_passed' : state === 'fail' ? 'answer_quality_failed' : 'answer_quality_unavailable', passed_gates: 2 + Number(state === 'pass'), required_gates: 2 + Number(required), evaluated_at: '2026-11-19T00:00:00Z', valid_until: until }
    }
  })) }
}
function availabilityFixture(status: string) {
  return { version: 1, status, reason: ({ pass: 'all_rules_passed', fail: 'availability_not_confirmed', unknown: 'availability_scope_unconfirmed', not_configured: 'availability_not_configured' } as Record<string, string>)[status]!,
    evaluated_at: '2026-11-19T00:00:00Z', valid_until: ['pass', 'fail'].includes(status) ? '2026-11-19T00:05:00Z' : null, rule_count: status === 'not_configured' ? 0 : 1, confirmed_rules: status === 'pass' ? 1 : 0 }
}
function qualityFixture(status: string) {
  const valid = status === 'pass' || status === 'fail'
  return { version: 1, status, reason: ({ pass: 'all_rules_passed', fail: 'answer_incorrect', unknown: 'rule_unconfirmed', not_configured: 'quality_not_configured' } as Record<string, string>)[status]!,
    model_identity: 'unknown', evaluated_at: '2026-11-19T00:00:00Z', valid_until: valid ? '2026-11-19T00:05:00Z' : null,
    rule_count: status === 'not_configured' ? 0 : 1, confirmed_rules: status === 'pass' ? 1 : 0, samples: status === 'not_configured' ? 0 : 1, valid_samples: valid ? 1 : 0, other_scope_rules: 0,
    rules: status === 'not_configured' ? [] : [{ plan_id: 12, status, reason: status === 'pass' ? 'all_samples_passed' : status === 'fail' ? 'answer_incorrect' : 'expired', samples: 1, valid_samples: valid ? 1 : 0, valid_until: valid ? '2026-11-19T00:05:00Z' : null }]
  }
}
