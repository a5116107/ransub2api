import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { describe, expect, it } from 'vitest'
import ToolCallProbeCard from '../ToolCallProbeCard.vue'
import zh from '@/i18n/locales/zh'
import en from '@/i18n/locales/en'
import type { ToolCallProbeResult } from '@/types'

const result: ToolCallProbeResult = {
  version: 1, status: 'pass', reason: 'completed', protocol: 'responses_http', model: 'mapped-model',
  token_budget: 1024, observed_tokens: 24, arguments_verified: true, result_verified: true,
  rounds: [{ http_status: 200, latency_ms: 35, input_tokens: 10, output_tokens: 2, cached_tokens: null }]
}
function card(value?: ToolCallProbeResult, locale = 'zh') {
  return mount(ToolCallProbeCard, { props: { result: value, model: 'public-model' }, global: { plugins: [createI18n({ legacy: false, locale, messages: { zh, en } })] } })
}
describe('independent tool-call conclusions', () => {
  it.each(['zh', 'en'])('shows both stages, model mapping and actual usage in %s', locale => {
    const wrapper = card(result, locale)
    expect(wrapper.get('[data-testid="tool-verdict"]').text()).toBe(locale === 'zh' ? '通过' : 'Passed')
    expect(wrapper.text()).toContain('public-model → mapped-model')
    expect(wrapper.text()).toContain('1024')
    expect(wrapper.text()).toContain('24')
    expect(wrapper.text()).toContain('—')
    expect(wrapper.text()).not.toContain('admin.accounts.')
    wrapper.unmount()
  })
  it('keeps missing evidence neutral instead of showing a passing text result', () => {
    const wrapper = card()
    expect(wrapper.get('[data-testid="tool-verdict"]').text()).toBe('证据不足')
    expect(wrapper.text()).not.toContain('已验证')
    wrapper.unmount()
  })
  it('shows a budget deferral, not an upstream failure or a completed second turn', () => {
    const wrapper = card({ ...result, status: 'deferred', reason: 'token_budget_reached', result_verified: false })
    expect(wrapper.get('[data-testid="tool-verdict"]').text()).toBe('检测延期')
    expect(wrapper.get('[data-testid="tool-reason"]').text()).toContain('第二轮未发送')
    expect(wrapper.text()).toContain('未确认')
    wrapper.unmount()
  })
  it('does not display untrusted raw reason text or markup', () => {
    const wrapper = card({ ...result, status: 'inconclusive', reason: '<script>secret</script>' })
    expect(wrapper.text()).not.toContain('secret')
    expect(wrapper.find('script').exists()).toBe(false)
    wrapper.unmount()
  })
})
