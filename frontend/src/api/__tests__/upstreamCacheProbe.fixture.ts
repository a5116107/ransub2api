import type { UpstreamCacheProbeRun } from '@/api/admin/upstreamCacheProbe'

export function upstreamCacheResult(): UpstreamCacheProbeRun {
  const at = new Date().toISOString()
  return {
    version: 1, account_id: 7, settings: { model: 'public-model', intervals_ms: [1000, 3000], prefix_blocks: 48, timeout_seconds: 60, reasoning_effort: '', service_tier: '' },
    started_at: at, finished_at: at, status: 'completed', verdict: 'hit_observed', reason: 'completed_observations', source: 'synthetic_upstream_reported_usage',
    cold_baseline: 'zero_read_observed', route_continuity: 'same_configured_route', upstream_group: 'not_observed', model_identity: 'not_verified', cache_domain: 'not_verified', valid_until: null,
    planned_warm_rounds: 2, observed_warm_rounds: 2, request_hit_rate: 1, token_hit_rate: .8,
    rounds: [0, 1000, 3000].map((interval, index) => ({ index: index + 1, interval_ms: interval, started_at: at, finished_at: at, status: 'completed', reason: 'usage_observed',
      scope: { version: 2, protocol: 'responses_http', model: 'wire-model', stream: true, service_tier: 'unspecified', reasoning_effort: 'unspecified', capability: 'text' },
      cache: { status: 'observed', source: 'canonical_responses_terminal_usage', scope: 'final_physical_http_responses_terminal_usage', input_tokens: 2000, read_tokens: index ? 1600 : 0, write_tokens: null, read_ratio: index ? .8 : 0, hit: !!index },
      output_tokens: 2, first_text_ms: 120, duration_ms: 150 }))
  }
}