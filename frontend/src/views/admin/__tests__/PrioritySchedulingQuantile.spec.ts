import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { baseCompile } from '@intlify/message-compiler'
function messages(value: unknown): any {
  if (typeof value === 'string') return new Function(`return ${baseCompile(value, { mode: 'arrow' }).code}`)()
  return Object.fromEntries(Object.entries(value as Record<string, unknown>).map(([key, item]) => [key, messages(item)]))
}
import PrioritySchedulingView from '../PrioritySchedulingView.vue'
import en from '@/i18n/locales/en/priorityScheduling'
import zh from '@/i18n/locales/zh/priorityScheduling'
const api = vi.hoisted(() => ({ config: vi.fn(), snapshot: vi.fn(), save: vi.fn(), recompute: vi.fn(), latency: vi.fn(), sticky: vi.fn(), clearSticky: vi.fn() }))
vi.mock('@/api/admin/priorityScheduling', () => ({ getPriorityConfig: api.config, getPrioritySnapshot: api.snapshot, savePriorityConfig: api.save, recomputePriorityScheduling: api.recompute, getPriorityLatency: api.latency, getPriorityStickyBindings: api.sticky, clearPriorityStickyBindings: api.clearSticky }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 1, role: 'admin' } }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
beforeEach(() => {
  vi.resetAllMocks()
  api.config.mockResolvedValue({ enabled: true, mode: 'balanced', group_ids: [], models: [], window_minutes: 180, min_samples: 5, target_ttft_ms: 15000, quality_weight:30,latency_weight:25,load_weight:25,cost_weight:20 })
  api.save.mockImplementation(async c => c)
  api.latency.mockResolvedValue({ at:'2026-10-21T01:00:00Z', model:'mock-model', group_id:11, bucket_minutes:5, accounts:[] })
  api.sticky.mockResolvedValue({ group_id:11, total:0, by_account:[] })
  api.snapshot.mockResolvedValue({ at:'2026-10-21T01:00:00Z', evaluated_at:'2026-10-21T01:02:00Z', history_observed_at:'2026-10-21T01:01:00Z', latency_percentile:70, latency_measurement:'attributed_v1', min_samples:5, model:'mock-model',group_id:11,mode:'balanced',history_ready:true,candidates:[{ account_id:1,account_name:'Mock account',score:50,tier:'insufficient',reasons:[],priority:1,samples:3,selected_ttft_ms:7000,p90_ttft_ms:20000,latency_percentile:70,latency_measurement:'attributed_v1',latency_unknown_samples:2,error_rate:0.25,error_successes:3,error_failures:1,error_window_minutes:10,error_prior_samples:5,error_observed_at:'2026-10-21T01:02:00Z',error_endpoint:'/v1/responses',error_penalty:0.9,base_score:55,quality_samples:0,quality_passed:0,rate:null,load_percent:0,waiting:0,economics_source:'unknown',experience_samples:3,experience_p90_ms:50000 }] })
})
describe('quantiles and measurement boundaries', () => {
  describe.each(['en', 'zh'])('candidate latency evidence in %s', locale => {
    it.each([
      { name:'known', value:1234.6, expected:'1235 ms' },
      { name:'explicit null', value:null, expected:null },
      { name:'zero', value:0, expected:null },
      { name:'negative', value:-1, expected:null },
      { name:'nonfinite', value:Infinity, expected:null },
      { name:'NaN', value:NaN, expected:null },
      { name:'missing attributed', value:undefined, expected:null },
      { name:'missing legacy P95', value:undefined, measurement:'legacy_forward', expected:null },
      { name:'missing legacy P90', value:undefined, measurement:'legacy_forward', percentile:90, expected:'20000 ms' },
      { name:'explicit null legacy P90', value:null, measurement:'legacy_forward', percentile:90, expected:null },
      { name:'no samples', value:1234.6, samples:0, expected:null },
    ])('keeps $name selected latency distinct from diagnostic P90', async tc => {
      const snapshot = await api.snapshot()
      snapshot.candidates[0] = { ...snapshot.candidates[0], selected_ttft_ms:tc.value,
        latency_percentile:tc.percentile ?? 95, latency_measurement:tc.measurement ?? 'attributed_v1',
        p95_ttft_ms:7777, samples:tc.samples ?? 3 }
      api.snapshot.mockResolvedValue(snapshot)
      const i18n = createI18n({ legacy:false, locale, messages:{ [locale]:{ priorityScheduling:messages(locale==='en' ? en : zh) } } })
      const w = mount(PrioritySchedulingView, { global:{ plugins:[i18n] } })
      try {
        await flushPromises()
        const cell = w.get('[data-testid="priority-candidate-scroll"] tbody tr').findAll('td')[6]!
        expect(cell.element.childNodes[0]?.textContent?.trim()).toBe(`P${tc.percentile ?? 95}: ${tc.expected ?? (locale==='en' ? 'Unknown' : '未知')}`)
        expect(cell.text()).not.toMatch(/7777 ms|NaN ms|Infinity ms|-1 ms/)
        expect(api.save).not.toHaveBeenCalled()
        expect(api.recompute).not.toHaveBeenCalled()
        expect(api.clearSticky).not.toHaveBeenCalled()
      } finally {
        w.unmount()
      }
    })

    it.each([
      { name:'known', value:1234.6, expected:'1235 ms' },
      { name:'missing', value:undefined, expected:'—' },
      { name:'null', value:null, expected:'—' },
      { name:'zero', value:0, expected:'—' },
      { name:'negative', value:-1, expected:'—' },
      { name:'nonfinite', value:Infinity, expected:'—' },
      { name:'NaN', value:NaN, expected:'—' },
      { name:'no samples', value:1234.6, samples:0, expected:'—' },
    ])('does not turn $name diagnostic evidence into a measured zero', async tc => {
      const snapshot = await api.snapshot()
      snapshot.candidates[0] = { ...snapshot.candidates[0], samples:tc.samples ?? 3, p90_ttft_ms:tc.value,
        experience_samples:tc.samples ?? 3, experience_p90_ms:tc.value }
      api.snapshot.mockResolvedValue(snapshot)
      const i18n = createI18n({ legacy:false, locale, messages:{ [locale]:{ priorityScheduling:messages(locale==='en' ? en : zh) } } })
      const w = mount(PrioritySchedulingView, { global:{ plugins:[i18n] } })
      try {
        await flushPromises()
        const cell = w.get('[data-testid="priority-candidate-scroll"] tbody tr').findAll('td')[6]!
        const diagnostics = cell.findAll('small')
        expect(diagnostics.at(-2)?.text()).toBe(`P90: ${tc.expected}`)
        expect(diagnostics.at(-1)?.text()).toBe(`${(locale==='en' ? en : zh).experienceP90}: ${tc.expected}${tc.samples === 0 ? '' : ' (3)'}`)
      } finally {
        w.unmount()
      }
    })

    it.each([
      { name:'negative', value:-1, expected:'—', color:'text-gray-400' },
      { name:'infinite', value:Infinity, expected:'—', color:'text-gray-400' },
      { name:'missing', value:undefined, expected:'—', color:'text-gray-400' },
      { name:'zero', value:0, expected:'—', color:'text-gray-400' },
      { name:'fast', value:1000, expected:'1000 ms', color:'text-emerald-600' },
      { name:'slow', value:20000, expected:'20000 ms', color:'text-amber-600' },
      { name:'very slow', value:31000, expected:'31000 ms', color:'text-red-600' },
    ])('uses an honest color for $name diagnostic latency', async tc => {
      api.latency.mockResolvedValue({ at:'2026-10-21T01:00:00Z', model:'mock-model', group_id:11, bucket_minutes:5,
        accounts:[{ account_id:1, account_name:'Diagnostic fixture', samples:3, p50_ttft_ms:500, p90_ttft_ms:tc.value,
          p99_ttft_ms:40000, selected_ttft_ms:null, slow_samples:0, error_samples:0, error_rate:0, buckets:[] }] })
      const i18n = createI18n({ legacy:false, locale, messages:{ [locale]:{ priorityScheduling:messages(locale==='en' ? en : zh) } } })
      const w = mount(PrioritySchedulingView, { global:{ plugins:[i18n] } })
      try {
        await flushPromises()
        const cell = w.get('[data-testid="latency-panel"] tbody tr').findAll('td')[3]!
        expect(cell.text()).toBe(tc.expected)
        expect(cell.classes()).toContain(tc.color)
      } finally {
        w.unmount()
      }
    })
  })

  describe.each(['en', 'zh'])('recompute selected latency in %s', locale => {
    it.each([
      { name:'selected value', old_selected_ttft_ms:950, new_selected_ttft_ms:1500, expected:'950 ms → 1500 ms' },
      { name:'became unknown', old_selected_ttft_ms:950, new_selected_ttft_ms:null, expected:'950 ms → —' },
      { name:'recovered', old_selected_ttft_ms:null, new_selected_ttft_ms:1500, expected:'— → 1500 ms' },
      { name:'both unknown', old_selected_ttft_ms:null, new_selected_ttft_ms:null, expected:'— → —' },
      { name:'legacy missing', expected:'— → —' },
      { name:'invalid nonpositive', old_selected_ttft_ms:0, new_selected_ttft_ms:-1, expected:'— → —' },
    ])('shows $name without borrowing P90 or clearing sessions', async tc => {
      const snapshot = { ...await api.snapshot(), history_status:'ready', latency_percentile:95, latency_measurement:'attributed_v1' }
      api.snapshot.mockResolvedValue(snapshot)
      api.recompute.mockResolvedValue({ before:snapshot, after:snapshot, timed_out:false, diff:[{
        account_id:1, account_name:'Recompute account', old_rank:1, new_rank:1, old_score:50, new_score:50,
        old_weight:1, new_weight:1, old_tier:'eligible', new_tier:'eligible', old_p90_ttft_ms:20000, new_p90_ttft_ms:21000,
        old_samples:10, new_samples:10, old_error_samples:0, new_error_samples:0, changed:true,
        ...('old_selected_ttft_ms' in tc ? { old_selected_ttft_ms:tc.old_selected_ttft_ms, new_selected_ttft_ms:tc.new_selected_ttft_ms } : {}),
      }] })
      const i18n = createI18n({ legacy:false, locale, messages:{ [locale]:{ priorityScheduling:messages(locale==='en' ? en : zh) } } })
      const w = mount(PrioritySchedulingView, { global:{ plugins:[i18n] } })
      try {
        await flushPromises()
        await w.get('[data-testid="recompute"]').trigger('click')
        await flushPromises()
        const cell = w.get('[data-testid="recompute-latency-1"]')
        expect(cell.get('[data-testid="recompute-latency-values"]').text()).toBe(tc.expected)
        expect(cell.text()).toContain('P95')
        expect(cell.text()).toContain('attributed_v1')
        expect(cell.text()).not.toMatch(/20000|21000/)
        expect(w.get('[data-testid="recompute-diff"]').text()).toContain(locale==='en' ? 'Selected first token' : '选定首字')
        expect(w.get('[data-testid="recompute-hint"]').text()).toContain(locale==='en' ? 'New sessions' : '新会话')
        expect(w.get('[data-testid="recompute-hint"]').text()).toContain(locale==='en' ? 'cold' : '冷启动')
        expect(w.text()).not.toMatch(/priorityScheduling\./)
        expect(api.clearSticky).not.toHaveBeenCalled()
      } finally {
        w.unmount()
      }
    })
  })
  it.each(['en','zh'])('renders actual %s translations and submits the percentile and measurement', async locale => {
    const i18n = createI18n({ legacy:false, locale, messages:{ en:{priorityScheduling:messages(en)},zh:{priorityScheduling:messages(zh)} } })
    const w=mount(PrioritySchedulingView,{global:{plugins:[i18n]}})
    await flushPromises()
    expect(w.get('[data-testid="latency-percentile"]').element).toHaveProperty('value','90')
    await w.get('[data-testid="latency-percentile"]').setValue('95')
    await w.get('[data-testid="latency-measurement"]').setValue('attributed_v1')
    await w.get('form').trigger('submit');await flushPromises()
    expect(api.save).toHaveBeenCalledWith(expect.objectContaining({latency_percentile:95,latency_measurement:'attributed_v1',recovery_exploration_percent:0}))
    expect(w.text()).toContain('P70')
    expect(w.text()).toContain('7000 ms')
    expect(w.get('[data-testid="error-rate-1"]').text()).toContain('25.0%')
    expect(w.get('[data-testid="error-rate-1"]').text()).toContain(locale==='en'?'Failures / valid attempts':'失败 / 有效尝试')
    expect(w.get('[data-testid="error-rate-1"]').text()).toContain(locale==='en'?'Insufficient samples':'样本不足')
    expect(w.get('[data-testid="error-rate-1"]').text()).toContain(locale==='en'?'Error evidence read':'错误统计读取')
    expect(w.get('[data-testid="error-rate-1"]').text()).toContain(locale==='en'?'Error window':'错误窗口')
    expect(w.get('[data-testid="error-rate-1"]').attributes('title')).toContain(locale==='en'?'excluded':'排除')
    expect(w.text()).toContain(locale==='en'?'This instance · HTTP attempts':'当前实例 · HTTP 尝试')
    expect(w.text()).toContain(locale==='en'?'Low sample count':'低样本')
    expect(w.text()).toContain(locale==='en'?'not a global probability':'不是全局概率')
    expect(w.text()).toContain(locale==='en'?'client receipt':'客户端收到')
    expect(w.text()).not.toMatch(/priorityScheduling\./)
    w.unmount()
  })
  it('keeps explicit null attribution unknown instead of showing a legacy P90 as the selected latency', async () => {
    const snapshot = await api.snapshot()
    snapshot.candidates[0] = { ...snapshot.candidates[0], selected_ttft_ms: null, latency_percentile: 90, latency_measurement: 'legacy_forward', samples: 0 }
    api.snapshot.mockResolvedValue(snapshot)
    const i18n = createI18n({ legacy:false, locale:'en', messages:{ en:{ priorityScheduling:messages(en) } } })
    const w = mount(PrioritySchedulingView, { global:{ plugins:[i18n] } })
    await flushPromises()
    expect(w.text()).toContain('P90: Unknown')
    expect(w.text()).not.toContain('P90: 20000 ms')
    w.unmount()
  })

})
