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

  it('edits exact Responses output options without granting them to Chat', async () => {
    const wrapper = mount(UpstreamBillingManualApplicabilityForm, { props: { account: account(), show: true } })
    await wrapper.get('[data-testid="manual-edit"]').setValue(true)
    await wrapper.get('[data-testid="manual-version"]').setValue('output-review-1')
    await wrapper.get('[data-testid="manual-reason"]').setValue('reviewed encrypted output and summary')
    await wrapper.get('[data-testid="manual-model"]').setValue('wire-fixture')
    await wrapper.get('[data-testid="manual-summary"]').setValue('auto')
    await wrapper.get('[data-testid="manual-encrypted"]').setValue(true)
    await wrapper.get('[data-testid="manual-confirm"]').setValue(true)
    expect(build(wrapper)).toMatchObject({ entries: [{ reasoning_summary: 'auto', include_encrypted_content: true }] })
    await wrapper.get('[data-testid="manual-protocol"]').setValue('chat_http')
    expect(wrapper.find('[data-testid="manual-summary"]').exists()).toBe(false)
    const update = build(wrapper)
    expect(update && 'entries' in update && update.entries[0]).not.toHaveProperty('reasoning_summary')
    expect(update && 'entries' in update && update.entries[0]).not.toHaveProperty('include_encrypted_content')
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

  it('preserves output options across reopening and an explicit reconfirmation', async () => {
    const extra = { upstream_billing_manual_applicability: {
      revision: 'output-1', status: 'active', source_version: 'contract-output-1', reason: 'reviewed',
      expires_at: new Date(Date.now() + 3600000).toISOString(),
      entries: [{ model: 'wire-fixture', protocol: 'responses_http', service_tier: 'priority', reasoning_effort: 'high', context: 'all', group_rate_applies: true, conclusion: 'covered', reasoning_summary: 'auto', include_encrypted_content: true }]
    } } as Account['extra']
    const wrapper = mount(UpstreamBillingManualApplicabilityForm, { props: { account: account(extra), show: true } })
    expect(build(wrapper)).toBeUndefined()
    await wrapper.get('[data-testid="manual-edit"]').setValue(true)
    expect((wrapper.get('[data-testid="manual-summary"]').element as HTMLSelectElement).value).toBe('auto')
    expect((wrapper.get('[data-testid="manual-encrypted"]').element as HTMLInputElement).checked).toBe(true)
    await wrapper.get('[data-testid="manual-confirm"]').setValue(true)
    expect(build(wrapper)).toMatchObject({ expected_revision: 'output-1', entries: [{ reasoning_summary: 'auto', include_encrypted_content: true }] })
  })
})
