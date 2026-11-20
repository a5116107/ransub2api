import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProcurementProductImpactView from '../ProcurementProductImpact.vue'
import Select from '@/components/common/Select.vue'
import { list as listGroups } from '@/api/admin/groups'
import { previewProcurementProductImpact, type ProcurementManagementReview, type ProcurementProductImpact, type ProcurementSourceInventory, type ProcurementWriterReview } from '@/api/admin/procurementCatalog'
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
