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

export interface MonitoringProbeBudgetDeferral {
  reason: string
  scope: string
  retry_at: string
}
