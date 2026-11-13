import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import OpenAIAPIKeyHealthBreakerSettings from './OpenAIAPIKeyHealthBreakerSettings.vue'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

enableAutoUnmount(afterEach)
const api = vi.hoisted(() => ({ getOpenAIAPIKeyHealthBreakerSettings: vi.fn(), updateOpenAIAPIKeyHealthBreakerSettings: vi.fn() }))
vi.mock('@/api', () => ({ adminAPI: { settings: api } }))
const saved = { enabled: false, window_minutes: 5, failure_threshold: 3, cooldown_minutes: 1 }
beforeEach(() => {
  vi.resetAllMocks()
  api.getOpenAIAPIKeyHealthBreakerSettings.mockResolvedValue({ ...saved })
  api.updateOpenAIAPIKeyHealthBreakerSettings.mockImplementation(async settings => settings)
})

function render(locale = 'zh') {
  return mount(OpenAIAPIKeyHealthBreakerSettings, { global: { plugins: [createI18n({ legacy: false, locale, missingWarn: false, fallbackWarn: false, messages: { en, zh } })] } })
}

describe('OpenAI API key health breaker settings', () => {
  it.each(['en', 'zh'])('renders translated labels and warnings in %s', async locale => {
    const wrapper = render(locale)
    await flushPromises()
    expect(wrapper.text()).not.toContain('admin.settings.openaiAPIKeyHealthBreaker.')
    expect(wrapper.get('[role="switch"]').attributes('aria-labelledby')).toBe('apikey-health-enabled-label')
  })
  it('loads the saved values, saves the toggle and reads server normalization', async () => {
    const wrapper = render()
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('false')
    expect(wrapper.get('#apikey-health-window_minutes').element).toHaveProperty('value', '5')
    await wrapper.get('[role="switch"]').trigger('click')
    api.updateOpenAIAPIKeyHealthBreakerSettings.mockResolvedValueOnce({ ...saved, enabled: true, failure_threshold: 4 })
    await wrapper.get('[data-testid="apikey-health-save"]').trigger('click')
    await flushPromises()
    expect(api.updateOpenAIAPIKeyHealthBreakerSettings).toHaveBeenCalledWith({ ...saved, enabled: true })
    expect(wrapper.get('#apikey-health-failure_threshold').element).toHaveProperty('value', '4')
    expect(wrapper.find('[role="status"]').exists()).toBe(true)
  })

  it.each(['0', '61', '1.5', ''])('rejects invalid minutes %s before sending', async value => {
    const wrapper = render()
    await flushPromises()
    await wrapper.get('#apikey-health-window_minutes').setValue(value)
    await wrapper.get('[data-testid="apikey-health-save"]').trigger('click')
    expect(api.updateOpenAIAPIKeyHealthBreakerSettings).not.toHaveBeenCalled()
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
  })

  it('hides editing after load failure and reloads explicitly', async () => {
    api.getOpenAIAPIKeyHealthBreakerSettings.mockRejectedValueOnce(new Error('read failed'))
    const wrapper = render('en')
    await flushPromises()
    expect(wrapper.find('[data-testid="apikey-health-save"]').exists()).toBe(false)
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
    await wrapper.get('[data-testid="apikey-health-reload"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="apikey-health-save"]').exists()).toBe(true)
  })

  it('keeps the edited draft on write failure and prevents duplicate writes', async () => {
    let rejectSave!: (reason: Error) => void
    api.updateOpenAIAPIKeyHealthBreakerSettings.mockReturnValueOnce(new Promise((_, reject) => { rejectSave = reject }))
    const wrapper = render()
    await flushPromises()
    await wrapper.get('#apikey-health-failure_threshold').setValue('7')
    await wrapper.get('[data-testid="apikey-health-save"]').trigger('click')
    await wrapper.get('[data-testid="apikey-health-save"]').trigger('click')
    expect(api.updateOpenAIAPIKeyHealthBreakerSettings).toHaveBeenCalledTimes(1)
    expect(wrapper.get('fieldset').attributes('disabled')).toBeDefined()
    rejectSave(new Error('write failed'))
    await flushPromises()
    expect(wrapper.get('#apikey-health-failure_threshold').element).toHaveProperty('value', '7')
    expect(wrapper.get('fieldset').attributes('disabled')).toBeUndefined()
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
  })
})
