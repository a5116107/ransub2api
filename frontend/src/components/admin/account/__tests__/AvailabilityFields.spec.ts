import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AvailabilityFields from '../AvailabilityFields.vue'
import { validAvailability } from '@/utils/availability'

vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))

describe('availability settings', () => {
  it('enables a single-connection default and emits edited limits', async () => {
    const wrapper = mount(AvailabilityFields, { props: { supported: true } })
    await wrapper.get('[data-testid="availability-enabled"]').setValue(true)
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([{ valid_for_seconds: 300, max_extra_samples: 0 }])
    await wrapper.setProps({ modelValue: { valid_for_seconds: 300, max_extra_samples: 0 } })
    await wrapper.get('[data-testid="availability-budget"]').setValue('8')
    expect(wrapper.emitted('update:modelValue')?.[1]).toEqual([{ valid_for_seconds: 300, max_extra_samples: 8 }])
    await wrapper.get('[data-testid="availability-validity"]').setValue('120')
    expect(wrapper.emitted('update:modelValue')?.[2]).toEqual([{ valid_for_seconds: 120, max_extra_samples: 0 }])
  })

  it('shows invalid or unsupported settings and permits turning them off', async () => {
    const wrapper = mount(AvailabilityFields, { props: { supported: true, modelValue: { valid_for_seconds: 29, max_extra_samples: 65 } } })
    expect(wrapper.get('[role="alert"]').text()).toBe('admin.accounts.pelicanTest.availabilityInvalid')
    await wrapper.setProps({ supported: false })
    expect(wrapper.get('[role="alert"]').text()).toBe('admin.accounts.pelicanTest.availabilityUnsupported')
    await wrapper.get('[data-testid="availability-enabled"]').setValue(false)
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([undefined])
    expect(validAvailability({ valid_for_seconds: 30, max_extra_samples: 64 }, true)).toBe(true)
    expect(validAvailability({ valid_for_seconds: 60, max_extra_samples: Number.NaN }, true)).toBe(false)
    expect(validAvailability({ valid_for_seconds: 60, max_extra_samples: 0 }, false)).toBe(false)
  })
})
