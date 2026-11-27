import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import en from '@/i18n/locales/en/dashboard'
import zh from '@/i18n/locales/zh/dashboard'
import { getMyErrorDetail } from '@/api/usage'
import { userErrorMessage } from '@/utils/userErrorMessage'
import UserErrorDetailModal from '../UserErrorDetailModal.vue'

vi.mock('@/api/usage', () => ({ getMyErrorDetail: vi.fn(async () => ({
  id: 833, created_at: '2026-11-13T10:00:00Z', model: 'example-model', inbound_endpoint: '/v1/responses',
  status_code: 200, category: 'transport_error', platform: 'openai', message: 'safe server fallback',
  reset_reason: 'INTERNAL_ERROR', error_body: '{"error":{"code":"upstream_http2_stream_error"}}', key_name: 'Demo key', key_deleted: false
})) }))
enableAutoUnmount(afterEach)

describe('peer reset detail in the real user component', () => {
  it.each([
    ['en', 'Transport error', 'reset by peer', 'Content may be incomplete', 'Do not automatically retry', 'Actual client HTTP status', 'HTTP 200 does not mean generation completed', 'Gateway-generated error details'],
    ['zh', '传输错误', '被对端重置', '内容可能不完整', '请勿自动重试', '客户端实际 HTTP 状态码', 'HTTP 200 不表示生成已完成', '网关生成的错误详情']
  ])('renders safe %s copy and the committed status', async (locale, category, reset, incomplete, retry, statusLabel, explanation, generatedBodyLabel) => {
    const i18n = createI18n({ legacy: false, locale, messages: { en, zh } })
    const wrapper = mount(UserErrorDetailModal, {
      props: { show: false, errorId: 833 },
      global: { plugins: [i18n], stubs: { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' } } }
    })
    await wrapper.setProps({ show: true }); await flushPromises()
    for (const text of [category, reset, incomplete, retry, statusLabel, explanation, generatedBodyLabel, 'HTTP/2 INTERNAL_ERROR', '200']) expect(wrapper.text()).toContain(text)
    for (const text of ['502', 'stream ID', 'safe server fallback', 'usage.errors.', '上游响应内容']) expect(wrapper.text()).not.toContain(text)
  })

  it.each([
    ['en', 'Upstream response stream was interrupted', 'Content may be incomplete', 'Do not automatically retry'],
    ['zh', '上游响应流中断', '内容可能不完整', '请勿自动重试']
  ])('does not invent HTTP/2 for a generic %s reader failure', async (locale, interrupted, incomplete, retry) => {
    vi.mocked(getMyErrorDetail).mockResolvedValueOnce({
      id: 834, created_at: '2026-11-27T08:51:10Z', model: 'example-model', inbound_endpoint: '/v1/responses',
      status_code: 200, category: 'transport_error', platform: 'openai', message: 'private-address',
      error_body: '{"error":{"code":"upstream_stream_read_error"}}', key_name: 'Demo key', key_deleted: false
    })
    const i18n = createI18n({ legacy: false, locale, messages: { en, zh } })
    const wrapper = mount(UserErrorDetailModal, {
      props: { show: false, errorId: 834 },
      global: { plugins: [i18n], stubs: { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' } } }
    })
    await wrapper.setProps({ show: true }); await flushPromises()
    for (const text of [interrupted, incomplete, retry, '200']) expect(wrapper.text()).toContain(text)
    for (const text of ['HTTP/2', 'INTERNAL_ERROR', 'private-address', '502', 'usage.errors.']) expect(wrapper.text()).not.toContain(text)
  })

  it.each([undefined, '', '{invalid', 'null', '{"error":{"code":"other"}}'])('keeps unknown transport evidence neutral: %s', errorBody => {
    const t = (key: string) => key
    expect(userErrorMessage({ category: 'transport_error', message: 'private-address', error_body: errorBody }, t))
      .toBe('usage.errors.streamInterrupted')
  })

  it('preserves verified HTTP/2 detail without a reset reason', () => {
    const t = (key: string, values?: Record<string, string>) => `${key}:${values?.protocol}`
    expect(userErrorMessage({ category: 'transport_error', message: '', error_body: '{"error":{"code":"upstream_http2_stream_error"}}' }, t))
      .toBe('usage.errors.peerReset:HTTP/2')
  })
})
