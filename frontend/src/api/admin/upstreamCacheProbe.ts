import { buildApiUrl } from '@/api/client'
import { ADMIN_UI_REQUEST_HEADER } from '@/api/adminUIRequest'
import { consumeSSE } from '@/api/groupCacheTest'

export interface UpstreamCacheProbeInput {
  model: string
  intervals_ms: number[]
  prefix_blocks: number
  timeout_seconds: number
  reasoning_effort: string
  service_tier: string
}
export interface UpstreamCacheProbeRound {
  index: number
  interval_ms: number
  started_at: string
  finished_at: string | null
  status: string
  reason: string
  scope: null | { version: number; protocol: string; model: string; stream: boolean; service_tier: string; reasoning_effort: string; capability: string }
  cache: null | { status: string; source: string; scope: string; input_tokens: number | null; read_tokens: number | null; write_tokens: number | null; read_ratio: number | null; hit: boolean | null }
  output_tokens: number | null
  first_text_ms: number | null
  duration_ms: number | null
}
export interface UpstreamCacheProbeRun {
  version: 1
  account_id: number
  settings: UpstreamCacheProbeInput
  started_at: string
  finished_at: string | null
  status: string
  verdict: string
  reason: string
  source: string
  cold_baseline: string
  route_continuity: string
  upstream_group: string
  model_identity: string
  cache_domain: string
  valid_until: null
  planned_warm_rounds: number
  observed_warm_rounds: number
  request_hit_rate: number | null
  token_hit_rate: number | null
  budget_deferral?: { reason: string; scope: string; retry_at: string }
  rounds: UpstreamCacheProbeRound[]
}
const record = (value: unknown): Record<string, unknown> => value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {}
const count = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
const metric = (value: unknown) => value === null || typeof value === 'number' && Number.isFinite(value) && value >= 0
const ratio = (value: unknown) => value === null || typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1
const date = (value: unknown) => typeof value === 'string' && Number.isFinite(Date.parse(value))
const text = (value: unknown) => typeof value === 'string' && value.length <= 240
const keys = (value: Record<string, unknown>, allowed: string) => Object.keys(value).every(key => allowed.split(' ').includes(key))
export function validUpstreamCacheProbeInput(value: UpstreamCacheProbeInput): boolean {
  return typeof value.model === 'string' && value.model.trim().length > 0 && value.model.length <= 200 && !/[\r\n\0]/.test(value.model) &&
    Array.isArray(value.intervals_ms) && value.intervals_ms.length >= 1 && value.intervals_ms.length <= 5 && value.intervals_ms.every(n => count(n) && n <= 10000) &&
    count(value.prefix_blocks) && value.prefix_blocks >= 16 && value.prefix_blocks <= 128 && count(value.timeout_seconds) && value.timeout_seconds >= 10 && value.timeout_seconds <= 120 &&
    ['', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].includes(value.reasoning_effort) && ['', 'default', 'priority'].includes(value.service_tier)
}

/** Do not display an unsupported/contradictory result as a green cache grant. */
export function readUpstreamCacheProbeRun(value: unknown): UpstreamCacheProbeRun {
  const run = record(value), input = record(run.settings) as unknown as UpstreamCacheProbeInput
  const reject = () => { throw new Error('INVALID_UPSTREAM_CACHE_PROBE_RESULT') }
  if (!keys(run, 'version account_id settings started_at finished_at status verdict reason source cold_baseline route_continuity upstream_group model_identity cache_domain valid_until planned_warm_rounds observed_warm_rounds request_hit_rate token_hit_rate budget_deferral rounds') ||
    run.version !== 1 || !count(run.account_id) || run.account_id <= 0 || !validUpstreamCacheProbeInput(input) ||
    !keys(record(run.settings), 'model intervals_ms prefix_blocks timeout_seconds reasoning_effort service_tier') ||
    !date(run.started_at) || run.finished_at !== null && !date(run.finished_at) ||
    !['running', 'completed', 'unknown', 'failed', 'deferred', 'stopped'].includes(String(run.status)) ||
    !['running', 'insufficient', 'hit_observed', 'no_hit_observed'].includes(String(run.verdict)) || !text(run.reason) ||
    run.source !== 'synthetic_upstream_reported_usage' || run.upstream_group !== 'not_observed' || run.model_identity !== 'not_verified' || run.cache_domain !== 'not_verified' || run.valid_until !== null ||
    !['not_observed', 'zero_read_observed', 'cold_not_confirmed'].includes(String(run.cold_baseline)) || !['not_observed', 'same_configured_route'].includes(String(run.route_continuity)) ||
    run.planned_warm_rounds !== input.intervals_ms.length || !count(run.observed_warm_rounds) || run.observed_warm_rounds > run.planned_warm_rounds ||
    !ratio(run.request_hit_rate) || !ratio(run.token_hit_rate) || !Array.isArray(run.rounds) || run.rounds.length > input.intervals_ms.length + 1) return reject()
  if ((run.status === 'running') !== (run.finished_at === null) || run.finished_at !== null && Date.parse(String(run.finished_at)) < Date.parse(String(run.started_at))) return reject()
  let observed = 0, hits = 0, total = 0, read = 0
  for (const [index, value] of run.rounds.entries()) {
    const round = record(value), cache = record(round.cache), scope = record(round.scope)
    if (!keys(round, 'index interval_ms started_at finished_at status reason scope cache output_tokens first_text_ms duration_ms') || round.index !== index + 1 || round.interval_ms !== (index ? input.intervals_ms[index - 1] : 0) ||
      !date(round.started_at) || round.finished_at !== null && !date(round.finished_at) ||
      !['running', 'completed', 'unknown', 'failed', 'deferred', 'stopped'].includes(String(round.status)) || !text(round.reason) ||
      round.output_tokens !== null && !count(round.output_tokens) || !metric(round.first_text_ms) || !metric(round.duration_ms) ||
      Date.parse(String(round.started_at)) < Date.parse(String(run.started_at)) ||
      round.finished_at !== null && Date.parse(String(round.finished_at)) < Date.parse(String(round.started_at)) ||
      run.finished_at !== null && Date.parse(String(round.finished_at ?? round.started_at)) > Date.parse(String(run.finished_at))) return reject()
    if (round.scope !== null && (!keys(scope, 'version protocol model stream service_tier reasoning_effort capability') || scope.version !== 2 || !['responses_http', 'chat_http'].includes(String(scope.protocol)) || !text(scope.model) || !scope.model || scope.stream !== true || scope.capability !== 'text' || !text(scope.service_tier) || !text(scope.reasoning_effort))) return reject()
    if (round.cache !== null && (!keys(cache, 'status source scope input_tokens read_tokens write_tokens read_ratio hit') || !text(cache.status) || !text(cache.source) || !text(cache.scope) ||
      !metric(cache.input_tokens) || !metric(cache.read_tokens) || !metric(cache.write_tokens) || !ratio(cache.read_ratio) || cache.hit !== null && typeof cache.hit !== 'boolean')) return reject()
    if (round.status === 'completed') {
      const protocol = scope.protocol === 'responses_http' ? 'responses_terminal' : 'chat_completions'
      if (!round.finished_at || !scope.model || cache.status !== 'observed' || cache.source !== `canonical_${protocol}_usage` || cache.scope !== `final_physical_http_${protocol}_usage` ||
        !count(cache.input_tokens) || cache.input_tokens === 0 || !count(cache.read_tokens) || cache.read_tokens > cache.input_tokens ||
        cache.write_tokens !== null && (!count(cache.write_tokens) || cache.write_tokens > cache.input_tokens - cache.read_tokens) ||
        cache.hit !== (cache.read_tokens > 0) || typeof cache.read_ratio !== 'number' || Math.abs(cache.read_ratio - cache.read_tokens / cache.input_tokens) > 1e-12) return reject()
      if (index) { observed++; total += cache.input_tokens; read += cache.read_tokens; if (cache.hit) hits++ }
    }
  }
  if (run.observed_warm_rounds !== observed) return reject()
  const completedScopes = run.rounds.filter(r => record(r).status === 'completed').map(r => record(record(r).scope))
  if (completedScopes.some(scope => Object.keys(completedScopes[0] ?? {}).some(k => scope[k] !== completedScopes[0]?.[k]))) return reject()
  if (run.finished_at !== null) {
    const first = record(run.rounds[0]), cache = record(first.cache)
    const cold = first.status !== 'completed' ? 'not_observed' : cache.read_tokens === 0 ? 'zero_read_observed' : 'cold_not_confirmed'
    if (run.cold_baseline !== cold || run.route_continuity !== (completedScopes.length ? 'same_configured_route' : 'not_observed')) return reject()
  }
  if (run.budget_deferral !== undefined) {
    const budget = record(run.budget_deferral)
    if (!keys(budget, 'reason scope retry_at') || !text(budget.reason) || !text(budget.scope) || !date(budget.retry_at)) return reject()
  }
  if (run.status === 'completed') {
    if (!run.finished_at || observed !== run.planned_warm_rounds || observed !== run.observed_warm_rounds || run.rounds.length !== observed + 1 || run.rounds.some(r => record(r).status !== 'completed') ||
      run.route_continuity !== 'same_configured_route' || run.request_hit_rate === null || run.token_hit_rate === null ||
      Math.abs(Number(run.request_hit_rate) - hits / observed) > 1e-12 || Math.abs(Number(run.token_hit_rate) - read / total) > 1e-12 ||
      run.verdict !== (hits ? 'hit_observed' : 'no_hit_observed')) return reject()
  } else if (run.request_hit_rate !== null || run.token_hit_rate !== null || !['running', 'insufficient'].includes(String(run.verdict))) return reject()
  return value as UpstreamCacheProbeRun
}

export async function runUpstreamCacheProbe(accountId: number, input: UpstreamCacheProbeInput, signal: AbortSignal, changed: (run: UpstreamCacheProbeRun) => void): Promise<void> {
  if (!Number.isSafeInteger(accountId) || accountId <= 0 || !validUpstreamCacheProbeInput(input)) throw new Error('INVALID_UPSTREAM_CACHE_PROBE_INPUT')
  const token = localStorage.getItem('auth_token')
  if (!token) throw new Error('AUTH_REQUIRED')
  const response = await fetch(buildApiUrl(`/admin/accounts/${accountId}/cache-probe`), {
    method: 'POST', headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream', Authorization: `Bearer ${token}`, [ADMIN_UI_REQUEST_HEADER]: '1' },
    credentials: 'include', redirect: 'error', cache: 'no-store', body: JSON.stringify(input), signal
  })
  if (!response.ok) { await response.body?.cancel(); throw new Error('UPSTREAM_CACHE_PROBE_HTTP') }
  await consumeSSE(response, value => {
    const event = record(value)
    if (event.type === 'error') throw new Error('UPSTREAM_CACHE_PROBE_UNAVAILABLE')
    if (event.type !== 'cache_probe') throw new Error('INVALID_UPSTREAM_CACHE_PROBE_EVENT')
    const run = readUpstreamCacheProbeRun(event.data)
    if (run.account_id !== accountId || Object.keys(input).some(k => JSON.stringify(run.settings[k as keyof UpstreamCacheProbeInput]) !== JSON.stringify(input[k as keyof UpstreamCacheProbeInput]))) throw new Error('UPSTREAM_CACHE_PROBE_TARGET_CHANGED')
    changed(run)
    return run.finished_at !== null && run.status !== 'running'
  }, signal)
}
