import { apiClient } from '../client'
import type { UpstreamBillingProbeSnapshot } from '@/types'
import type { Section, Quality, ErrorLogs } from './channelOperations'
export interface OperationsQuery { from: string; until: string; upstream: string; account_id: number; group_id: number; model: string; model_scope: 'public'|'recorded'|'upstream'; dimension: 'upstream'|'account'|'model'; sort: string; direction: 'asc'|'desc'; page: number; page_size: number }
export interface UpstreamMetrics { requests: number; attributed_requests: number; unknown_upstream_requests: number; samples: number; unknown_samples: number; p50_ms: number|null; p90_ms: number|null; p95_ms: number|null; duration_p90_ms: number|null; latest_config_updated_at: string|null; oldest_config_updated_at: string|null }
export interface OperationsRow extends UpstreamMetrics { key: string; name: string }
export interface AccountBasis { id: number; name: string; key: string; attribution: string; updated_at: string; deleted: boolean; status?: string; schedulable?: boolean; concurrency?: number; platform?: string; type?: string; rate_multiplier?: number; cost_multiplier?: number | null; billing_probe?: UpstreamBillingProbeSnapshot | null; billing_probe_enabled?: boolean; new_api_apply_usd_rate?: boolean | null; metrics?: UpstreamMetrics }
export interface ModelTestArtwork { id: number; account_id: number; group_id: number; model: string | null; reasoning_effort: string | null; started_at: string; latency_ms?: number }
export interface ModelTestRow extends ModelTestArtwork { plan_id: number; status: string; finished_at: string; has_output: boolean; attempts: Array<{account_id: number}>; artwork: ModelTestArtwork | null }
export interface ModelTestSnapshot { version: number; source: string; rows: ModelTestRow[]; total: number }
export interface AvailabilityProbe {
 version: number; status: string; reason: string; started_at: string; finished_at: string; attempt_count: number
 scope?: {version: number; protocol: string; model: string; stream: boolean; service_tier: string; reasoning_effort: string; capability: string}
}
export interface AvailabilityLineModel { model_id: string; status: string; reason: string; observation: AvailabilityProbe | null; decision?: AvailabilityProbe; valid_until?: string }
export interface AvailabilityLineReport { plan_id: number; account_id: number; configured: boolean; enabled: boolean; evaluated_at: string; models: AvailabilityLineModel[] }
export interface AvailabilityLineSnapshot { version: number; source: string; reports: AvailabilityLineReport[]; plan_total: number; model_total: number; models_loaded: number; truncated: boolean }
export interface OperationsSnapshot { availability?: Section<AvailabilityLineSnapshot>; model_tests?: Section<ModelTestSnapshot>; scope: OperationsQuery; queried_at: string; attribution: string; usage: { summary: UpstreamMetrics; rows: OperationsRow[]; total: number; accounts: AccountBasis[]; members: AccountBasis[]; member_total: number; capacity: {accounts: number; schedulable: number; configured_concurrency: number}; catalog: Array<{key: string; name: string; accounts: number}>; catalog_total: number; account_details_limit: number }; quality: Section<Quality & { total: number }>; errors: Section<ErrorLogs> }
export async function readOperations(params: OperationsQuery, signal?: AbortSignal): Promise<OperationsSnapshot> { return (await apiClient.get<OperationsSnapshot>('/admin/upstream-operations/snapshot',{params,signal})).data }
