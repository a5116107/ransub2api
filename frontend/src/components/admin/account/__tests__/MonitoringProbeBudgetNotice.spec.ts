import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh/modelTrace'
import en from '@/i18n/locales/en/modelTrace'
import MonitoringProbeBudgetNotice from '../MonitoringProbeBudgetNotice.vue'

describe('plugin monitoring deferral', () => {
  it.each(['zh', 'en'])('explains unmetered automatic probes in %s without promising a timed recovery', async (locale) => {
    const i18n = createI18n({ legacy: false, locale, messages: { zh: { modelTrace: zh }, en: { modelTrace: en } } })
    const messages = locale === 'zh' ? zh : en
    const wrapper = mount(MonitoringProbeBudgetNotice, {
      props: {
        title: 'caller budget title',
        deferral: { reason: 'plugin_metering_unavailable', scope: 'plugin', retry_at: '2026-11-19T00:00:00Z' }
      },
      global: { plugins: [i18n] }
    })
    expect(wrapper.attributes('role')).toBe('status')
    expect(wrapper.find('time').exists()).toBe(false)
    expect(wrapper.text()).toContain(messages.pluginMeteringDeferred)
    expect(wrapper.text()).toContain(messages.pluginMeteringReason)
    expect(wrapper.text()).toContain(messages.pluginMeteringResume)
    expect(wrapper.text()).toContain(messages.budgetScopes.plugin)
    expect(wrapper.text()).not.toContain('caller budget title')
    expect(wrapper.text()).not.toContain(messages.retryAfter)
    expect(wrapper.classes()).not.toContain('text-red-700')
    await wrapper.setProps({ deferral: { reason: 'unrecognized PRIVATE_REASON', scope: 'PRIVATE_SCOPE', retry_at: 'invalid' } })
    expect(wrapper.text()).toContain('caller budget title')
    expect(wrapper.text()).toContain(messages.budgetReasonUnknown)
    expect(wrapper.text()).toContain(messages.budgetScopes.unknown)
    expect(wrapper.text()).not.toContain('PRIVATE_')
    expect(wrapper.text()).not.toContain(messages.pluginMeteringDeferred)
    wrapper.unmount()
  })
})
