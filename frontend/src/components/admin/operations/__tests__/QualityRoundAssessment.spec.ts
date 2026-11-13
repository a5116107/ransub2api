import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { describe, expect, it } from 'vitest'
import type { QualityRound } from '@/api/admin/channelOperations'
import en from '@/i18n/locales/en/channelOps'
import zh from '@/i18n/locales/zh/channelOps'
import QualityRoundAssessment from '../QualityRoundAssessment.vue'

const base: QualityRound = {
  id: 1, account_id: 42, plan_id: 8, measured_model: 'fixture-model',
  observed_at: '2026-11-13T11:55:00Z', probes: 2, passed_probes: 2, expected: 2,
  outcome: 'passed', round_complete: true, round_probes: 4, round_expected: 4, actions: []
}
function render(patch: Partial<QualityRound> = {}, locale: 'en' | 'zh' = 'zh', queriedAt = '2026-11-13T12:00:00Z') {
  return mount(QualityRoundAssessment, {
    props: { round: { ...base, ...patch }, queriedAt },
    global: { plugins: [createI18n({ legacy: false, locale, messages: { en: { channelOps: en }, zh: { channelOps: zh } } })] }
  })
}

describe('persisted quality round evidence', () => {
  it.each(['zh', 'en'] as const)('keeps functional outcome, coverage, sample age and identity distinct in %s', locale => {
    const w = render({}, locale)
    const messages = locale === 'zh' ? zh : en
    expect(w.get('[data-testid="quality-round-outcome"]').text()).toBe(messages.passed)
    expect(w.get('[data-testid="quality-round-coverage"]').text()).toContain('4 / 4 · 100%')
    expect(w.get('progress').attributes('value')).toBe('100')
    expect(w.get('progress').attributes('aria-label')).toBe(messages.evidence.roundCoverage)
    expect(w.text()).toContain(messages.evidence.historical)
    expect(w.text()).toContain(messages.evidence.identityNote)
    expect(w.text()).toContain(locale === 'zh' ? '300 秒' : '300 seconds')
    expect(w.get('[data-testid="quality-round-identity"]').text()).toBe(messages.evidence.identityUnknown)
    expect(w.get('time').attributes('datetime')).toBe(base.observed_at)
    expect(w.get('summary').text()).toBe(messages.evidence.details)
    w.unmount()
  })

  it.each([
    { round_complete: false }, { round_expected: null }, { round_expected: 0 },
    { round_probes: 3 }, { round_probes: 5 }, { round_probes: Number.NaN },
    { round_probes: Number.POSITIVE_INFINITY }, { round_probes: -1 },
    { probes: 0, expected: 0, passed_probes: 0 }, { probes: 3 }, { expected: null },
    { passed_probes: 3 }, { passed_probes: 1 }, { passed_probes: -1 },
    { passed_probes: 1.5 }, { measured_model: null }, { measured_model: ' ' },
    { round_probes: 1, round_expected: 1 }, { outcome: 'inconclusive' }
  ] satisfies Partial<QualityRound>[])('keeps incomplete or contradictory evidence inconclusive: %j', patch => {
    const w = render(patch)
    expect(w.get('[data-testid="quality-round-outcome"]').text()).toBe(zh.inconclusive)
    expect(w.get('[data-testid="quality-round-identity"]').text()).toBe(zh.evidence.identityUnknown)
    w.unmount()
  })

  it('uses the recorded degradation verdict rather than deriving a pass from the ratio', () => {
    const w = render({ outcome: 'degraded', passed_probes: 1 })
    expect(w.get('[data-testid="quality-round-outcome"]').text()).toBe(zh.degraded)
    expect(w.get('[data-testid="quality-round-outcome"] .bg-red-500').exists()).toBe(true)
    w.unmount()
  })

  it('preserves unknown outcome even when every measured probe has passed', () => {
    const w = render({ outcome: 'inconclusive' })
    expect(w.get('[data-testid="quality-round-outcome"]').text()).toBe(zh.inconclusive)
    w.unmount()
  })

  it('shows partial coverage without rounding it to a full round', () => {
    const w = render({ round_complete: false, round_probes: 399, round_expected: 400 })
    expect(w.get('[data-testid="quality-round-coverage"]').text()).toContain('399 / 400 · 99.7%')
    expect(w.get('progress').attributes('value')).toBe('99.75')
    expect(w.text()).toContain(`${zh.incompleteRound} 399 / 400`)
    w.unmount()
  })

  it.each([null, 0, -1, 1.5, Number.NaN, Number.POSITIVE_INFINITY])('does not draw invented coverage with an invalid expected count: %s', expected => {
    const w = render({ round_expected: expected })
    expect(w.find('progress').exists()).toBe(false)
    expect(w.get('[data-testid="quality-round-coverage"]').text()).toContain(zh.evidence.unknown)
    w.unmount()
  })

  it.each([
    ['invalid timestamp', '2026-11-13T12:00:00Z'],
    ['2026-11-13T12:00:01Z', '2026-11-13T12:00:00Z'],
    ['2026-11-13T11:55:00Z', ''],
    ['2026-11-13T11:55:00Z', 'invalid read timestamp']
  ])('keeps age unknown for invalid or future time: %s / %s', (observed_at, read) => {
    const w = render({ observed_at }, 'zh', read)
    expect(w.findAll('dd').at(-1)!.text()).toBe(zh.evidence.unknown)
    expect(w.text()).not.toContain('NaN')
    expect(w.text()).not.toContain('Infinity')
    w.unmount()
  })

  it('updates the verdict and coverage together after refresh without converting history into live admission', async () => {
    const w = render()
    await w.setProps({ round: { ...base, outcome: 'inconclusive', round_complete: false, round_probes: 2 } })
    expect(w.get('[data-testid="quality-round-outcome"]').text()).toBe(zh.inconclusive)
    expect(w.get('[data-testid="quality-round-coverage"]').text()).toContain('50%')
    expect(w.text()).toContain(zh.evidence.validityUnknown)
    w.unmount()
  })
})
