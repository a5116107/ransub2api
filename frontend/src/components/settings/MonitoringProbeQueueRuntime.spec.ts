import { enableAutoUnmount, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { afterEach, expect, it } from 'vitest'
import MonitoringProbeQueueRuntime from './MonitoringProbeQueueRuntime.vue'
import MonitoringProbeBudgetNotice from '@/components/admin/account/MonitoringProbeBudgetNotice.vue'
import type { MonitoringProbeGroupQueues, MonitoringProbeQueueRuntime as QueueRuntime } from '@/types/monitoringProbeBudget'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

enableAutoUnmount(afterEach)

function runtime(): QueueRuntime {
  return { version: 1, scope: 'responding_process', instance_id: '7ce0d565-05bf-469f-9ca5-6408bcc38545', state: 'budget_paused',
    observed_at: '2026-11-23T12:02:00Z', last_scan: { observed_at: '2026-11-23T12:00:00Z', targets: 10, due: 8 },
    cycle: { started_at: '2026-11-23T12:00:00Z', finished_at: '2026-11-23T12:01:00Z', queued: 4, active: 0, finished: 4, deferred: 2,
      oldest_pending_at: '2026-11-23T11:00:00Z', budget_deferral: { reason: 'token_limit', scope: 'global_day', retry_at: '2026-11-24T00:00:00Z' } } }
}

function open(queues?: MonitoringProbeGroupQueues, locale = 'en') {
  return mount(MonitoringProbeQueueRuntime, { props: { queues }, global: { plugins: [createI18n({ legacy: false, locale, messages: { en, zh } })] } })
}

it.each(['en', 'zh'])('shows due review classes without turning them into health verdicts in %s', locale => {
  const value = runtime()
  Object.assign(value.last_scan!, { priority_version: 1, review_targets: 3, routine_targets: 5 })
  const wrapper = open({ v2: value, v3: value }, locale)
  for (const mode of ['v2', 'v3']) {
    expect(wrapper.get(`[data-testid="group-queue-priority-${mode}"]`).text()).toBe(locale === 'zh'
      ? '本次待检：必要复核 3 · 常规轮转 5' : 'Due in this scan: 3 required reviews · 5 routine targets')
  }
  expect(wrapper.text()).toContain(locale === 'zh' ? '不是健康或质量结论' : 'not health or quality verdicts')
  expect(wrapper.text()).toContain(locale === 'zh' ? '旧轮未服务目标优先' : 'Unserved older rounds come first')
})

it.each([
  ['legacy', {}], ['future', { priority_version: 2, review_targets: 3, routine_targets: 5 }],
  ['negative', { priority_version: 1, review_targets: -1, routine_targets: 9 }],
  ['missing', { priority_version: 1, review_targets: 3 }],
  ['wrong denominator', { priority_version: 1, review_targets: 3, routine_targets: 7 }],
  ['fractional', { priority_version: 1, review_targets: .5, routine_targets: 7.5 }],
  ['unsafe', { priority_version: 1, review_targets: Number.MAX_SAFE_INTEGER, routine_targets: 1 }],
] as const)('keeps %s priority classes unknown while retaining valid queue progress', (_name, fields) => {
  const value = runtime()
  Object.assign(value.last_scan!, fields)
  const wrapper = open({ v2: value })
  expect(wrapper.find('[data-testid="group-queue-priority-v2"]').exists()).toBe(false)
  expect(wrapper.get('[data-testid="group-queue-priority-unknown-v2"]').text()).toContain('not zero required reviews')
  expect(wrapper.find('[data-testid="group-queue-cycle-v2"]').exists()).toBe(true)
})

it('retains an explicit zero due inventory and classification', () => {
  const value = runtime()
  value.state = 'idle'
  Object.assign(value.last_scan!, { targets: 0, due: 0, priority_version: 1, review_targets: 0, routine_targets: 0 })
  Object.assign(value.cycle!, { queued: 0, active: 0, finished: 0, deferred: 0, oldest_pending_at: null, budget_deferral: null })
  expect(open({ v2: value }).get('[data-testid="group-queue-priority-v2"]').text()).toContain('0 required reviews · 0 routine targets')
})

it.each(['en', 'zh'])('separates queue progress from quality and physical usage in %s', locale => {
  const paused = runtime()
  const idle = runtime()
  idle.instance_id = '7ce0d565-05bf-469f-9ca5-6408bcc38546'
  idle.state = 'idle'
  Object.assign(idle.cycle!, { queued: 0, active: 0, finished: 8, deferred: 0, oldest_pending_at: null, budget_deferral: null })
  const wrapper = open({ v2: paused, v3: idle }, locale)
  expect(wrapper.get('[data-testid="group-queue-state-v2"]').text()).toContain(locale === 'zh' ? '预算暂停' : 'Budget paused')
  expect(wrapper.get('[data-testid="group-queue-state-v3"]').text()).toContain(locale === 'zh' ? '本轮已结束' : 'Cycle finished')
  expect(wrapper.get('[data-testid="group-queue-cycle-v2"]').findAll('dd').map(n => n.text())).toEqual(['4', '0', '4', '2'])
  expect(wrapper.get('[data-testid="group-queue-cycle-v3"]').findAll('dd').map(n => n.text())).toEqual(['0', '0', '8', '0'])
  expect(wrapper.get('[data-testid="group-queue-oldest-v2"] time').attributes('datetime')).toBe(paused.cycle!.oldest_pending_at)
  expect(wrapper.get('[data-testid="group-queue-oldest-v3"]').text()).toContain(locale === 'zh' ? '无待轮转' : 'No waiting')
  expect(wrapper.text()).toContain(locale === 'zh' ? '退出不代表检测通过' : 'Exiting is not passing')
  expect(wrapper.text()).toContain(locale === 'zh' ? '不是账号数或物理请求数' : 'not account or physical-request counts')
  expect(wrapper.text()).toContain(locale === 'zh' ? '不代表' : 'not')
})

it.each(['en', 'zh'])('shows a global budget stop while admitted workers still drain in %s', async locale => {
  const value = runtime()
  value.state = 'running'
  Object.assign(value.cycle!, { finished_at: null, queued: 4, active: 4, finished: 0, deferred: 0 })
  const wrapper = open({ v2: value }, locale)
  const panel = () => wrapper.get('[data-group-probe-queue="v2"]')
  expect(panel().get('[data-testid="group-queue-cycle-v2"]').findAll('dd').map(n => n.text())).toEqual(['4', '4', '0', '0'])
  expect(panel().findComponent(MonitoringProbeBudgetNotice).exists()).toBe(true)
  expect(panel().text()).not.toContain(locale === 'zh' ? '本轮数量未知' : 'Cycle counts unknown')
  const completed = runtime()
  Object.assign(completed.cycle!, { queued: 4, active: 0, finished: 4, deferred: 1 })
  await wrapper.setProps({ queues: { v2: completed } })
  expect(panel().get('[data-testid="group-queue-cycle-v2"]').findAll('dd').map(n => n.text())).toEqual(['4', '0', '4', '1'])
  expect(panel().findComponent(MonitoringProbeBudgetNotice).exists()).toBe(true)
  const recovered = runtime()
  recovered.state = 'idle'
  Object.assign(recovered.cycle!, { queued: 0, active: 0, finished: 8, deferred: 0, oldest_pending_at: null, budget_deferral: null })
  await wrapper.setProps({ queues: { v2: recovered } })
  expect(panel().findComponent(MonitoringProbeBudgetNotice).exists()).toBe(false)
  expect(panel().get('[data-testid="group-queue-state-v2"]').text()).toContain(locale === 'zh' ? '本轮已结束' : 'Cycle finished')
})

it.each(['key', 'key_minute', 'site', 'site_minute', 'plugin'])('does not invent an early global pause from a %s deferral', scope => {
  const value = runtime()
  value.state = 'running'
  Object.assign(value.cycle!, { finished_at: null, queued: 4, active: 4, finished: 0, deferred: 0 })
  value.cycle!.budget_deferral!.scope = scope
  expect(open({ v2: value }).find('[data-testid="group-queue-cycle-v2"]').exists()).toBe(false)
})

it.each(['unavailable', 'unobserved', 'scanning', 'queued', 'running', 'idle', 'budget_paused', 'deferred', 'canceled', 'inactive', 'not_due', 'inventory_unavailable', 'coordination_unavailable', 'claim_changed'])('localizes the %s observation without treating it as a health verdict', state => {
  const value = runtime()
  value.state = state
  value.last_scan = null
  value.cycle = null
  const panel = open({ v2: value }).get('[data-group-probe-queue="v2"]')
  expect(panel.text()).not.toContain('groupQueues.states.')
  expect(panel.text()).toContain('not zero targets')
  expect(panel.find('[data-testid="group-queue-cycle-v2"]').exists()).toBe(false)
})

it.each([
  ['future version', (v: QueueRuntime) => { v.version = 2 }],
  ['fleet scope', (v: QueueRuntime) => { v.scope = 'fleet' }],
  ['unknown state', (v: QueueRuntime) => { v.state = 'PRIVATE_STATE' }],
  ['private instance', (v: QueueRuntime) => { v.instance_id = 'PRIVATE_INSTANCE' }],
  ['invalid read time', (v: QueueRuntime) => { v.observed_at = 'bad' }],
  ['unobserved with counts', (v: QueueRuntime) => { v.state = 'unobserved' }],
  ['unavailable with counts', (v: QueueRuntime) => { v.state = 'unavailable' }],
  ['future scan', (v: QueueRuntime) => { v.last_scan!.observed_at = '2026-11-25T00:00:00Z' }],
  ['negative targets', (v: QueueRuntime) => { v.last_scan!.targets = -1 }],
  ['due exceeds targets', (v: QueueRuntime) => { v.last_scan!.due = 11 }],
  ['mismatched cycle', (v: QueueRuntime) => { v.cycle!.started_at = '2026-11-23T11:59:00Z' }],
  ['inconsistent count', (v: QueueRuntime) => { v.cycle!.queued = 5 }],
  ['fractional count', (v: QueueRuntime) => { v.cycle!.deferred = .5 }],
  ['unsafe count', (v: QueueRuntime) => { v.cycle!.queued = Number.MAX_SAFE_INTEGER + 1 }],
  ['excess active workers', (v: QueueRuntime) => { v.cycle!.active = 5; v.cycle!.queued = -1 }],
  ['deferred exceeds exits', (v: QueueRuntime) => { v.cycle!.deferred = 5 }],
  ['unfinished idle', (v: QueueRuntime) => { v.state = 'idle'; v.cycle!.finished_at = null }],
  ['already finished running', (v: QueueRuntime) => { v.state = 'running' }],
  ['finish before start', (v: QueueRuntime) => { v.cycle!.finished_at = '2026-11-23T11:59:00Z' }],
  ['finish after read', (v: QueueRuntime) => { v.cycle!.finished_at = '2026-11-23T12:03:00Z' }],
  ['wait after scan', (v: QueueRuntime) => { v.cycle!.oldest_pending_at = '2026-11-23T12:01:00Z' }],
  ['missing oldest wait', (v: QueueRuntime) => { v.cycle!.oldest_pending_at = null }],
  ['missing deferral', (v: QueueRuntime) => { v.cycle!.budget_deferral = null }],
  ['deferral without exits', (v: QueueRuntime) => { v.cycle!.deferred = 0 }],
] as const)('keeps %s data unknown without leaking its values', (_name, corrupt) => {
  const value = runtime()
  corrupt(value)
  const panel = open({ v2: value }).get('[data-group-probe-queue="v2"]')
  expect(panel.find('[data-testid="group-queue-cycle-v2"]').exists()).toBe(false)
  expect(panel.text()).toContain('unknown')
  expect(panel.text()).not.toContain('PRIVATE_')
})

it('does not renew scan or waiting times on refresh, and drops the previous instance on rebuild', async () => {
  const value = runtime()
  const wrapper = open({ v2: value })
  const original = wrapper.get('[data-testid="group-queue-oldest-v2"] time').attributes('datetime')
  const later = structuredClone(value)
  later.observed_at = '2026-11-23T12:10:00Z'
  await wrapper.setProps({ queues: { v2: later } })
  expect(wrapper.get('[data-testid="group-queue-oldest-v2"] time').attributes('datetime')).toBe(original)
  expect(wrapper.get('[data-group-probe-queue="v2"]').findAll('time').some(n => n.attributes('datetime') === value.last_scan!.observed_at)).toBe(true)
  later.instance_id = '7ce0d565-05bf-469f-9ca5-6408bcc38547'
  later.state = 'unobserved'
  later.last_scan = null
  later.cycle = null
  await wrapper.setProps({ queues: { v2: later } })
  expect(wrapper.find('[data-testid="group-queue-cycle-v2"]').exists()).toBe(false)
})

it('sanitizes unknown deferral reasons and scopes through the existing notice', () => {
  const value = runtime()
  value.cycle!.budget_deferral = { reason: 'PRIVATE_REASON', scope: 'PRIVATE_SCOPE', retry_at: 'PRIVATE_RETRY' }
  const wrapper = open({ v2: value })
  expect(wrapper.find('[data-testid="group-queue-cycle-v2"]').exists()).toBe(true)
  expect(wrapper.text()).not.toContain('PRIVATE_')
})


it.each(['en', 'zh'])('shows shared state failure, draining and recovery without a health verdict in %s', async locale => {
  const value = runtime()
  value.state = 'coordination_unavailable'
  Object.assign(value.cycle!, { finished_at: null, queued: 4, active: 4, finished: 0, deferred: 0 })
  const wrapper = open({ v2: value, v3: value }, locale)
  for (const mode of ['v2', 'v3']) {
    expect(wrapper.get(`[data-testid="group-queue-state-${mode}"]`).text()).toContain(locale === 'zh' ? '共享排队状态异常，已停止新检测' : 'Shared queue unavailable; new checks stopped')
    expect(wrapper.get(`[data-testid="group-queue-cycle-${mode}"]`).findAll('dd').map(n => n.text())).toEqual(['4', '4', '0', '0'])
  }
  const recovered = runtime()
  recovered.state = 'idle'
  Object.assign(recovered.cycle!, { queued: 0, active: 0, finished: 8, deferred: 0, oldest_pending_at: null, budget_deferral: null })
  await wrapper.setProps({ queues: { v2: recovered, v3: recovered } })
  expect(wrapper.text()).not.toContain(locale === 'zh' ? '共享排队状态异常' : 'Shared queue unavailable')
  expect(wrapper.text()).toContain(locale === 'zh' ? '退出不代表检测通过' : 'Exiting is not passing')
})
