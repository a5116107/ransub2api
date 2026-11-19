import { mount, flushPromises } from '@vue/test-utils'
import { reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import UpstreamCacheProbe from '../UpstreamCacheProbe.vue'
import { readUpstreamCacheProbeRun } from '@/api/admin/upstreamCacheProbe'
import { upstreamCacheResult } from '@/api/__tests__/upstreamCacheProbe.fixture'
import zh from '@/i18n/locales/zh/admin/groupCacheTest'
import en from '@/i18n/locales/en/admin/groupCacheTest'

const mocks = vi.hoisted(() => ({ list: vi.fn(), run: vi.fn(), save: vi.fn(), auth: { isAdmin: true, user: { id: 1 } } }))
vi.mock('@/api/admin/accounts', () => ({ list: mocks.list }))
vi.mock('@/api/admin/upstreamCacheProbe', async original => ({ ...await original<typeof import('@/api/admin/upstreamCacheProbe')>(), runUpstreamCacheProbe: mocks.run }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => mocks.auth }))
vi.mock('file-saver', () => ({ saveAs: mocks.save }))
vi.mock('vue-i18n', async original => ({ ...await original<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
const wrappers: ReturnType<typeof mount>[] = []
const render = async () => { const wrapper = mount(UpstreamCacheProbe); wrappers.push(wrapper); await flushPromises(); return wrapper }
beforeEach(() => { mocks.list.mockReset().mockResolvedValue({ items: [{ id: 7, name: 'Upstream fixture' }] }); mocks.run.mockReset().mockResolvedValue(undefined); mocks.save.mockReset(); mocks.auth = reactive({ isAdmin: true, user: { id: 1 } }) })
afterEach(() => { wrappers.splice(0).forEach(w => w.unmount()) })

describe('upstream cache probe panel', () => {
  it('requires an explicit upstream target and valid intervals, without fetching a downstream key', async () => {
    const wrapper = await render()
    expect(mocks.list).toHaveBeenCalledWith(1, 100, expect.objectContaining({ platform: 'openai', type: 'apikey' }), expect.anything())
    expect(wrapper.get('[data-testid="upstream-cache-start"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="upstream-cache-account"]').setValue('7')
    await wrapper.get('[data-testid="upstream-cache-model"]').setValue('public-model')
    await wrapper.get('[data-testid="upstream-cache-intervals"]').setValue('1, 3')
    expect(wrapper.get('[data-testid="upstream-cache-start"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.run).toHaveBeenCalledWith(7, expect.objectContaining({ model: 'public-model', intervals_ms: [1000, 3000] }), expect.any(AbortSignal), expect.any(Function))
    await wrapper.get('[data-testid="upstream-cache-intervals"]').setValue('1,')
    expect(wrapper.get('[data-testid="upstream-cache-start"]').attributes('disabled')).toBeDefined()
  })
  it('shows account loading failures and recovers without keeping a stale selection', async () => {
    mocks.list.mockRejectedValueOnce(new Error('fixture'))
    const wrapper = await render()
    expect(wrapper.get('[role="alert"]').text()).toContain('upstream.loadError')
    await wrapper.findAll('button').find(b => b.text().endsWith('reload'))!.trigger('click'); await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })
  it('cancels the active call when unmounted', async () => {
    let signal: AbortSignal | undefined
    mocks.run.mockImplementation((_id, _input, current) => { signal = current; return new Promise(() => {}) })
    const wrapper = await render()
    await wrapper.get('[data-testid="upstream-cache-account"]').setValue('7')
    await wrapper.get('[data-testid="upstream-cache-model"]').setValue('public-model')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    wrapper.unmount()
    expect(signal?.aborted).toBe(true)
  })
  it('has equal Chinese and English conclusion labels', () => {
    expect(Object.keys(zh.groupCacheTest.upstream.verdict)).toEqual(Object.keys(en.groupCacheTest.upstream.verdict))
    expect(zh.groupCacheTest.upstream.boundary).toContain('不授予')
    expect(en.groupCacheTest.upstream.boundary).toContain('do not grant')
    expect(readUpstreamCacheProbeRun).toBeTypeOf('function')
  })
  it.each(['disconnect', 'cancel'])('withdraws optimistic results after %s and ignores late progress', async scenario => {
    let reject: (error: Error) => void = () => {}, progress: (run: ReturnType<typeof upstreamCacheResult>) => void = () => {}
    let signal: AbortSignal | undefined
    mocks.run.mockImplementation((_id, _input, current, changed) => {
      signal = current; progress = changed
      changed(upstreamCacheResult())
      return new Promise((_resolve, fail) => { reject = fail })
    })
    const wrapper = await render()
    await wrapper.get('[data-testid="upstream-cache-account"]').setValue('7')
    await wrapper.get('[data-testid="upstream-cache-model"]').setValue('public-model')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[data-testid="upstream-cache-verdict"]').text()).toContain('hit_observed')
    if (scenario === 'cancel') {
      await wrapper.findAll('button').find(b => b.text().endsWith('.stop'))!.trigger('click')
      expect(signal?.aborted).toBe(true)
    }
    reject(new Error('fixture network ended')); await flushPromises()
    if (scenario === 'cancel') { progress(upstreamCacheResult()); await flushPromises() }
    expect(wrapper.get('[data-testid="upstream-cache-verdict"]').text()).toContain('insufficient')
    expect(wrapper.get('[role="alert"]').text()).toContain(scenario === 'cancel' ? 'stopped' : 'runError')
    expect(wrapper.get('[data-testid="upstream-cache-start"]').attributes('disabled')).toBeUndefined()
    await wrapper.findAll('button').find(b => b.text().endsWith('.export'))!.trigger('click')
    const blob = mocks.save.mock.calls[0]![0] as Blob
    const raw = await new Promise<string>((resolve, reject) => { const reader = new FileReader(); reader.onload = () => resolve(String(reader.result)); reader.onerror = reject; reader.readAsText(blob) })
    expect(readUpstreamCacheProbeRun(JSON.parse(raw)).verdict).toBe('insufficient')
  })
  it('clears private upstream results and cancels requests when the administrator changes', async () => {
    let signal: AbortSignal | undefined, progress: (run: ReturnType<typeof upstreamCacheResult>) => void = () => {}
    mocks.run.mockImplementation((_id, _input, current, changed) => { signal = current; progress = changed; changed(upstreamCacheResult()); return new Promise(() => {}) })
    const wrapper = await render()
    await wrapper.get('[data-testid="upstream-cache-account"]').setValue('7')
    await wrapper.get('[data-testid="upstream-cache-model"]').setValue('public-model')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    mocks.auth.user.id = 2; await flushPromises()
    expect(signal?.aborted).toBe(true)
    progress(upstreamCacheResult()); await flushPromises()
    expect(wrapper.find('[data-testid="upstream-cache-result"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="upstream-cache-account"]').element).toHaveProperty('selectedIndex', 0)
    await wrapper.get('[data-testid="upstream-cache-account"]').setValue('7')
    expect(wrapper.get('[data-testid="upstream-cache-start"]').attributes('disabled')).toBeUndefined()
  })
})
