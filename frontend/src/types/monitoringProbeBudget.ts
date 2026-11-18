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
}

export interface MonitoringProbeBudgetDeferral {
  reason: string
  scope: string
  retry_at: string
}
