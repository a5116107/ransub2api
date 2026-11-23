import type { PelicanTestConfig } from '@/types'

export interface MonitoringProbePlanDraft {
  plan_id: number
  account_id: number
  model_id: string
  cron_expression: string
  enabled: boolean
  auto_recover: boolean
  max_results: number
  pelican_config?: PelicanTestConfig
}

export interface MonitoringProbeBudgetSettings {
  enabled: boolean
  revision: number
  requests_per_minute: number
  requests_per_day: number
  site_requests_per_minute: number
  key_requests_per_minute: number
  tokens_per_minute: number
  tokens_per_day: number
  site_tokens_per_minute: number
  key_tokens_per_minute: number
  concurrency: number
  site_concurrency: number
  key_concurrency: number
  recovery_reserve_percent: number
}

export interface MonitoringProbeBudgetSummary {
  coverage?: MonitoringProbeCoverage | null
  observed_at: string
  settings: MonitoringProbeBudgetSettings
  minute_used: number
  minute_regular: number
  day_used: number
  day_regular: number
  active_requests: number
  active_regular: number
  minute_tokens: number
  day_tokens: number
  usage_reported: number
  usage_unknown: number
  usage_pending: number
}

export interface MonitoringProbeCoverage {
  plan_change?: {
    version: number
    plan_id: number
    account_id: number
    enabled: boolean
    current_minimum_requests_per_day: number
    delta_minimum_requests_per_day: number
    proposed_minimum_requests_per_day: number
  }
  observed_at: string
  budget_revision: number
  billing_enabled: boolean
  interval_minutes: number
  accounts: number
  new_api_accounts: number
  unresolved_accounts: number
  inventory_complete: boolean
  minimum_requests_per_sweep: number
  minimum_requests_per_day: number
  minimum_sweep_batches: number
  runner_accounts_per_minute: number
  regular_minute_capacity: number
  regular_day_capacity: number
  enforced: boolean
  can_save: boolean
  blocking_reasons: string[]
  warnings: string[]
  schedule_window_start?: string
  schedule_window_end?: string
  account_plans?: number
  group_plans?: number
  unresolved_plans?: number
  plan_inventory_complete?: boolean
  scheduled_runs?: number
  scheduled_samples?: number
  minimum_judge_requests?: number
  minimum_scheduled_requests?: number
  candy_probes?: number
  unresolved_candy_probes?: number
  candy_inventory_complete?: boolean
  minimum_candy_requests?: number
  active_monitors?: number
  active_quota_monitors?: number
  unresolved_active_monitors?: number
  active_inventory_complete?: boolean
  active_monitor_runs?: number
  minimum_active_requests?: number
  minimum_total_requests_per_day?: number
  unestimated_demand_sources?: string[]
  cn_quota_cycle?: CNMonitoringRuntime | null
  ollama_usage?: OllamaMonitoringRuntime | null
  responses_capability?: ResponsesCapabilityMonitoringRuntime | null
  group_probe_queues?: MonitoringProbeGroupQueues | null
  billing_scopes?: MonitoringProbeCoverageBillingScopes | null
  periodic_sites?: MonitoringProbeCoveragePeriodicSites | null
  periodic_credentials?: MonitoringProbeCoveragePeriodicCredentials | null
}

export interface MonitoringProbeGroupQueues {
  v2?: MonitoringProbeQueueRuntime | null
  v3?: MonitoringProbeQueueRuntime | null
}

export interface MonitoringProbeQueueRuntime {
  version: number
  scope: string
  instance_id: string
  state: string
  observed_at: string
  last_scan: { observed_at: string; targets: number; due: number } | null
  cycle: {
    started_at: string
    finished_at: string | null
    queued: number
    active: number
    finished: number
    deferred: number
    oldest_pending_at: string | null
    budget_deferral: MonitoringProbeBudgetDeferral | null
  } | null
}

export interface MonitoringProbeCoveragePeriodicCredentials {
  version: number
  scope: string
  credential_groups: number
  new_api_authorization_groups: number
  billing_attributions_per_day: number
  active_model_attributions_per_day: number
  origin_ping_attributions_per_day: number
  max_attributions_per_day: number
  key_day_capacity: number
  over_limit_credential_groups: number
  unresolved_billing_targets: number
  unresolved_active_targets: number
}

export interface MonitoringProbeCoveragePeriodicSites {
  version: number
  scope: string
  sites: number
  billing_requests_per_day: number
  active_model_requests_per_day: number
  origin_ping_requests_per_day: number
  max_requests_per_day: number
  site_day_capacity: number
  over_limit_sites: number
  unresolved_billing_targets: number
  unresolved_active_targets: number
}

export interface MonitoringProbeCoverageBillingScopes {
  version: number
  sites: number
  credential_groups: number
  new_api_authorization_groups: number
  max_site_requests_per_sweep: number
  max_credential_requests_per_sweep: number
  site_sweep_capacity: number
  credential_sweep_capacity: number
  over_limit_sites: number
  over_limit_credential_groups: number
  unresolved_sites: number
  unresolved_credentials: number
  unresolved_new_api_authorizations: number
}

export interface ResponsesCapabilityMonitoringRuntime {
  version: number
  scope: string
  instance_id: string
  state: string
  observed_at: string
  started_events: number
  finished_events: number
  active_events: number
  lifecycle_timeout_seconds: number
  http_timeout_seconds: number
  last_started_at: string | null
  last_finished_at: string | null
}

export interface CNMonitoringRuntime {
  version: number
  scope: string
  instance_id: string
  state: string
  interval_seconds: number | null
  observed_at: string
  last_scan: {
    observed_at: string
    complete: boolean
    targets: number
    balance_targets: number
    quota_targets: number
  } | null
}

export interface MonitoringProbeBudgetDeferral {
  reason: string
  scope: string
  retry_at: string
}

export interface OllamaMonitoringRuntime {
  version: number
  scope: string
  instance_id: string
  state: string
  periodic_state: string
  observed_at: string
  scan_interval_seconds: number
  scan_limit: number
  pending_rate_limit_probes: number
  active_rate_limit_probes: number
  active_refreshes: number
  last_scan: {
    observed_at: string
    state: string
    enabled: boolean | null
    interval_minutes: number | null
    debounce_minutes: number | null
    candidates: number | null
    due_groups: number | null
    limit_reached: boolean | null
  } | null
}
