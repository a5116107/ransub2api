import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import { readOperations, type OperationsSnapshot } from '@/api/admin/upstreamOperations'
import UpstreamChannelOperationsView from '../UpstreamChannelOperationsView.vue'
vi.mock('@/components/admin/operations/UpstreamLineCards.vue', () => ({ default: { props: ['snapshot'], emits: ['select','edit','configure'], template: `<section data-testid="line-panel"><button @click="$emit('select',42)">Read line</button></section>` } }))
vi.mock('@/api/admin/pelicanTests', () => ({ getResult: vi.fn() }))
vi.mock('@/api/admin/accounts', () => ({ getById: vi.fn() }))
vi.mock('@/api/admin/groups', () => ({ getAllIncludingInactive: vi.fn() }))
vi.mock('@/api/admin/proxies', () => ({ getAll: vi.fn() }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
vi.mock('@/components/admin/operations/MatureProvidersPanel.vue', () => ({ default: { template: '<section />' } }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/upstreamOperations', () => ({ readOperations: vi.fn() }))
describe('upstream channel definition', () => {
 it('discovers URLs without creating or fetching model-pricing channels, including idle accounts', async () => {
  vi.mocked(readOperations).mockResolvedValue({ scope: {}, usage: { summary: {}, rows: [], accounts: [], members: [{id:42,name:'Idle account'}],member_total:1, total: 0, catalog_total: 1, catalog: [{key:'https://idle.example:8443/tenant/v1',name:'https://idle.example:8443/tenant/v1',accounts:2}] }, quality: {status:'unsupported_scope'}, errors:{status:'query_failed'} } as unknown as OperationsSnapshot)
  const w=mount(UpstreamChannelOperationsView); await flushPromises()
  expect(w.text()).toContain('upstreamOps.overviewTitle'); expect(w.text()).toContain('https://idle.example:8443/tenant/v1')
  expect(w.text()).toContain('upstreamOps.latencyNote'); expect(w.text()).toContain('upstreamOps.qualityNote'); expect(w.text()).toContain('upstreamOps.errorNote')
  expect(readOperations).toHaveBeenCalledWith(expect.objectContaining({dimension:'upstream'}),expect.anything())
  expect(w.find('[data-testid="new-channel"]').exists()).toBe(false)
  expect(w.find('[data-testid="line-panel"]').exists()).toBe(true)
  const idle=w.findAll('button').find(b=>b.text().includes('idle.example'))!;await idle.trigger('click');await flushPromises()
  expect(readOperations).toHaveBeenLastCalledWith(expect.objectContaining({upstream:'https://idle.example:8443/tenant/v1',account_id:0,dimension:'account'}),expect.anything())
  await w.get('[data-testid="member-drill"]').trigger('click');await flushPromises()
  expect(readOperations).toHaveBeenLastCalledWith(expect.objectContaining({account_id:42,dimension:'model'}),expect.anything())
  w.unmount()
 })
})

it('paginates idle lines and removes stale cards on read failure before retry', async () => {
 const snapshot = { scope: {}, usage: { summary: {}, rows: [], accounts: [], members: [], member_total: 21, total: 0, catalog_total: 0, catalog: [] }, quality: {status:'unsupported_scope'}, errors:{status:'query_failed'} } as unknown as OperationsSnapshot
 vi.mocked(readOperations).mockResolvedValue(snapshot)
 const w = mount(UpstreamChannelOperationsView); await flushPromises()
 const next = w.get('[data-testid="operations-next"]')
 expect(next.attributes('disabled')).toBeUndefined()
 vi.mocked(readOperations).mockRejectedValueOnce(new Error('synthetic unavailable database'))
 await next.trigger('click'); await flushPromises()
 expect(readOperations).toHaveBeenLastCalledWith(expect.objectContaining({page:2}), expect.anything())
 expect(w.find('[data-testid="line-panel"]').exists()).toBe(false)
 expect(w.get('[role="alert"]').text()).toContain('upstreamOps.queryFailed')
 await w.get('form').trigger('submit'); await flushPromises()
 expect(w.find('[data-testid="line-panel"]').exists()).toBe(true)
 w.unmount()
})
