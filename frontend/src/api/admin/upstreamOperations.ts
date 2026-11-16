import { apiClient } from '../client'
import type { Section, Quality, ErrorLogs } from './channelOperations'
export interface OperationsQuery { from: string; until: string; upstream: string; account_id: number; group_id: number; model: string; model_scope: 'public'|'recorded'|'upstream'; dimension: 'upstream'|'account'|'model'; sort: string; direction: 'asc'|'desc'; page: number; page_size: number }
export interface UpstreamMetrics { requests: number; attributed_requests: number; unknown_upstream_requests: number; samples: number; unknown_samples: number; p50_ms: number|null; p90_ms: number|null; p95_ms: number|null; duration_p90_ms: number|null; latest_config_updated_at: string|null; oldest_config_updated_at: string|null }
export interface OperationsRow extends UpstreamMetrics { key: string; name: string }
export interface AccountBasis { id: number; name: string; key: string; attribution: string; updated_at: string; deleted: boolean }
export interface UpstreamRawCacheMetrics {
 requests: number; observed_requests: number; unknown_requests: number; unattributed_requests: number
 hit_requests: number; input_tokens: number; read_tokens: number
 coverage: number | null; request_hit_rate: number | null; token_read_rate: number | null; latest_sample_at: string | null
}
export interface UpstreamRawCacheRow extends UpstreamRawCacheMetrics {
 account_id: number; credential_fingerprint: string | null; route_fingerprint: string | null
 public_model: string; upstream_model: string | null; service_tier: string | null; reasoning_effort: string | null
 protocol: string | null; transport: string | null; stream: boolean; upstream_group_status: 'not_observed'
 waits?: {
  version: number; source: string
  first_byte: UpstreamWaitMetric; first_event: UpstreamWaitMetric
  first_reasoning: UpstreamWaitMetric; first_text: UpstreamWaitMetric
 }
}
export interface UpstreamWaitMetric { samples: number; unknown_samples: number; mean_ms: number | null; coverage: number | null; latest_sample_at: string | null }
export interface UpstreamRawCache {
 source: string; scope: string; retention_days: number; summary: UpstreamRawCacheMetrics
 total: number; rows: UpstreamRawCacheRow[]
}
export interface UpstreamAttemptMetrics {
 attempts: number; logical_requests: number; succeeded: number; failed: number; interrupted: number; unknown: number
 unattributed_attempts: number; outcome_coverage: number | null; confirmed_success_rate: number | null
 attempts_per_request: number | null; latest_sample_at: string | null
}
export interface UpstreamAttemptRow extends UpstreamAttemptMetrics {
 account_id: number; credential_fingerprint: string | null; route_fingerprint: string | null
 transport: string; protocol: string | null; public_model: string; stream: boolean
 upstream_model_status: 'observed' | 'not_observed'; upstream_group_status: 'not_observed'
 upstream_model?: string | null; upstream_protocol?: string | null; service_tier?: string | null
 reasoning_effort?: string | null; upstream_stream?: boolean | null
}
export interface UpstreamAttempts {
 version: number; source: string; scope: string; retention_days: number
 summary: UpstreamAttemptMetrics; capture: { traces: number; complete_traces: number; unknown_traces: number; coverage: number | null }
 request_scope?: { attempts: number; observed_attempts: number; unknown_attempts: number; coverage: number | null }
 total: number; rows: UpstreamAttemptRow[]
}
export interface OperationsSnapshot { scope: OperationsQuery; queried_at: string; attribution: string; usage: { summary: UpstreamMetrics; rows: OperationsRow[]; total: number; accounts: AccountBasis[]; members: AccountBasis[]; member_total: number; capacity: {accounts: number; schedulable: number; configured_concurrency: number}; catalog: Array<{key: string; name: string; accounts: number}>; catalog_total: number; account_details_limit: number }; attempts?: Section<UpstreamAttempts>; raw_cache?: Section<UpstreamRawCache>; quality: Section<Quality & { total: number }>; errors: Section<ErrorLogs> }
export async function readOperations(params: OperationsQuery, signal?: AbortSignal): Promise<OperationsSnapshot> { return (await apiClient.get<OperationsSnapshot>('/admin/upstream-operations/snapshot',{params,signal})).data }
