import { createPinia, setActivePinia } from 'pinia'
import { beforeEach as setupPiniaBeforeEach } from 'vitest'
setupPiniaBeforeEach(() => setActivePinia(createPinia()))
import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import PelicanTestFields from '../PelicanTestFields.vue'
import { CANDY_PROMPT } from '@/utils/intelligenceTest'
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))
const SelectStub = { name: 'Select', props: ['modelValue', 'options'], emits: ['update:modelValue'], template: '<div class="select-stub" />' }
const config = { question_kind: 'pelican' as const, prompt: 'draw a pelican', reasoning_effort: 'high', parallel_count: 4 }
function render(modelValue: Record<string, unknown> = config) {
  return mount(PelicanTestFields, { props: { modelValue: modelValue as any }, global: { stubs: { Select: SelectStub, TextArea: true, Input: true } } })
}
describe('PelicanTestFields question kinds', () => {
  it('offers the state probe and switches to a prompt-free single run', async () => {
    const wrapper = render()
    expect(wrapper.find('text-area-stub').exists()).toBe(true)
    const question = wrapper.findAllComponents(SelectStub)[0]
    expect(question.props('options').map((o: { value: string }) => o.value)).toEqual(['candy', 'pelican', 'state_probe', 'tool_call'])
    question.vm.$emit('update:modelValue', 'state_probe')
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([{ ...config, prompt_mode: 'custom', question_kind: 'state_probe', prompt: '', parallel_count: 1, reasoning_effort: 'high' }])
    wrapper.unmount()
  })
  it('hides the question and parallel fields for probe plans and restores them for candy', async () => {
    const wrapper = render({ ...config, prompt_mode: 'custom', question_kind: 'state_probe', prompt: '', parallel_count: 1 })
    expect(wrapper.find('[data-testid="state-probe-hint"]').exists()).toBe(true)
    expect(wrapper.find('text-area-stub').exists()).toBe(false)
    expect(wrapper.findAllComponents(SelectStub)).toHaveLength(1)
    wrapper.findAllComponents(SelectStub)[0].vm.$emit('update:modelValue', 'candy')
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toMatchObject({ question_kind: 'candy', prompt: CANDY_PROMPT })
    wrapper.unmount()
  })
  it('uses an independent bounded tool test, clears admission and removes stale budget when changing kind', async () => {
    const wrapper = render({ ...config, availability: { valid_for_seconds: 120 } })
    wrapper.findAllComponents(SelectStub)[0].vm.$emit('update:modelValue', 'tool_call')
    const tool = wrapper.emitted('update:modelValue')?.[0]?.[0] as any
    expect(tool).toMatchObject({ question_kind: 'tool_call', parallel_count: 1, prompt: '', tool_call_token_budget: 1024, reasoning_effort: 'low' })
    expect(tool.availability).toBeUndefined()
    expect(tool.quality).toBeUndefined()
    await wrapper.setProps({ modelValue: tool })
    expect(wrapper.find('text-area-stub').exists()).toBe(false)
    expect(wrapper.findAllComponents(SelectStub)).toHaveLength(1)
    wrapper.findAllComponents(SelectStub)[0].vm.$emit('update:modelValue', 'candy')
    expect((wrapper.emitted('update:modelValue')?.[1]?.[0] as any).tool_call_token_budget).toBeUndefined()
    wrapper.unmount()
  })
})
