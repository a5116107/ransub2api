import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { describe, expect, it, vi } from 'vitest'
import VersionBadge from '../VersionBadge.vue'
import en from '@/i18n/locales/en'

const app = vi.hoisted(() => ({
  versionLoading: false,
  currentVersion: '2.19.2-ransub.2',
  latestVersion: '2.19.2-ransub.2',
  hasUpdate: false,
  releaseInfo: null,
  buildType: 'release',
  fetchVersion: vi.fn(),
  clearVersionCache: vi.fn()
}))

vi.mock('@/stores', () => ({ useAuthStore: () => ({ isAdmin: true }), useAppStore: () => app }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copied: false, copyToClipboard: vi.fn() }) }))
vi.mock('@/api/admin/system', () => ({
  performUpdate: vi.fn(), restartService: vi.fn(), rollback: vi.fn(),
  getRollbackVersions: vi.fn().mockResolvedValue({ versions: [{ version: '2.19.2-ransub.1', published_at: '2026-11-12T00:00:00Z' }] })
}))

describe('custom release rollback channel', () => {
  it('renders script and Docker rollback commands from the same user repository', async () => {
    const wrapper = mount(VersionBadge, { global: { plugins: [createI18n({ legacy: false, locale: 'en', messages: { en } })] } })
    await wrapper.get('button').trigger('click')
    const rollback = wrapper.findAll('button').find(button => button.text().includes(en.version.rollback))
    expect(rollback).toBeDefined()
    await rollback!.trigger('click')
    await flushPromises()
    const version = wrapper.findAll('button').find(button => button.text().includes('2.19.2-ransub.1'))
    expect(version).toBeDefined()
    await version!.trigger('click')
    expect(wrapper.text()).toContain('raw.githubusercontent.com/a5116107/ransub2api/v2.19.2-ransub.1/deploy/install.sh')
    const docker = wrapper.findAll('button').find(button => button.text().includes(en.version.deployDocker))
    expect(docker).toBeDefined()
    await docker!.trigger('click')
    expect(wrapper.text()).toContain('ghcr.io/a5116107/ransub2api:2.19.2-ransub.1')
    expect(wrapper.text()).not.toContain('weishaw/sub2api')
    wrapper.unmount()
  })
})
