import { beforeEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../../client'
import { listPelicanHistory } from '../scheduledTests'
import { createQualityPreset, listQualityPresets, updateQualityPreset } from '../accountQuality'
import { readProcurementSharedMonitoring, previewProcurementSharedMonitoring, applyProcurementSharedMonitoring } from '../procurementCatalog'

vi.mock('../../client', () => ({ apiClient: { get: vi.fn(), post: vi.fn(), put: vi.fn() } }))

beforeEach(() => {
  vi.mocked(apiClient.get).mockReset().mockResolvedValue({ data: { items: [], next_cursor: 0 } })
  vi.mocked(apiClient.post).mockReset(); vi.mocked(apiClient.put).mockReset()
})

describe('reusable quality templates API', () => {
  it('persists and reloads configuration through the pure preset endpoint, never the distributing group-rule endpoint', async () => {
    const body = { name: 'Saved probe', model_id: 'test-model', cron_expression: '*/30 * * * *', enabled: true, max_results: 100,
      pelican_config: { question_kind: 'state_probe' as const, prompt: '', reasoning_effort: 'high', parallel_count: 1,
        quality: { action: 'observe_only' as const, expected_answer: '', remove_group_ids: [], auto_restore: false } } }
    const persisted = { ...body, id: 17, created_at: '', updated_at: '' }
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: persisted })
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: [persisted] })
    expect(await createQualityPreset(body)).toEqual(persisted)
    expect(await listQualityPresets()).toEqual([persisted])
    expect(apiClient.post).toHaveBeenCalledTimes(1)
    expect(apiClient.post).toHaveBeenCalledWith('/admin/account-quality-presets', body)
    expect(apiClient.get).toHaveBeenCalledTimes(1)
    expect(apiClient.get).toHaveBeenCalledWith('/admin/account-quality-presets', undefined)
    vi.mocked(apiClient.put).mockResolvedValueOnce({ data: persisted })
    expect(await updateQualityPreset(17, body)).toEqual(persisted)
    expect(apiClient.put).toHaveBeenCalledWith('/admin/account-quality-presets/17', body)
  })
})

describe('Pelican history API', () => {
  it('asks for one account only when the records window names it', async () => {
    await listPelicanHistory(0, 42)
    expect(apiClient.get).toHaveBeenLastCalledWith('/admin/pelican-test-results', { params: { before_id: 0, account_id: 42 } })
    await listPelicanHistory(9)
    expect(apiClient.get).toHaveBeenLastCalledWith('/admin/pelican-test-results', { params: { before_id: 9 } })
  })
})

describe('shared observation API', () => {
  it('reads saved participation and posts explicit false release consent through the original admin endpoints', async () => {
    const signal = new AbortController().signal, products = [{ product_id: 2, preset_id: 3 }]
    vi.mocked(apiClient.post).mockResolvedValue({ data: {} })
    await readProcurementSharedMonitoring(signal)
    expect(apiClient.get).toHaveBeenCalledWith('/admin/upstream-procurement/monitoring', { signal })
    await previewProcurementSharedMonitoring(products, signal)
    expect(apiClient.post).toHaveBeenLastCalledWith('/admin/upstream-procurement/monitoring-preview', { version: 2, products, release_ownership: false }, { signal })
    const command = { version: 2 as const, products, release_ownership: false, revision: 0, observed_at: '2026-11-21T08:00:00Z', fingerprint: 'a'.repeat(64), confirm: true as const }
    await applyProcurementSharedMonitoring(command)
    expect(apiClient.post).toHaveBeenLastCalledWith('/admin/upstream-procurement/monitoring', command)
    await previewProcurementSharedMonitoring([], signal, true)
    expect(apiClient.post).toHaveBeenLastCalledWith('/admin/upstream-procurement/monitoring-preview', { version: 2, products: [], release_ownership: true }, { signal })
  })
})
