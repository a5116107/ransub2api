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
