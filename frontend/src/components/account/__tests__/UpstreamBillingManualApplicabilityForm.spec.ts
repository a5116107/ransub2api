import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import UpstreamBillingManualApplicabilityForm from '../UpstreamBillingManualApplicabilityForm.vue'
import type { Account, UpstreamBillingManualApplicabilityUpdate } from '@/types'

enableAutoUnmount(afterEach)
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const account = (current?: Account['extra']) => ({ id: 42, type: 'apikey', extra: current } as Account)
const build = (wrapper: ReturnType<typeof mount>) => (wrapper.vm as unknown as { buildUpdate: () => UpstreamBillingManualApplicabilityUpdate | undefined }).buildUpdate()

describe('manual rate applicability editor', () => {
  it('does not silently renew on an ordinary save', () => {
    const wrapper = mount(UpstreamBillingManualApplicabilityForm, { props: { account: account(), show: true } })
    expect(build(wrapper)).toBeUndefined()
  })

  it('requires reviewed evidence and explicit confirmation, then returns exact scopes', async () => {
    const wrapper = mount(UpstreamBillingManualApplicabilityForm, { props: { account: account(), show: true } })
    await wrapper.get('[data-testid="manual-edit"]').setValue(true)
    expect(() => build(wrapper)).toThrow('manualApplicability.invalid')
    await wrapper.get('[data-testid="manual-version"]').setValue('review-1')
    await wrapper.get('[data-testid="manual-reason"]').setValue('reviewed supplier contract')
    await wrapper.get('[data-testid="manual-model"]').setValue('wire-fixture')
    await wrapper.get('[data-testid="manual-confirm"]').setValue(true)
    expect(build(wrapper)).toMatchObject({ expected_revision: '', confirm_current_binding: true, source_version: 'review-1', entries: [{ model: 'wire-fixture', protocol: 'responses_http', context: 'all', service_tier: 'unspecified', group_rate_applies: true }] })
    await wrapper.get('[data-testid="manual-add"]').trigger('click')
    expect(wrapper.findAll('[data-testid="manual-entry"]')).toHaveLength(2)
    expect(() => build(wrapper)).toThrow()
  })

  it('revokes with the observed revision, without requiring new evidence', async () => {
    const current = { upstream_billing_manual_applicability: { status: 'expired', revision: 'revision-1', source_version: 'old' } } as Account['extra']
    const wrapper = mount(UpstreamBillingManualApplicabilityForm, { props: { account: account(current), show: true } })
    expect(build(wrapper)).toBeUndefined()
    await wrapper.get('[data-testid="manual-revoke"]').setValue(true)
    expect(build(wrapper)).toEqual({ revoke: true, expected_revision: 'revision-1' })
    await wrapper.setProps({ account: { ...account(), id: 43 } })
    expect(build(wrapper)).toBeUndefined()
  })

  it('keeps the opened revision when a background refresh changes the account', async () => {
    const initial = { upstream_billing_manual_applicability: { status: 'active', revision: 'revision-1', source_version: 'review-1' } } as Account['extra']
    const wrapper = mount(UpstreamBillingManualApplicabilityForm, { props: { account: account(initial), show: true } })
    await wrapper.get('[data-testid="manual-revoke"]').setValue(true)
    const refreshed = { upstream_billing_manual_applicability: { status: 'active', revision: 'revision-2', source_version: 'review-2' } } as Account['extra']
    await wrapper.setProps({ account: account(refreshed) })
    expect(build(wrapper)).toEqual({ revoke: true, expected_revision: 'revision-1' })
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await wrapper.get('[data-testid="manual-revoke"]').setValue(true)
    expect(build(wrapper)).toEqual({ revoke: true, expected_revision: 'revision-2' })
  })
})
