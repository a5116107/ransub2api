import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post, put, del } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  put: vi.fn(),
  del: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { get, post, put, delete: del }
}))

import {
  getNewAPIUpstreamConfig,
  previewNewAPIUpstreamConfig,
  saveNewAPIUpstreamConfig,
  deleteNewAPIUpstreamConfig,
  getUpstreamBillingProbeSettings,
  getMonitoringProbeBudget,
  updateMonitoringProbeBudget,
  previewMonitoringProbeCoverage,
  probeUpstreamBilling,
  probeUpstreamBillingBatch,
  setUpstreamBillingProbeEnabled,
  updateUpstreamBillingProbeSettings
} from '@/api/admin/accounts'

describe('admin account upstream billing probe API', () => {
  it('uses the shared budget endpoints and preserves the CAS revision', async () => {
    const policy = { enabled: true, revision: 3, requests_per_minute: 90, requests_per_day: 1000, site_requests_per_minute: 50, key_requests_per_minute: 20, concurrency: 4, site_concurrency: 2, key_concurrency: 1, recovery_reserve_percent: 20 }
    const summary = { settings: policy, minute_used: 10 }
    get.mockResolvedValueOnce({ data: summary })
    put.mockResolvedValueOnce({ data: { ...policy, revision: 4 } })
    await expect(getMonitoringProbeBudget()).resolves.toEqual(summary)
    await expect(updateMonitoringProbeBudget(policy)).resolves.toEqual({ ...policy, revision: 4 })
    expect(get).toHaveBeenCalledWith('/admin/accounts/upstream-billing-probe/budget', undefined)
    expect(put).toHaveBeenCalledWith('/admin/accounts/upstream-billing-probe/budget', policy)
  })
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
    put.mockReset()
    del.mockReset()
  })

  it('previews a draft without writing or sending probes', async () => {
    const policy = { enabled: true, revision: 3 } as Parameters<typeof previewMonitoringProbeCoverage>[0]
    const result = { can_save: false, blocking_reasons: ['day_capacity'] }
    post.mockResolvedValueOnce({ data: result })
    await expect(previewMonitoringProbeCoverage(policy)).resolves.toEqual(result)
    expect(post).toHaveBeenCalledWith('/admin/accounts/upstream-billing-probe/budget/preview', policy, undefined)
    expect(put).not.toHaveBeenCalled()
  })

  it('forwards optional cancellation and the exact plan draft without writing', async () => {
    const signal = new AbortController().signal
    const policy = { enabled: true, revision: 3, plan_draft: {
      plan_id: 12, account_id: 7, model_id: 'model-a', cron_expression: '0 * * * *', enabled: false, auto_recover: false, max_results: 100
    } } as Parameters<typeof previewMonitoringProbeCoverage>[0]
    get.mockResolvedValueOnce({ data: { settings: policy } })
    post.mockResolvedValueOnce({ data: { can_save: true } })
    await getMonitoringProbeBudget({ signal })
    await previewMonitoringProbeCoverage(policy, { signal })
    expect(get).toHaveBeenCalledWith('/admin/accounts/upstream-billing-probe/budget', { signal })
    expect(post).toHaveBeenCalledWith('/admin/accounts/upstream-billing-probe/budget/preview', policy, { signal })
    expect(put).not.toHaveBeenCalled()
    expect(del).not.toHaveBeenCalled()
    expect(post).toHaveBeenCalledTimes(1)
  })

  it('uses the config endpoints and preserves explicit account and token selections', async () => {
    const config = { account_id: 7, site_url: 'https://upstream.example', configured: false, encryption_key_configured: true, accounts: [] }
    const result = { site_url: config.site_url, user_id: 42, wallet: { amount: 12.34, unit: 'USD' }, accounts: [] }
    const payload = { user_id: 42, access_token: 'fixture-secret', account_ids: [7, 8], token_selections: { '8': 19 } }
    get.mockResolvedValueOnce({ data: config })
    post.mockResolvedValueOnce({ data: result })
    put.mockResolvedValueOnce({ data: result })
    del.mockResolvedValueOnce({ data: { account_id: 7, configured: false } })
    await expect(getNewAPIUpstreamConfig(7)).resolves.toEqual(config)
    await expect(previewNewAPIUpstreamConfig(7, payload)).resolves.toEqual(result)
    await expect(saveNewAPIUpstreamConfig(7, payload)).resolves.toEqual(result)
    await expect(deleteNewAPIUpstreamConfig(7)).resolves.toEqual({ account_id: 7, configured: false })
    expect(get).toHaveBeenCalledWith('/admin/accounts/7/upstream-billing-probe/config')
    expect(post).toHaveBeenCalledWith('/admin/accounts/7/upstream-billing-probe/config/preview', payload)
    expect(put).toHaveBeenCalledWith('/admin/accounts/7/upstream-billing-probe/config', payload)
    expect(del).toHaveBeenCalledWith('/admin/accounts/7/upstream-billing-probe/config')
  })

  it('reads and updates global settings', async () => {
    const settings = { enabled: true, interval_minutes: 30 }
    get.mockResolvedValueOnce({ data: settings })
    put.mockResolvedValueOnce({ data: settings })

    await expect(getUpstreamBillingProbeSettings()).resolves.toEqual(settings)
    await expect(updateUpstreamBillingProbeSettings(settings)).resolves.toEqual(settings)
    expect(get).toHaveBeenCalledWith('/admin/accounts/upstream-billing-probe/settings')
    expect(put).toHaveBeenCalledWith('/admin/accounts/upstream-billing-probe/settings', settings)
  })

  it('uses dedicated account and batch endpoints', async () => {
    const result = { account_id: 7, snapshot: { status: 'unsupported' } }
    put.mockResolvedValueOnce({ data: {} })
    post.mockResolvedValueOnce({ data: result })
    post.mockResolvedValueOnce({ data: { results: [result] } })

    await setUpstreamBillingProbeEnabled(7, true)
    await expect(probeUpstreamBilling(7)).resolves.toEqual(result)
    await expect(probeUpstreamBillingBatch([7])).resolves.toEqual([result])

    expect(put).toHaveBeenCalledWith('/admin/accounts/7/upstream-billing-probe', { enabled: true })
    expect(post).toHaveBeenNthCalledWith(1, '/admin/accounts/7/upstream-billing-probe')
    expect(post).toHaveBeenNthCalledWith(2, '/admin/accounts/upstream-billing-probe/batch', { account_ids: [7] })
  })
})
