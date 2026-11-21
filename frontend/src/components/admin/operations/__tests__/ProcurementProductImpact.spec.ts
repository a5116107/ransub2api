import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProcurementProductImpactView from '../ProcurementProductImpact.vue'
import Select from '@/components/common/Select.vue'
import { list as listGroups } from '@/api/admin/groups'
import { applyProcurementRecipe, previewProcurementProductImpact, readProcurementManagement, previewProcurementManagement, saveProcurementManagement, type ProcurementManagementState, type ProcurementManagementPreview, type ProcurementManagementReview, type ProcurementProductImpact, type ProcurementSourceInventory, type ProcurementWriterReview } from '@/api/admin/procurementCatalog'
import { validProcurementProductImpact, validProcurementManagementPreview } from '@/utils/procurementProductImpact'
import zh from '@/i18n/locales/zh/upstreamOps'
import en from '@/i18n/locales/en/upstreamOps'
import { listQualityPresets, type QualityRulePreset } from '@/api/admin/accountQuality'
import { observationRecipePreset } from '@/utils/procurementProductImpact'
import type { MonitoringProbeCoverage } from '@/types/monitoringProbeBudget'

vi.mock('vue-i18n', async importOriginal => ({ ...(await importOriginal<typeof import('vue-i18n')>()), useI18n: () => ({ t: (key: string, values?: Record<string, unknown>) => `${key}${values ? ' ' + JSON.stringify(values) : ''}` }) }))
vi.mock('@/api/admin/groups', () => ({ list: vi.fn() }))
vi.mock('@/api/admin/accountQuality', () => ({ listQualityPresets: vi.fn() }))
vi.mock('@/api/admin/procurementCatalog', () => ({ applyProcurementRecipe: vi.fn(), previewProcurementProductImpact: vi.fn(), readProcurementManagement: vi.fn(), previewProcurementManagement: vi.fn(), saveProcurementManagement: vi.fn() }))
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
const reviewFixture = (): ProcurementManagementReview => ({
  version: 1, mode: 'manual', scope: 'current_bindings_and_saved_rule_targets', inventory_complete: true,
  rules_examined: 3, conflicting_rules: 1, protection_rules: 1, unknown_rules: 0, judge_dependency_rules: 1,
  rules: [
    { plan_id: 12, account_id: 14, account_name: 'Former supplier', enabled: false, action: 'observe_only', risk: 'membership_conflict', writes_product: false, retained_ownership: true, saved_product_ownership: true, judge_depends_on_product: false },
    { plan_id: 16, account_id: 9, account_name: 'Current supplier', enabled: true, action: 'disable_scheduling', risk: 'account_protection', writes_product: false, retained_ownership: false, saved_product_ownership: false, judge_depends_on_product: false },
    { plan_id: 18, account_id: 9, account_name: 'Current supplier', enabled: true, action: 'observe_only', risk: 'observation', writes_product: false, retained_ownership: false, saved_product_ownership: false, judge_depends_on_product: true }
  ],
  budget: { state: 'partial', revision: 3, enforced: true, inventory_complete: true, minimum_requests_per_day: 3000,
    regular_day_capacity: 80000, unestimated_sources: 4, reasons: [], warnings: [], window_start: '2026-11-21T00:00:00Z', window_end: '2026-11-22T00:00:00Z' }
})
const inventoryFixture = (): ProcurementSourceInventory => ({
  version: 1, state: 'approved_sources', policy_revision: 4, approved_accounts: 2, unbound_approved_accounts: 1,
  groups: [{ id: 8, name: 'Approved supply group', state: 'active' }],
  accounts: [9, 14, 20, 30].map(id => ({ account_id: id, name: id === 30 ? 'Not yet in product' : 'Supplier ' + id,
    platform: 'openai', status: id === 20 ? 'inactive' : 'active', schedulable: id !== 20, in_product: id !== 30, in_approved_sources: id === 9 || id === 30 }))
})
const writerFixture = (): ProcurementWriterReview => ({
  version: 1, scope: 'supply_candidates_and_explicit_writer_targets', inventory_complete: true, accounts_examined: 3,
  membership_conflicts: 2, account_protections: 1, historical_only: 1, unknown: 1,
  writers: [
    { kind: 'excel_bps_403', account_id: 9, account_name: 'Current supplier', enabled: true, risk: 'membership_conflict', writes_product: true, retained_ownership: false, reasons: ['moves_all_memberships'] },
    { kind: 'astra_scheduling', account_id: 30, account_name: 'External saved owner', enabled: false, risk: 'membership_conflict', writes_product: false, retained_ownership: true, reasons: ['astra_membership_owner'] },
    { kind: 'oauth_degradation', account_id: 9, account_name: 'Current supplier', enabled: false, risk: 'historical_only', writes_product: false, retained_ownership: false, reasons: ['native_history_not_exclusion'] },
    { kind: 'two_fa_admission', account_id: 14, account_name: 'Unbound enrollment', enabled: false, risk: 'account_protection', writes_product: false, retained_ownership: false, reasons: ['paused_enrollment_can_resume'] },
    { kind: 'oauth_degradation', account_id: null, account_name: '', enabled: null, risk: 'unknown', writes_product: false, retained_ownership: false, reasons: ['configuration_unreadable'] }
  ]
})
const managementStateFixture = (): ProcurementManagementState => ({ version: 1, group_id: 2, revision: 0, mode: 'manual', current_owned: 0, retained_owned: 0, manual_memberships: 3 })
const managementPreviewFixture = (): ProcurementManagementPreview => {
  const review = reviewFixture()
  review.source_inventory = inventoryFixture(); review.scope = 'current_bindings_and_approved_sources_and_saved_rule_targets'
  review.writer_review = { ...writerFixture(), membership_conflicts: 0, account_protections: 0, historical_only: 0, unknown: 0, writers: [] }
  return { version: 1, state: managementStateFixture(), observed_at: '2026-11-20T04:30:00Z', expires_at: '2026-11-20T04:32:00Z', fingerprint: 'a'.repeat(64), can_confirm: true,
    memberships: [{ account_id: 9, account_name: 'Current supplier', ownership: null, claimable: true, reasons: [], protections: ['quality_protection', 'judge_dependency'] }], review }
}
async function selectedManagement(w: ReturnType<typeof mount>) {
  const next = fixture(); next.management_review = managementPreviewFixture().review
  vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
  await choose(w); await preview(w)
  await w.get('[data-testid="management-select-9"]').setValue(true)
  await w.get('[data-testid="management-preview"]').trigger('click'); await flushPromises()
}
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
  vi.mocked(readProcurementManagement).mockResolvedValue(managementStateFixture())
  vi.mocked(previewProcurementManagement).mockResolvedValue(managementPreviewFixture())
  vi.mocked(listQualityPresets).mockResolvedValue([])
})
afterEach(() => { wrappers.splice(0).forEach(w => w.unmount()); vi.useRealTimers() })

const presetFixture = (): QualityRulePreset => ({ id: 7, name: 'Observe two models', model_id: 'model-a', cron_expression: '0 * * * *', enabled: true, max_results: 100,
  created_at: '2026-11-20T00:00:00Z', updated_at: '2026-11-20T00:00:00Z',
  pelican_config: { question_kind: 'candy', prompt: 'Private prompt', reasoning_effort: 'medium', parallel_count: 2, quality: { action: 'observe_only' } } })
const recipeFixture = (): ProcurementProductImpact => {
  const next = fixture(), review = reviewFixture()
  review.source_inventory = inventoryFixture(); review.scope = 'current_bindings_and_approved_sources_and_saved_rule_targets'
  next.management_review = review
  const coverage = { observed_at: next.observed_at, budget_revision: 3, enforced: true, can_save: true, blocking_reasons: [], warnings: [],
    inventory_complete: true, plan_inventory_complete: true, candy_inventory_complete: true, active_inventory_complete: true,
    minimum_total_requests_per_day: 3384, regular_day_capacity: 80000, schedule_window_start: review.budget.window_start, schedule_window_end: review.budget.window_end,
    unestimated_demand_sources: ['event_demand'] } as unknown as MonitoringProbeCoverage
  next.monitoring_recipe = { version: 1, preset_id: 7, preset_name: 'Observe two models', preset_updated_at: presetFixture().updated_at, source_policy_revision: 4,
    state: 'previewed', accounts: 2, demand: { additional_plans: 2, current_minimum_requests_per_day: 3000, delta_minimum_requests_per_day: 384, coverage } }
  return next
}

const reviewedRecipeFixture = (): ProcurementProductImpact => {
  const next = recipeFixture()
  next.monitoring_recipe!.version = 2
  next.monitoring_recipe!.distribution_review = { state: 'ready', examined_accounts: 2, conflicts: [] }
  return next
}

describe('read-only product monitoring recipe', () => {
  async function selectRecipe(w: ReturnType<typeof mount>) {
    await w.get('#impact-preset').trigger('focus'); await flushPromises(); await w.get('#impact-preset').setValue('7')
  }
  it.each([true, false])('separates an enabled=%s observation-plan conflict from sufficient budget', async enabled => {
    const next = reviewedRecipeFixture()
    next.monitoring_recipe!.demand!.coverage.unestimated_demand_sources = []
    next.monitoring_recipe!.distribution_review = { state: 'conflict', examined_accounts: 2, conflicts: [{ account_id: 9, plan_id: 18, enabled }] }
    vi.mocked(listQualityPresets).mockResolvedValue([presetFixture()]); vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await selectRecipe(w); await preview(w)
    expect(w.get('[data-testid="recipe-conclusion"]').text()).toContain(p + 'recipe.states.lower_bound_fits')
    expect(w.get('[data-testid="recipe-distribution-conclusion"]').text()).toContain(p + 'recipe.distribution.states.conflict')
    expect(w.get('[data-testid="recipe-distribution-conflict"]').text()).toContain('Supplier 9')
    expect(w.get('[data-testid="recipe-distribution-conflict"]').text()).toContain(`"plan":18`)
    expect(w.get('[data-testid="recipe-distribution-conflict"]').text()).toContain(p + `recipe.distribution.${enabled ? 'enabled' : 'paused'}`)
    expect(w.get('[data-testid="recipe-distribution"]').find('a').attributes('href')).toBe('/admin/account-quality')
    expect(saveProcurementManagement).not.toHaveBeenCalled()
  })
  it.each(['ready', 'unknown', 'legacy'])('reports %s uniqueness without claiming automatic distribution', async state => {
    const next = state === 'legacy' ? recipeFixture() : reviewedRecipeFixture()
    if (state === 'unknown') next.monitoring_recipe!.distribution_review = { state: 'unknown', examined_accounts: 0, conflicts: [] }
    vi.mocked(listQualityPresets).mockResolvedValue([presetFixture()]); vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await selectRecipe(w); await preview(w)
    expect(w.get('[data-testid="recipe-distribution-conclusion"]').text()).toContain(p + `recipe.distribution.states.${state}`)
    expect(w.findAll('[data-testid="recipe-distribution-conflict"]')).toHaveLength(0)
  })
  it('rejects incomplete, mismatched or contradictory uniqueness snapshots and recovers without losing the recipe', async () => {
    expect(validProcurementProductImpact(reviewedRecipeFixture(), 2, 7)).toBe(true)
    for (const mutate of [
      (v: ProcurementProductImpact) => { delete v.monitoring_recipe!.distribution_review },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.version = 1 },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.distribution_review!.examined_accounts = 1 },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.distribution_review!.state = 'conflict' },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.distribution_review!.conflicts.push({ account_id: 9, plan_id: 18, enabled: true }) },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.distribution_review = { state: 'unknown', examined_accounts: 2, conflicts: [] } },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.distribution_review = { state: 'conflict', examined_accounts: 2, conflicts: [{ account_id: 20, plan_id: 18, enabled: true }] } },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.distribution_review = { state: 'conflict', examined_accounts: 2, conflicts: [{ account_id: 9, plan_id: 0, enabled: false }] } },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.distribution_review = { state: 'conflict', examined_accounts: 2, conflicts: [{ account_id: 9, plan_id: 18, enabled: true }, { account_id: 9, plan_id: 19, enabled: false }] } }
    ]) {
      const next = reviewedRecipeFixture(); mutate(next)
      expect(validProcurementProductImpact(next, 2, 7)).toBe(false)
    }
    const bad = reviewedRecipeFixture(); delete bad.monitoring_recipe!.distribution_review
    vi.mocked(listQualityPresets).mockResolvedValue([presetFixture()]); vi.mocked(previewProcurementProductImpact).mockResolvedValueOnce(bad).mockResolvedValue(reviewedRecipeFixture())
    const w = view(); await choose(w); await selectRecipe(w); await preview(w)
    expect(w.get('[data-testid="product-impact-error"]').text()).toContain(p + 'failed')
    expect((w.get('#impact-preset').element as HTMLSelectElement).value).toBe('7')
    await preview(w)
    expect(w.get('[data-testid="recipe-distribution-conclusion"]').text()).toContain(p + 'recipe.distribution.states.ready')
  })
  it('keeps empty or unknown approved inventory unreviewed instead of inventing ready', async () => {
    for (const state of ['no_approved_accounts', 'sources_unavailable']) {
      const next = reviewedRecipeFixture(), source = next.management_review!.source_inventory!
      next.monitoring_recipe!.state = state
      delete next.monitoring_recipe!.demand
      next.monitoring_recipe!.distribution_review = { state: 'unknown', examined_accounts: 0, conflicts: [] }
      source.accounts = source.accounts.filter(a => a.in_product)
      if (state === 'no_approved_accounts') {
        source.accounts.forEach(a => { a.in_approved_sources = false })
        source.approved_accounts = 0; source.unbound_approved_accounts = 0
        next.monitoring_recipe!.accounts = 0
      } else {
        source.state = 'unavailable'; source.groups[0].state = 'inactive'
        source.accounts.forEach(a => { a.in_approved_sources = null })
        source.approved_accounts = null; source.unbound_approved_accounts = null
        next.management_review!.scope = 'current_bindings_and_saved_rule_targets'
        next.monitoring_recipe!.accounts = null
      }
      expect(validProcurementProductImpact(next, 2, 7)).toBe(true)
      next.monitoring_recipe!.distribution_review = { state: 'ready', examined_accounts: 0, conflicts: [] }
      expect(validProcurementProductImpact(next, 2, 7)).toBe(false)
    }
  })
  it('paginates many plan conflicts and resets the page on a new snapshot', async () => {
    const next = reviewedRecipeFixture(), source = next.management_review!.source_inventory!
    for (let id = 100; id < 121; id++) source.accounts.push({ account_id: id, name: 'Approved ' + id, platform: 'openai', status: 'active', schedulable: false, in_product: false, in_approved_sources: true })
    source.approved_accounts = 23; source.unbound_approved_accounts = 22
    const recipe = next.monitoring_recipe!
    recipe.accounts = 23; recipe.demand!.additional_plans = 23
    recipe.distribution_review = { state: 'conflict', examined_accounts: 23, conflicts: source.accounts.filter(a => a.account_id >= 100).map(a => ({ account_id: a.account_id, plan_id: a.account_id + 100, enabled: false })) }
    vi.mocked(listQualityPresets).mockResolvedValue([presetFixture()]); vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await selectRecipe(w); await preview(w)
    expect(w.findAll('[data-testid="recipe-distribution-conflict"]')).toHaveLength(20)
    const navigation = w.get('[data-testid="recipe-distribution"] nav')
    expect(navigation.text()).toContain('1 / 2')
    await navigation.findAll('button')[1].trigger('click')
    expect(w.findAll('[data-testid="recipe-distribution-conflict"]')).toHaveLength(1)
    expect(w.get('[data-testid="recipe-distribution-conflict"]').text()).toContain('Approved 120')
    await preview(w)
    expect(w.get('[data-testid="recipe-distribution"] nav').text()).toContain('1 / 2')
  })
  it('loads presets lazily, excludes action/state/BPS/disabled rules, and never writes or displays private questions', async () => {
    const valid = presetFixture(), action = structuredClone(valid), probe = structuredClone(valid), bps = structuredClone(valid)
    action.id = 8; action.pelican_config.quality!.action = 'disable_scheduling'
    probe.id = 9; probe.pelican_config.question_kind = 'state_probe'
    bps.id = 10; bps.pelican_config.test_channel = 'bps'
    vi.mocked(listQualityPresets).mockResolvedValue([valid, action, probe, bps, { ...valid, id: 11, enabled: false }])
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(recipeFixture())
    const w = view(); await choose(w)
    expect(listQualityPresets).not.toHaveBeenCalled()
    await selectRecipe(w)
    expect(w.findAll('#impact-preset option')).toHaveLength(2)
    await preview(w)
    expect(previewProcurementProductImpact).toHaveBeenLastCalledWith(2, expect.any(AbortSignal), 7)
    expect(w.get('[data-testid="recipe-current"]').text()).toBe('3000')
    expect(w.get('[data-testid="recipe-delta"]').text()).toBe('+384')
    expect(w.get('[data-testid="recipe-proposed"]').text()).toBe('3384')
    expect(w.get('[data-testid="recipe-conclusion"]').text()).toContain(p + 'recipe.states.partial')
    expect(w.text()).not.toContain('Private prompt')
    expect(saveProcurementManagement).not.toHaveBeenCalled()
    expect(observationRecipePreset({ ...valid, pelican_config: { ...valid.pelican_config, question_kind: 'random_reasoning' } })).toBe(true)
  })
  it('invalidates in-flight and loaded results on recipe changes, and restores legacy requests on clearing', async () => {
    vi.mocked(listQualityPresets).mockResolvedValue([presetFixture()])
    const w = view(); await choose(w); await selectRecipe(w)
    let release!: (value: ProcurementProductImpact) => void
    vi.mocked(previewProcurementProductImpact).mockReturnValueOnce(new Promise(resolve => { release = resolve }))
    await w.get('form').trigger('submit'); await w.get('#impact-preset option').setSelected()
    release(recipeFixture()); await flushPromises()
    expect(w.find('[data-testid="product-impact-result"]').exists()).toBe(false)
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(fixture())
    await preview(w)
    expect(previewProcurementProductImpact).toHaveBeenLastCalledWith(2, expect.any(AbortSignal))
  })
  it('retries preset and preview failures without losing selection or granting eligibility', async () => {
    vi.mocked(listQualityPresets).mockRejectedValueOnce(new Error('read failed')).mockResolvedValue([presetFixture()])
    const w = view(); await choose(w); await w.get('#impact-preset').trigger('focus'); await flushPromises()
    expect(w.text()).toContain(p + 'recipe.loadFailed')
    await w.get('#impact-preset').trigger('focus'); await flushPromises(); await w.get('#impact-preset').setValue('7')
    vi.mocked(previewProcurementProductImpact).mockRejectedValueOnce({ status: 503 }).mockResolvedValue(recipeFixture())
    await preview(w); expect(w.get('[data-testid="product-impact-error"]').text()).toContain(p + 'failed')
    expect((w.get('#impact-preset').element as HTMLSelectElement).value).toBe('7')
    await preview(w); expect(w.find('[data-testid="product-monitoring-recipe"]').exists()).toBe(true)
    vi.mocked(previewProcurementProductImpact).mockRejectedValue({ status: 400, reason: 'PROCUREMENT_MONITORING_RECIPE_INVALID' })
    await preview(w); expect(w.get('[data-testid="product-impact-error"]').text()).toContain(p + 'recipe.invalid')
  })
  it.each(['not_enforced', 'insufficient', 'lower_bound_fits'])('displays the %s capacity conclusion without a qualification badge', async state => {
    const next = recipeFixture(), c = next.monitoring_recipe!.demand!.coverage
    c.enforced = state !== 'not_enforced'; c.can_save = state !== 'insufficient'; c.blocking_reasons = state === 'insufficient' ? ['day_capacity'] : []
    c.unestimated_demand_sources = []
    vi.mocked(listQualityPresets).mockResolvedValue([presetFixture()]); vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await selectRecipe(w); await preview(w)
    expect(w.get('[data-testid="recipe-conclusion"]').text()).toContain(p + `recipe.states.${state}`)
  })
  it('rejects missing, future, mismatched or contradictory recipe results rather than falling back to saved-only counts', () => {
    expect(validProcurementProductImpact(recipeFixture(), 2, 7)).toBe(true)
    for (const mutate of [
      (v: ProcurementProductImpact) => { delete v.monitoring_recipe },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.version = 2 },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.preset_id = 8 },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.source_policy_revision++ },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.demand!.delta_minimum_requests_per_day = 0 },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.demand!.coverage.can_save = false },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.accounts = 0 }
    ]) { const next = recipeFixture(); mutate(next); expect(validProcurementProductImpact(next, 2, 7)).toBe(false) }
    expect(validProcurementProductImpact(recipeFixture(), 2)).toBe(false)
  })
  it.each([zh, en])('provides translated recipe inputs, unknown states and capacity boundaries', locale => {
    for (const key of ['label', 'none', 'title', 'help', 'loading', 'loadFailed', 'empty', 'configure', 'invalid', 'boundary', 'plans', 'current', 'delta', 'proposed', 'capacity', 'identity', 'window', 'unestimated']) expect(locale.productImpact.recipe[key as keyof typeof locale.productImpact.recipe]).toBeTypeOf('string')
    for (const value of Object.values(locale.productImpact.recipe.states)) expect(value).toBeTypeOf('string')
    for (const value of Object.values(locale.productImpact.recipe.distribution.states)) expect(value).toBeTypeOf('string')
    for (const key of ['title', 'conflict', 'enabled', 'paused', 'boundary', 'manage', 'pages'] as const) expect(locale.productImpact.recipe.distribution[key]).toBeTypeOf('string')
  })
})

const applicableRecipeFixture = (): ProcurementProductImpact => {
  const next = reviewedRecipeFixture()
  next.monitoring_recipe!.version = 3
  next.monitoring_recipe!.application_review = { observed_at: next.observed_at, expires_at: new Date(Date.parse(next.observed_at) + 120000).toISOString(), fingerprint: 'b'.repeat(64), can_confirm: true }
  return next
}
const applicationFixture = () => ({ version: 1, group_id: 2, preset_id: 7, plans: [{ account_id: 9, plan_id: 101 }, { account_id: 30, plan_id: 102 }] })
const savedRecipeFixture = (): ProcurementProductImpact => {
  const next = applicableRecipeFixture()
  next.monitoring_recipe!.application_review!.can_confirm = false
  next.monitoring_recipe!.distribution_review = { state: 'conflict', examined_accounts: 2, conflicts: applicationFixture().plans.map(row => ({ ...row, enabled: true })) }
  return next
}
async function applicationView() {
  vi.mocked(listQualityPresets).mockResolvedValue([presetFixture()])
  vi.mocked(previewProcurementProductImpact).mockResolvedValue(applicableRecipeFixture())
  const w = view(); await choose(w)
  await w.get('#impact-preset').trigger('focus'); await flushPromises(); await w.get('#impact-preset').setValue('7'); await preview(w)
  return w
}
async function confirmRecipe(w: ReturnType<typeof mount>) {
  await w.get('[data-testid="recipe-consent"]').setValue(true)
  await w.get('[data-testid="recipe-confirm"]').trigger('click'); await flushPromises()
}
describe('explicit first monitoring-plan distribution', () => {
  it('requires consent, writes only the reviewed preset and verifies every plan in an independent read', async () => {
    const w = await applicationView()
    expect(validProcurementProductImpact(applicableRecipeFixture(), 2, 7)).toBe(true)
    expect(w.get('[data-testid="recipe-confirm"]').attributes('disabled')).toBeDefined()
    expect(applyProcurementRecipe).not.toHaveBeenCalled()
    vi.mocked(applyProcurementRecipe).mockResolvedValue(applicationFixture())
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(savedRecipeFixture())
    await confirmRecipe(w)
    const r = applicableRecipeFixture().monitoring_recipe!.application_review!
    expect(applyProcurementRecipe).toHaveBeenCalledTimes(1)
    expect(applyProcurementRecipe).toHaveBeenCalledWith(2, { version: 1, preset_id: 7, observed_at: r.observed_at, fingerprint: r.fingerprint }, expect.any(AbortSignal))
    expect(previewProcurementProductImpact).toHaveBeenCalledTimes(2)
    expect(w.get('[data-testid="recipe-saved"]').text()).toContain(p + 'recipe.application.saved')
    expect(w.get('[data-testid="recipe-saved"]').text()).toContain('"count":2')
    expect(saveProcurementManagement).not.toHaveBeenCalled()
    expect(w.find('[data-testid="recipe-consent"]').exists()).toBe(false)
  })
  it.each(['legacy1', 'legacy2', 'inactive', 'conflict', 'not_enforced', 'insufficient', 'unknown'])('never offers first distribution for %s', async state => {
    const next = applicableRecipeFixture(), r = next.monitoring_recipe!
    if (state.startsWith('legacy')) { r.version = state === 'legacy1' ? 1 : 2; delete r.application_review; if (r.version === 1) delete r.distribution_review }
    else {
      r.application_review!.can_confirm = false
      if (state === 'inactive') next.group_status = 'inactive'
      if (state === 'conflict') r.distribution_review = { state: 'conflict', examined_accounts: 2, conflicts: [{ account_id: 9, plan_id: 18, enabled: false }] }
      if (state === 'not_enforced') r.demand!.coverage.enforced = false
      if (state === 'insufficient') { r.demand!.coverage.can_save = false; r.demand!.coverage.blocking_reasons = ['day_capacity'] }
      if (state === 'unknown') r.distribution_review = { state: 'unknown', examined_accounts: 0, conflicts: [] }
    }
    const w = await applicationView(); vi.mocked(previewProcurementProductImpact).mockResolvedValue(next); await preview(w)
    expect(w.find('[data-testid="recipe-consent"]').exists()).toBe(false)
    expect(applyProcurementRecipe).not.toHaveBeenCalled()
  })
  it('expires conservatively from request start, clears consent and requires a fresh preview', async () => {
    vi.useFakeTimers()
    const w = await applicationView()
    await w.get('[data-testid="recipe-consent"]').setValue(true)
    await vi.advanceTimersByTimeAsync(121000)
    expect(w.get('[data-testid="recipe-confirm"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain(p + 'recipe.application.expired')
    await w.get('[data-testid="recipe-confirm"]').trigger('click'); await flushPromises()
    expect(applyProcurementRecipe).not.toHaveBeenCalled()
    await preview(w)
    expect(w.get<HTMLInputElement>('[data-testid="recipe-consent"]').element.checked).toBe(false)
    let finish!: (v: ProcurementProductImpact) => void
    vi.mocked(previewProcurementProductImpact).mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    await w.get('form').trigger('submit'); await vi.advanceTimersByTimeAsync(121000)
    finish(applicableRecipeFixture()); await flushPromises()
    expect(w.get('[data-testid="recipe-confirm"]').attributes('disabled')).toBeDefined()
  })
  it.each([409, 403, 503])('clears the confirmation after HTTP %s and does not blindly replay', async status => {
    const w = await applicationView()
    vi.mocked(applyProcurementRecipe).mockRejectedValue({ status })
    await confirmRecipe(w)
    expect(w.get('[data-testid="recipe-error"]').text()).toContain(p + `recipe.application.${status === 409 ? 'conflict' : status === 403 ? 'forbidden' : 'failed'}`)
    expect(w.find('[data-testid="recipe-consent"]').exists()).toBe(false)
    expect((w.get('#impact-preset').element as HTMLSelectElement).value).toBe('7')
    expect(applyProcurementRecipe).toHaveBeenCalledTimes(1)
    await preview(w)
    expect(w.get<HTMLInputElement>('[data-testid="recipe-consent"]').element.checked).toBe(false)
  })
  it.each(['read_failure', 'missing_plan', 'paused_plan', 'different_plan', 'malformed_write'])('reports the saved result as unverified after %s', async fault => {
    const w = await applicationView()
    const saved = applicationFixture(), next = savedRecipeFixture()
    if (fault === 'read_failure') vi.mocked(previewProcurementProductImpact).mockRejectedValue({ status: 503 })
    else {
      if (fault === 'missing_plan') next.monitoring_recipe!.distribution_review!.conflicts.pop()
      if (fault === 'paused_plan') next.monitoring_recipe!.distribution_review!.conflicts[0].enabled = false
      if (fault === 'different_plan') next.monitoring_recipe!.distribution_review!.conflicts[0].plan_id = 999
      vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    }
    vi.mocked(applyProcurementRecipe).mockResolvedValue(fault === 'malformed_write' ? { ...saved, plans: [saved.plans[0], saved.plans[0]] } : saved)
    await confirmRecipe(w)
    expect(w.find('[data-testid="recipe-saved"]').exists()).toBe(false)
    expect(w.get('[data-testid="recipe-error"]').text()).toContain(p + 'recipe.application.savedReadFailed')
    expect(w.find('[data-testid="recipe-consent"]').exists()).toBe(false)
  })
  it('serializes write controls and ignores a late completion after the selected product changes', async () => {
    const w = await applicationView()
    let finish!: (v: ReturnType<typeof applicationFixture>) => void
    vi.mocked(applyProcurementRecipe).mockReturnValue(new Promise(resolve => { finish = resolve }))
    await confirmRecipe(w)
    expect(w.get('#impact-preset').attributes('disabled')).toBeDefined()
    expect(w.get('[data-testid="product-impact-preview"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-testid="management-refresh"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-testid="recipe-confirm"]').attributes('disabled')).toBeDefined()
    w.getComponent(Select).vm.$emit('update:modelValue', 3); await flushPromises()
    finish(applicationFixture()); await flushPromises()
    expect(w.find('[data-testid="recipe-saved"]').exists()).toBe(false)
    expect(w.find('[data-testid="product-impact-result"]').exists()).toBe(false)
    expect(previewProcurementProductImpact).toHaveBeenCalledTimes(1)
  })
  it('rejects corrupt, contradictory or future confirmation contracts rather than creating permissions', () => {
    for (const mutate of [
      (v: ProcurementProductImpact) => { delete v.monitoring_recipe!.application_review },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.version = 2 },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.version = 4 },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.application_review!.fingerprint = 'z'.repeat(64) },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.application_review!.expires_at = 'invalid' },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.application_review!.can_confirm = false },
      (v: ProcurementProductImpact) => { v.group_status = 'inactive' },
      (v: ProcurementProductImpact) => { v.monitoring_recipe!.application_review!.expires_at = v.monitoring_recipe!.application_review!.observed_at }
    ]) { const next = applicableRecipeFixture(); mutate(next); expect(validProcurementProductImpact(next, 2, 7)).toBe(false) }
  })
  it.each([zh, en])('provides consent, cost, conflict and uncertain-write explanations', locale => {
    for (const key of ['title', 'ready', 'blocked', 'legacy', 'consent', 'confirm', 'saving', 'expires', 'expired', 'saved', 'conflict', 'forbidden', 'failed', 'savedReadFailed', 'boundary', 'manage']) {
      expect((locale.productImpact.recipe.application as Record<string, unknown>)[key]).toBeTypeOf('string')
    }
  })
})

describe('explicit review authorization without automatic execution', () => {
  it.each([zh, en])('covers all review action and conclusion labels, including the manual button on mobile', locale => {
    for (const key of ['manual', 'ready', 'blocked', 'choose', 'loading', 'refresh', 'preview', 'consent', 'confirm', 'counts', 'saved', 'conflict', 'expired', 'forbidden', 'failed', 'savedReadFailed']) {
      expect((locale.productImpact.authorization as Record<string, unknown>)[key]).toBeTypeOf('string')
    }
  })

  it('keeps old products manual and requires explicit selection, preview and consent before writing', async () => {
    const w = view(); await selectedManagement(w)
    expect(readProcurementManagement).toHaveBeenCalledWith(2, expect.any(AbortSignal))
    expect(previewProcurementManagement).toHaveBeenCalledWith(2, [9], expect.any(AbortSignal))
    expect(w.get('[data-testid="management-preview-result"]').text()).toContain(p + 'authorization.ready')
    expect(w.get('[data-testid="management-confirm"]').attributes('disabled')).toBeDefined()
    expect(saveProcurementManagement).not.toHaveBeenCalled()
    const state: ProcurementManagementState = { ...managementStateFixture(), revision: 1, mode: 'review', current_owned: 1, manual_memberships: 2 }
    vi.mocked(saveProcurementManagement).mockResolvedValue(state); vi.mocked(readProcurementManagement).mockResolvedValue(state)
    await w.get('[data-testid="management-consent"]').setValue(true)
    await w.get('[data-testid="management-confirm"]').trigger('click'); await flushPromises()
    expect(saveProcurementManagement).toHaveBeenCalledWith(2, { version: 1, mode: 'review', expected_revision: 0, account_ids: [9], observed_at: '2026-11-20T04:30:00Z', fingerprint: 'a'.repeat(64) }, expect.any(AbortSignal))
    expect(w.get('[data-testid="management-saved"]').text()).toContain(p + 'authorization.saved')
    expect(w.get('[data-testid="management-state"]').text()).toContain(p + 'authorization.modes.review')
    expect(w.find('[data-testid="management-preview-result"]').exists()).toBe(false)
  })
  it('preserves selection on 409, clears consent and requires another preview', async () => {
    const w = view(); await selectedManagement(w)
    vi.mocked(saveProcurementManagement).mockRejectedValue({ status: 409 })
    await w.get('[data-testid="management-consent"]').setValue(true); await w.get('[data-testid="management-confirm"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="management-error"]').text()).toContain(p + 'authorization.conflict')
    expect(w.get<HTMLInputElement>('[data-testid="management-select-9"]').element.checked).toBe(true)
    expect(w.find('[data-testid="management-preview-result"]').exists()).toBe(false)
    await w.get('[data-testid="management-preview"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="management-confirm"]').attributes('disabled')).toBeDefined()
  })
  it.each(['result', 'error'])('replaces an in-flight pre-save impact read and ignores its late %s', async ending => {
    const w = view(); await selectedManagement(w)
    let resolveOld!: (value: ProcurementProductImpact) => void
    let rejectOld!: (error: unknown) => void
    let oldSignal: AbortSignal | undefined
    vi.mocked(previewProcurementProductImpact).mockImplementationOnce((_id, signal) => {
      oldSignal = signal
      return new Promise((resolve, reject) => { resolveOld = resolve; rejectOld = reject })
    })
    await w.get('form').trigger('submit'); await flushPromises()
    const saved: ProcurementManagementState = { ...managementStateFixture(), revision: 1, mode: 'review', current_owned: 1, manual_memberships: 2 }
    const next = fixture(); next.management_review = { ...managementPreviewFixture().review, mode: 'review' }
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    vi.mocked(saveProcurementManagement).mockResolvedValue(saved); vi.mocked(readProcurementManagement).mockResolvedValue(saved)
    await w.get('[data-testid="management-consent"]').setValue(true)
    await w.get('[data-testid="management-confirm"]').trigger('click'); await flushPromises()
    expect(oldSignal?.aborted).toBe(true)
    expect(previewProcurementProductImpact).toHaveBeenCalledTimes(3)
    expect(w.get('[data-testid="product-management-mode"]').text()).toContain(p + 'review.reviewMode')
    const stale = fixture(); stale.management_review = managementPreviewFixture().review
    if (ending === 'result') resolveOld(stale)
    else rejectOld({ status: 503 })
    await flushPromises()
    expect(w.get('[data-testid="product-management-mode"]').text()).toContain(p + 'review.reviewMode')
    expect(w.get('[data-testid="management-state"]').text()).toContain(p + 'authorization.modes.review')
    expect(w.find('[data-testid="product-impact-error"]').exists()).toBe(false)
  })
  it('invalidates the old impact when a successful write returns an unreadable body', async () => {
    const w = view(); await selectedManagement(w)
    vi.mocked(saveProcurementManagement).mockResolvedValue(undefined as never)
    await w.get('[data-testid="management-consent"]').setValue(true)
    await w.get('[data-testid="management-confirm"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="management-error"]').text()).toContain(p + 'authorization.savedReadFailed')
    expect(w.find('[data-testid="management-saved"]').exists()).toBe(false)
    expect(w.find('[data-testid="product-impact-result"]').exists()).toBe(false)
    expect(w.find('[data-testid="management-state"]').exists()).toBe(false)
  })
  it('does not retain pre-save impact when the write succeeds but read-back fails', async () => {
    const w = view(); await selectedManagement(w)
    vi.mocked(saveProcurementManagement).mockResolvedValue({ ...managementStateFixture(), revision: 1, mode: 'review', current_owned: 1, manual_memberships: 2 })
    vi.mocked(readProcurementManagement).mockRejectedValue(new Error('read failed'))
    await w.get('[data-testid="management-consent"]').setValue(true)
    await w.get('[data-testid="management-confirm"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="management-error"]').text()).toContain(p + 'authorization.savedReadFailed')
    expect(w.find('[data-testid="product-impact-result"]').exists()).toBe(false)
  })
  it('does not call a successful write activated when independent read-back fails', async () => {
    const w = view(); await selectedManagement(w)
    vi.mocked(saveProcurementManagement).mockResolvedValue({ ...managementStateFixture(), revision: 1, mode: 'review', current_owned: 1, manual_memberships: 2 })
    vi.mocked(readProcurementManagement).mockRejectedValue(new Error('read failed'))
    await w.get('[data-testid="management-consent"]').setValue(true); await w.get('[data-testid="management-confirm"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="management-error"]').text()).toContain(p + 'authorization.savedReadFailed')
    expect(w.find('[data-testid="management-saved"]').exists()).toBe(false)
    expect(w.find('[data-testid="management-state"]').exists()).toBe(false)
    vi.mocked(readProcurementManagement).mockResolvedValue({ ...managementStateFixture(), revision: 1, mode: 'review', current_owned: 1, manual_memberships: 2 })
    await w.get('[data-testid="management-refresh"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="management-state"]').text()).toContain(p + 'authorization.modes.review')
    expect(saveProcurementManagement).toHaveBeenCalledTimes(1)
  })
  it('expires the confirmation locally without assuming the browser clock matches the server', async () => {
    vi.useFakeTimers(); const w = view(); await selectedManagement(w)
    await w.get('[data-testid="management-consent"]').setValue(true)
    expect(w.get('[data-testid="management-confirm"]').attributes('disabled')).toBeUndefined()
    await vi.advanceTimersByTimeAsync(121000)
    expect(w.get('[data-testid="management-confirm"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain(p + 'authorization.expired')
    expect(saveProcurementManagement).not.toHaveBeenCalled()
  })
  it('returns to manual with revision CAS without clearing retained ownership', async () => {
    const active: ProcurementManagementState = { ...managementStateFixture(), revision: 4, mode: 'review', current_owned: 1, retained_owned: 1, manual_memberships: 1 }
    vi.mocked(readProcurementManagement).mockResolvedValue(active)
    const w = view(); await choose(w)
    const manual: ProcurementManagementState = { ...active, revision: 5, mode: 'manual', current_owned: 0, retained_owned: 2 }
    vi.mocked(saveProcurementManagement).mockResolvedValue(manual); vi.mocked(readProcurementManagement).mockResolvedValue(manual)
    await w.get('[data-testid="management-manual"]').trigger('click'); await flushPromises()
    expect(saveProcurementManagement).toHaveBeenCalledWith(2, { version: 1, mode: 'manual', expected_revision: 4 }, expect.any(AbortSignal))
    expect(w.get('[data-testid="management-state"]').text()).toContain('"retained":2')
  })
  it.each([403, 503])('shows %s and recovers a state read without requesting a write', async status => {
    vi.mocked(readProcurementManagement).mockRejectedValue({ status })
    const w = view(); await choose(w)
    expect(w.get('[data-testid="management-error"]').text()).toContain(p + `authorization.${status === 403 ? 'forbidden' : 'failed'}`)
    vi.mocked(readProcurementManagement).mockResolvedValue(managementStateFixture())
    await w.get('[data-testid="management-refresh"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="management-error"]').exists()).toBe(false)
    expect(saveProcurementManagement).not.toHaveBeenCalled()
  })
  it('blocks malformed, future and contradictory previews rather than displaying a green result', () => {
    expect(validProcurementManagementPreview(managementPreviewFixture(), 2, [9])).toBe(true)
    for (const mutate of [
      (p: ProcurementManagementPreview) => { p.version = 2 as never },
      (p: ProcurementManagementPreview) => { p.memberships[0]!.reasons = ['future'] },
      (p: ProcurementManagementPreview) => { p.memberships[0]!.claimable = false },
      (p: ProcurementManagementPreview) => { p.memberships[0]!.ownership = { version: 2, kind: 'procurement', revision: 1 } as never },
      (p: ProcurementManagementPreview) => { p.review.source_inventory!.accounts = null as never },
      (p: ProcurementManagementPreview) => { p.state.mode = 'automatic' as never },
      (p: ProcurementManagementPreview) => { p.expires_at = p.observed_at }
    ]) { const p = managementPreviewFixture(); mutate(p); expect(validProcurementManagementPreview(p, 2, [9])).toBe(false) }
  })
})

describe('administrator product price impact in the existing operations journey', () => {
  it('keeps non-quality writers unchecked on the previous management response', async () => {
    const next = fixture(); next.management_review = reviewFixture()
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="product-writer-review"]').text()).toContain(p + 'review.writers.legacy')
  })
  it('separates automatic membership conflicts, native controls, history and unknown settings with original configuration links', async () => {
    const next = fixture(), review = reviewFixture(); next.management_review = review; review.writer_review = writerFixture()
    expect(validProcurementProductImpact(next, 2)).toBe(true)
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    const section = w.get('[data-testid="product-writer-review"]')
    expect(section.get('[data-testid="product-writer-state"]').text()).toContain(p + 'review.writers.states.conflict')
    expect(section.text()).toContain('External saved owner')
    expect(section.text()).toContain(p + 'review.writers.reasons.native_history_not_exclusion')
    expect(section.text()).toContain(p + 'review.writers.reasons.paused_enrollment_can_resume')
    expect(section.findAll('[data-testid="product-writer-row"]')).toHaveLength(5)
    expect(section.findAll('a').map(a => a.attributes('href'))).toEqual(['/admin/accounts', '/admin/harvest-flow', '/admin/auto-config', '/admin/account-quality', '/admin/auto-config'])
    expect(section.findAll('button')).toHaveLength(0)
  })
  it.each(['unknown', 'protected', 'no_membership_conflict'])('shows %s without inferring automatic takeover', async state => {
    const next = fixture(), review = reviewFixture(), writers = writerFixture(); next.management_review = review; review.writer_review = writers
    Object.assign(writers, { membership_conflicts: 0, historical_only: 0, account_protections: state === 'protected' ? 1 : 0, unknown: state === 'unknown' ? 1 : 0,
      writers: state === 'unknown' ? writers.writers.slice(4) : state === 'protected' ? writers.writers.slice(3, 4) : [] })
    expect(validProcurementProductImpact(next, 2)).toBe(true)
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="product-writer-state"]').text()).toContain(p + `review.writers.states.${state}`)
  })
  it('does not call a partial writer read conflict-free', async () => {
    const next = fixture(), review = reviewFixture(); next.management_review = review
    review.writer_review = { ...writerFixture(), inventory_complete: false, writers: [], membership_conflicts: 0, account_protections: 0, historical_only: 0, unknown: 0 }
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="product-writer-state"]').text()).toContain(p + 'review.writers.states.unknown')
  })
  it.each(['version', 'scope', 'counts', 'duplicate', 'unknown-reason', 'global-kind', 'global-name', 'safe-unknown', 'bad-risk', 'historical-owner', 'disabled-write', 'zero-account', 'missing-reasons', 'inventory-count'])('rejects an inconsistent %s writer review', kind => {
    const next = fixture(), review = reviewFixture(), writers = writerFixture(); next.management_review = review; review.writer_review = writers
    if (kind === 'version') writers.version++
    if (kind === 'scope') writers.scope = 'all_accounts'
    if (kind === 'counts') writers.membership_conflicts++
    if (kind === 'duplicate') writers.writers.push(writers.writers[0]!)
    if (kind === 'unknown-reason') writers.writers[0]!.reasons = ['future']
    if (kind === 'global-kind') writers.writers[4]!.kind = 'excel_bps_403'
    if (kind === 'global-name') writers.writers[4]!.account_name = 'private configuration'
    if (kind === 'safe-unknown') writers.writers[4]!.risk = 'historical_only'
    if (kind === 'bad-risk') writers.writers[0]!.risk = 'qualified'
    if (kind === 'historical-owner') writers.writers[2]!.retained_ownership = true
    if (kind === 'disabled-write') writers.writers[0]!.enabled = false
    if (kind === 'zero-account') writers.writers[0]!.account_id = 0
    if (kind === 'missing-reasons') writers.writers[0]!.reasons = []
    if (kind === 'inventory-count') writers.accounts_examined = 2
    expect(validProcurementProductImpact(next, 2)).toBe(false)
  })
  it('paginates writers separately and clears stale pages on refresh', async () => {
    const next = fixture(), review = reviewFixture(), writers = writerFixture(); next.management_review = review; review.writer_review = writers
    Object.assign(writers, { accounts_examined: 21, membership_conflicts: 21, account_protections: 0, historical_only: 0, unknown: 0,
      writers: Array.from({ length: 21 }, (_, index) => ({ ...writers.writers[0]!, account_id: index + 100 })) })
    expect(validProcurementProductImpact(next, 2)).toBe(true)
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    const details = w.get('[data-testid="product-writer-details"]')
    expect(details.findAll('[data-testid="product-writer-row"]')).toHaveLength(20)
    await details.get('nav button:last-child').trigger('click')
    expect(details.findAll('[data-testid="product-writer-row"]')).toHaveLength(1)
    expect(w.findAll('[data-testid="product-management-rule"]')).toHaveLength(3)
    await preview(w)
    expect(w.findAll('[data-testid="product-writer-row"]')).toHaveLength(20)
  })
  it('shows approved, unbound and retained outside-source accounts without implying qualified supply', async () => {
    const next = fixture(), review = reviewFixture(); next.management_review = review
    review.source_inventory = inventoryFixture(); review.scope = 'current_bindings_and_approved_sources_and_saved_rule_targets'
    expect(validProcurementProductImpact(next, 2)).toBe(true)
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    const section = w.get('[data-testid="product-source-inventory"]')
    expect(section.text()).toContain('Approved supply group')
    expect(section.text()).toContain('Not yet in product')
    expect(section.text()).toContain(p + 'review.sources.unbound')
    expect(section.text()).toContain(p + 'review.sources.outside')
    expect(section.text()).toContain(p + 'review.sources.boundary')
    expect(section.findAll('[data-testid="product-source-account"]')).toHaveLength(4)
    expect(w.get('[data-testid="product-management-mode"]').text()).toContain(p + 'review.manual')
  })
  it.each(['original_pool', 'unavailable'])('keeps %s approval unknown, not zero or an unrestricted new pool', async state => {
    const next = fixture(), review = reviewFixture(); next.management_review = review
    const inventory = inventoryFixture(); review.source_inventory = inventory
    Object.assign(inventory, { state, approved_accounts: null, unbound_approved_accounts: null })
    inventory.accounts = inventory.accounts.filter(a => a.in_product).map(a => ({ ...a, in_approved_sources: null }))
    if (state === 'original_pool') inventory.groups = []
    expect(validProcurementProductImpact(next, 2)).toBe(true)
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="product-source-inventory"]').text()).toContain(p + 'review.sources.states.' + state)
    expect(w.find('[data-testid="product-source-counts"]').exists()).toBe(false)
  })
  it('leaves source inventory unchecked on the previous management-review response', async () => {
    const next = fixture(); next.management_review = reviewFixture(); vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="product-source-inventory"]').text()).toContain(p + 'review.sources.legacy')
  })
  it.each(['version', 'state', 'counts', 'unbound-count', 'duplicate', 'unbound-unapproved', 'unknown-approved', 'group-state', 'scope', 'bound-count', 'missing-groups', 'zero-policy', 'group-empty', 'self-source'])('rejects the inconsistent %s source inventory', kind => {
    const next = fixture(), review = reviewFixture(); next.management_review = review
    const inventory = inventoryFixture(); review.source_inventory = inventory; review.scope = 'current_bindings_and_approved_sources_and_saved_rule_targets'
    if (kind === 'version') inventory.version++
    if (kind === 'state') inventory.state = 'qualified'
    if (kind === 'counts') inventory.approved_accounts!++
    if (kind === 'unbound-count') inventory.unbound_approved_accounts!++
    if (kind === 'duplicate') inventory.accounts[0]!.account_id = 14
    if (kind === 'unbound-unapproved') inventory.accounts[3]!.in_approved_sources = false
    if (kind === 'unknown-approved') inventory.accounts[0]!.in_approved_sources = null
    if (kind === 'group-state') inventory.groups[0]!.state = 'inactive'
    if (kind === 'scope') review.scope = 'current_bindings_and_saved_rule_targets'
    if (kind === 'bound-count') inventory.accounts[2]!.in_product = false
    if (kind === 'missing-groups') delete (inventory as Partial<ProcurementSourceInventory>).groups
    if (kind === 'zero-policy') inventory.policy_revision = 0
    if (kind === 'group-empty') inventory.groups = []
    if (kind === 'self-source') inventory.groups[0]!.id = 2
    expect(validProcurementProductImpact(next, 2)).toBe(false)
  })
  it('paginates the source union independently and clears its page on refresh', async () => {
    const next = fixture(), review = reviewFixture(), inventory = inventoryFixture(); next.management_review = review; review.source_inventory = inventory
    review.scope = 'current_bindings_and_approved_sources_and_saved_rule_targets'
    inventory.accounts = Array.from({ length: 21 }, (_, index) => ({ ...inventory.accounts[0]!, account_id: index + 100, in_product: index < 3, in_approved_sources: true }))
    inventory.approved_accounts = 21; inventory.unbound_approved_accounts = 18
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    const details = w.get('[data-testid="product-source-details"]')
    expect(details.findAll('[data-testid="product-source-account"]')).toHaveLength(20)
    await details.get('nav button:last-child').trigger('click')
    expect(details.findAll('[data-testid="product-source-account"]')).toHaveLength(1)
    expect(w.findAll('[data-testid="product-management-rule"]')).toHaveLength(3)
    await preview(w)
    expect(w.findAll('[data-testid="product-source-account"]')).toHaveLength(20)
  })
  it('keeps management readiness unknown on a price-only server', async () => {
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="product-management-review"]').text()).toContain(p + 'review.legacy')
  })
  it('separates paused ownership, current protections and judging dependencies without a takeover action', async () => {
    const next = fixture(); next.management_review = reviewFixture()
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    const review = w.get('[data-testid="product-management-review"]')
    expect(review.text()).toContain(p + 'review.manual')
    expect(review.text()).toContain('Former supplier')
    expect(review.text()).toContain(p + 'review.paused')
    expect(review.text()).toContain(p + 'review.retained')
    expect(review.text()).toContain(p + 'review.risks.account_protection')
    expect(review.text()).toContain(p + 'review.judge')
    expect(w.get('[data-testid="product-management-quality-link"]').attributes('href')).toBe('/admin/account-quality')
    expect(w.get('[data-testid="product-management-budget-link"]').attributes('href')).toBe('/admin/settings?tab=gateway#settings-section-upstream-billing-probe')
    expect(review.findAll('button')).toHaveLength(0)
  })
  it.each(['not_enforced', 'insufficient', 'partial', 'known_lower_bound_fits'])('shows the %s shared budget independently of prices', async state => {
    const next = fixture(), review = reviewFixture(); next.management_review = review
    Object.assign(review.budget, { state, enforced: state !== 'not_enforced', unestimated_sources: state === 'partial' ? 4 : 0,
      reasons: state === 'insufficient' ? ['day_capacity'] : [] })
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    expect(validProcurementProductImpact(next, 2)).toBe(true)
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="product-management-budget-state"]').text()).toContain(p + `review.budgetStates.${state}`)
    expect(w.get('[data-testid="product-impact-conclusion"]').text()).toContain(p + 'allRejected')
  })
  it('keeps empty inventory and a truncated read distinct from no conflicts', async () => {
    const next = fixture(), review = reviewFixture(); next.management_review = review
    Object.assign(review, { rules_examined: 0, conflicting_rules: 0, protection_rules: 0, unknown_rules: 0, judge_dependency_rules: 0, rules: [], inventory_complete: false })
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.get('[data-testid="product-management-empty"]').text()).toContain(p + 'review.empty')
    expect(w.get('[data-testid="product-management-incomplete"]').text()).toContain(p + 'review.incomplete')
  })
  it.each(['version', 'mode', 'scope', 'counts', 'duplicate', 'action', 'paused-writer', 'missing-owner', 'unknown-risk', 'budget-state', 'missing-budget', 'invalid-window', 'unestimated-pass', 'unknown-pass'])('rejects a contradictory %s management contract', async kind => {
    const next = fixture(), review = reviewFixture(); next.management_review = review
    if (kind === 'version') review.version++
    if (kind === 'mode') review.mode = 'automatic'
    if (kind === 'scope') review.scope = 'all_approved_suppliers'
    if (kind === 'counts') review.conflicting_rules++
    if (kind === 'duplicate') review.rules[1]!.plan_id = 12
    if (kind === 'action') review.rules[0]!.action = 'future-action'
    if (kind === 'paused-writer') review.rules[0]!.writes_product = true
    if (kind === 'missing-owner') review.rules[0]!.retained_ownership = false
    if (kind === 'unknown-risk') review.rules[0]!.risk = 'qualified'
    if (kind === 'budget-state') review.budget.state = 'ready'
    if (kind === 'missing-budget') delete (review as Partial<ProcurementManagementReview>).budget
    if (kind === 'invalid-window') review.budget.window_end = review.budget.window_start
    if (kind === 'unestimated-pass') review.budget.state = 'known_lower_bound_fits'
    if (kind === 'unknown-pass') Object.assign(review.rules[0]!, { risk: 'unknown', action: 'observe_only' })
    expect(validProcurementProductImpact(next, 2)).toBe(false)
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    expect(w.find('[data-testid="product-management-review"]').exists()).toBe(false)
    expect(w.get('[data-testid="product-impact-error"]').text()).toContain(p + 'failed')
  })
  it('paginates rule details independently and resets the page after refresh', async () => {
    const next = fixture(), review = reviewFixture(); next.management_review = review
    review.rules = Array.from({ length: 21 }, (_, index) => ({ ...review.rules[0]!, plan_id: index + 1 }))
    Object.assign(review, { rules_examined: 21, conflicting_rules: 21, protection_rules: 0, judge_dependency_rules: 0 })
    vi.mocked(previewProcurementProductImpact).mockResolvedValue(next)
    const w = view(); await choose(w); await preview(w)
    const details = w.get('[data-testid="product-management-rules"]')
    expect(details.findAll('[data-testid="product-management-rule"]')).toHaveLength(20)
    await details.get('nav button:last-child').trigger('click')
    expect(details.findAll('[data-testid="product-management-rule"]')).toHaveLength(1)
    await preview(w)
    expect(w.findAll('[data-testid="product-management-rule"]')).toHaveLength(20)
  })
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
