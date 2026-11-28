<template>
  <details class="card min-w-0 p-4" data-testid="source-inventory" @toggle="opened">
    <summary class="cursor-pointer font-semibold">{{ t(p + '.title') }} <span class="ml-2 text-xs font-normal text-gray-500">{{ t(p + '.subtitle') }}</span></summary>
    <div class="mt-4 space-y-4">
      <p class="rounded-lg bg-amber-50 p-3 text-sm leading-6 text-amber-900 dark:bg-amber-900/20 dark:text-amber-200">{{ t(p + '.scope') }}</p>
      <form class="grid grid-cols-2 items-end gap-3 lg:grid-cols-6" @submit.prevent="load(1, true)">
        <label class="col-span-2 min-w-0 text-sm lg:col-span-3">{{ t(p + '.search') }}<input v-model="search" class="input mt-1 w-full" maxlength="200" :disabled="saving" data-testid="source-search" /></label>
        <label class="col-span-2 min-w-0 text-sm lg:col-span-1">{{ t(p + '.platform') }}<select v-model="filterPlatform" class="input mt-1 w-full" :disabled="saving"><option value="">{{ t(p + '.all') }}</option><option v-for="option in CONCRETE_PLATFORM_OPTIONS" :key="option.value" :value="option.value">{{ option.label }}</option></select></label>
        <button type="submit" class="btn btn-secondary" :disabled="reading || saving" data-testid="source-refresh">{{ t(p + '.read') }}</button>
        <button type="button" class="btn btn-primary" :disabled="saving" @click="toggleForm">{{ t(p + '.register') }}</button>
      </form>
      <p v-if="reading" role="status">{{ t(p + '.loading') }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ t(p + '.' + error) }}</p>
      <p v-if="saved" role="status" class="text-sm text-primary-700 dark:text-primary-300" data-testid="source-saved">{{ t(p + '.saved') }} · {{ saved.site_name }} / {{ saved.external_group_name || saved.external_group_id }} / {{ saved.account_name }}</p>
      <form v-if="showForm" class="space-y-4 rounded-lg border border-gray-200 p-4 dark:border-dark-600" data-testid="source-form" @submit.prevent="save">
        <h3 class="font-medium">{{ t(p + '.register') }}</h3>
        <div class="grid min-w-0 gap-3 sm:grid-cols-2">
          <label class="text-sm">{{ t(p + '.siteName') }}<input v-model="draft.site_name" class="input mt-1 w-full" maxlength="100" required :disabled="saving" data-testid="source-site-name" /></label>
          <label class="text-sm">{{ t(p + '.siteCode') }}<input v-model="draft.site_code" class="input mt-1 w-full" pattern="[a-z0-9][a-z0-9_-]{0,63}" maxlength="64" required :disabled="saving" data-testid="source-site-code" /><span class="mt-1 block text-xs text-gray-500">{{ t(p + '.siteCodeHelp') }}</span></label>
          <label class="text-sm">{{ t(p + '.externalName') }}<input v-model="draft.external_group_name" class="input mt-1 w-full" maxlength="100" :disabled="saving" /></label>
          <label class="text-sm">{{ t(p + '.externalID') }}<input v-model="draft.external_group_id" class="input mt-1 w-full" maxlength="256" required :disabled="saving" data-testid="source-external-id" /></label>
          <label class="text-sm">{{ t(p + '.platform') }}<select v-model="draft.platform" class="input mt-1 w-full" :disabled="saving" data-testid="source-platform"><option v-for="option in CONCRETE_PLATFORM_OPTIONS" :key="option.value" :value="option.value">{{ option.label }}</option></select></label>
          <div class="min-w-0"><label for="source-local-group" class="mb-1 block text-sm">{{ t(p + '.localGroup') }}</label>
            <Select id="source-local-group" v-model="draft.local_group_id" :options="groupOptions" :aria-label="t(p + '.localGroup')" :placeholder="t(p + '.choose')" remote searchable :loading="groupsLoading" :disabled="saving" @open="loadGroups('')" @search="loadGroups" />
          </div>
          <div class="min-w-0 sm:col-span-2"><label for="source-account" class="mb-1 block text-sm">{{ t(p + '.account') }}</label>
            <Select id="source-account" v-model="draft.account_id" :options="accountOptions" :aria-label="t(p + '.account')" :placeholder="t(p + '.choose')" remote searchable :loading="accountsLoading" :disabled="saving || !draft.local_group_id" @open="loadAccounts('')" @search="loadAccounts" />
          </div>
        </div>
        <p v-if="optionsError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ t(p + '.optionsFailed') }}</p>
        <p class="text-xs text-gray-500">{{ t(p + '.optionsScope') }}</p>
        <label class="flex items-start gap-2 text-sm"><input v-model="consent" type="checkbox" class="mt-1" :disabled="saving" data-testid="source-consent" /><span>{{ t(p + '.consent') }}</span></label>
        <div class="flex flex-wrap gap-2"><button type="submit" class="btn btn-primary" data-testid="source-save" :disabled="saving || reading || uncertain || !consent || !draft.account_id || !draft.local_group_id">{{ t(p + (saving ? '.saving' : '.save')) }}</button><button type="button" class="btn btn-secondary" :disabled="saving" @click="showForm = false">{{ t('common.cancel') }}</button></div>
      </form>
      <template v-if="snapshot && !reading && !error">
        <p class="text-xs text-gray-500">{{ t(p + '.observed') }} {{ snapshot.observed_at }} · {{ t(p + '.page', { page: snapshot.page, total: snapshot.total }) }}</p>
        <p v-if="!snapshot.items.length" class="rounded-lg border border-dashed p-5 text-center text-sm text-gray-500">{{ t(p + '.empty') }}</p>
        <div v-for="site in sites" :key="site.id" class="min-w-0 rounded-lg border border-gray-200 dark:border-dark-600">
          <h3 class="break-words border-b px-4 py-3 font-medium dark:border-dark-600">{{ site.name }} <span class="text-xs font-normal text-gray-500">{{ site.code }}</span></h3>
          <details v-for="vendor in site.vendors" :key="vendor.platform" class="min-w-0 px-4 py-3" open>
            <summary class="cursor-pointer text-sm font-medium">{{ platformLabel(vendor.platform) }}</summary>
            <div v-for="group in vendor.groups" :key="group.id" class="mt-3 min-w-0 space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-800" data-testid="source-group">
              <div><h4 class="break-words font-medium">{{ group.name || group.externalID }}</h4><p class="break-all text-xs text-gray-500">{{ t(p + '.externalID') }} {{ group.externalID }} · {{ t(p + '.localGroup') }} {{ group.localName }}</p></div>
              <article v-for="item in group.items" :key="item.id" class="min-w-0 space-y-2 rounded-lg bg-white p-3 dark:bg-dark-700" data-testid="source-row">
                <div class="flex flex-wrap items-start justify-between gap-2"><span class="break-words text-sm font-medium">{{ item.account_name || t(p + '.noAccount') }}</span><span :class="statusClass(item)" class="rounded-full px-2 py-1 text-xs font-medium" data-testid="source-conclusion">{{ t(p + '.states.' + item.conclusion) }}</span></div>
                <p class="break-words text-sm">{{ reason(item.reasons[0]) }}</p>
                <details class="text-xs text-gray-500"><summary class="cursor-pointer">{{ t(p + '.details') }}</summary><dl class="mt-2 space-y-1">
                  <div><dt class="inline">{{ t(p + '.route') }}：</dt><dd class="inline">{{ t(p + '.routes.' + item.route_identity) }}</dd></div>
                  <div><dt class="inline">{{ t(p + '.fixedGroup') }}：</dt><dd class="inline">{{ t(p + '.unverified') }}</dd></div>
                  <div v-for="code in item.reasons.slice(1)" :key="code"><dd>{{ reason(code) }}</dd></div>
                  <div><dt class="inline">{{ t(p + '.updated') }}：</dt><dd class="inline">{{ item.updated_at }}</dd></div>
                </dl></details>
                <div v-if="item.id" class="flex flex-wrap gap-3 text-xs">
                  <a href="/admin/accounts" class="text-primary-700 underline dark:text-primary-300">{{ t(p + '.openAccount') }}</a>
                  <button type="button" :disabled="saving || uncertain" class="text-primary-700 underline disabled:opacity-50 dark:text-primary-300" @click="pending = { item, kind: 'toggle' }">{{ t(p + (item.enabled ? '.pause' : '.resume')) }}</button>
                  <button type="button" :disabled="saving || uncertain" class="text-red-600 underline disabled:opacity-50 dark:text-red-400" @click="pending = { item, kind: 'remove' }">{{ t(p + '.remove') }}</button>
                </div>
              </article>
            </div>
          </details>
        </div>
        <nav class="flex flex-wrap items-center gap-3" :aria-label="t(p + '.pagination')">
          <button class="btn btn-secondary" :disabled="reading || saving || snapshot.page <= 1" data-testid="source-previous" @click="load(snapshot.page - 1)">{{ t('upstreamOps.previous') }}</button>
          <span class="text-sm">{{ t(p + '.page', { page: snapshot.page, total: snapshot.total }) }}</span>
          <button class="btn btn-secondary" :disabled="reading || saving || snapshot.page * snapshot.page_size >= snapshot.total" data-testid="source-next" @click="load(snapshot.page + 1)">{{ t('upstreamOps.next') }}</button>
        </nav>
      </template>
      <ConfirmDialog :show="!!pending" :title="t(p + '.confirmTitle')" :message="t(p + '.confirmAction')" :danger="pending?.kind === 'remove'" @confirm="mutate" @cancel="pending = null" />
    </div>
  </details>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'
import { list as listGroups } from '@/api/admin/groups'
import { list as listAccounts } from '@/api/admin/accounts'
import {
  listProcurementSources, createProcurementSource, getProcurementSource, setProcurementSourceEnabled, deleteProcurementSource,
  type ProcurementSourceMapping, type ProcurementSourcePage, type CreateProcurementSourceMapping
} from '@/api/admin/procurementSources'
import type { AccountListItem, AdminGroup, GroupPlatform } from '@/types'

const { t } = useI18n(), p = 'upstreamOps.sources'
const search = ref(''), filterPlatform = ref(''), snapshot = ref<ProcurementSourcePage | null>(null)
const reading = ref(false), saving = ref(false), showForm = ref(false), consent = ref(false), uncertain = ref(false), error = ref(''), optionsError = ref(false)
const saved = ref<ProcurementSourceMapping | null>(null)
const draft = reactive({ site_code: '', site_name: '', platform: 'openai', external_group_id: '', external_group_name: '', local_group_id: 0, account_id: 0 })
const groups = ref<AdminGroup[]>([]), accounts = ref<AccountListItem[]>([]), groupsLoading = ref(false), accountsLoading = ref(false)
const pending = ref<{ item: ProcurementSourceMapping; kind: 'remove' | 'toggle' } | null>(null)
let alive = true, hasOpened = false, generation = 0, groupsGeneration = 0, accountsGeneration = 0
let controller: AbortController | undefined, groupsController: AbortController | undefined, accountsController: AbortController | undefined
watch(() => draft.platform, () => { draft.local_group_id = 0; groups.value = []; groupsController?.abort(); groupsGeneration++ })
watch(() => draft.local_group_id, () => { draft.account_id = 0; accounts.value = []; accountsController?.abort(); accountsGeneration++ })
watch(draft, () => { consent.value = false; saved.value = null })
const groupOptions = computed(() => groups.value.map(row => ({ value: row.id, label: row.name })))
const accountOptions = computed(() => accounts.value.map(row => ({ value: row.id, label: row.name })))
const sites = computed(() => {
  type Group = { id: number; name: string; externalID: string; localName: string; items: ProcurementSourceMapping[] }
  const out: Array<{ id: number; code: string; name: string; vendors: Array<{ platform: string; groups: Group[] }> }> = []
  for (const item of snapshot.value?.items ?? []) {
    let site = out.find(row => row.id === item.site_id)
    if (!site) { site = { id: item.site_id, code: item.site_code, name: item.site_name, vendors: [] }; out.push(site) }
    let vendor = site.vendors.find(row => row.platform === item.platform)
    if (!vendor) { vendor = { platform: item.platform, groups: [] }; site.vendors.push(vendor) }
    let group = vendor.groups.find(row => row.id === item.source_group_id)
    if (!group) { group = { id: item.source_group_id, name: item.external_group_name, externalID: item.external_group_id, localName: item.local_group_name, items: [] }; vendor.groups.push(group) }
    group.items.push(item)
  }
  return out
})
const knownReasons = new Set(['fixed_group_selection_unverified', 'source_mapping_paused', 'source_account_not_mapped', 'source_account_deleted', 'local_group_deleted', 'source_platform_mismatch', 'source_account_not_in_local_group', 'source_account_inactive', 'local_group_inactive', 'source_account_route_changed', 'source_account_route_unknown'])
const reason = (code?: string) => t(p + '.reasons.' + (knownReasons.has(code ?? '') ? code : 'unknown'))
const platformLabel = (platform: string) => CONCRETE_PLATFORM_OPTIONS.find(option => option.value === platform)?.label ?? platform
const statusClass = (item: ProcurementSourceMapping) => item.conclusion === 'blocked'
  ? 'bg-red-50 text-red-800 dark:bg-red-900/20 dark:text-red-200'
  : item.conclusion === 'paused' ? 'bg-gray-100 text-gray-700 dark:bg-dark-600 dark:text-gray-200' : 'bg-amber-50 text-amber-900 dark:bg-amber-900/20 dark:text-amber-200'
const status = (e: unknown) => (e as { status?: number; response?: { status?: number } })?.status ?? (e as { response?: { status?: number } })?.response?.status
async function load(page = 1, explicit = false): Promise<boolean> {
  const seq = ++generation
  controller?.abort(); controller = new AbortController()
  reading.value = true; error.value = ''; snapshot.value = null
  try {
    const next = await listProcurementSources({ search: search.value, platform: filterPlatform.value, page, page_size: 20 }, controller.signal)
    if (!alive || seq !== generation) return false
    snapshot.value = next
    if (explicit) { uncertain.value = false; saved.value = null }
    return true
  } catch (e) { if (alive && seq === generation) error.value = status(e) === 403 ? 'forbidden' : 'failed'; return false }
  finally { if (alive && seq === generation) reading.value = false }
}
function opened(event: Event) {
  if ((event.currentTarget as HTMLDetailsElement).open && !hasOpened) { hasOpened = true; void load() }
}
function toggleForm() { showForm.value = !showForm.value; if (showForm.value && !groups.value.length) void loadGroups('') }
async function loadGroups(query: string) {
  const seq = ++groupsGeneration, platform = draft.platform
  groupsController?.abort(); groupsController = new AbortController(); groupsLoading.value = true; optionsError.value = false
  try {
    const next = await listGroups(1, 100, { platform: platform as GroupPlatform, search: query }, { signal: groupsController.signal })
    if (!next || !Array.isArray(next.items) || next.items.some(row => !Number.isSafeInteger(row.id) || row.id < 1 || row.platform !== platform || typeof row.name !== 'string')) throw new Error('Invalid groups')
    if (alive && seq === groupsGeneration) groups.value = next.items
  } catch { if (alive && seq === groupsGeneration) optionsError.value = true }
  finally { if (alive && seq === groupsGeneration) groupsLoading.value = false }
}
async function loadAccounts(query: string) {
  if (!draft.local_group_id) return
  const seq = ++accountsGeneration, platform = draft.platform, group = draft.local_group_id
  accountsController?.abort(); accountsController = new AbortController(); accountsLoading.value = true; optionsError.value = false
  try {
    const next = await listAccounts(1, 100, { platform, group: String(group), search: query, lite: 'true' }, { signal: accountsController.signal })
    if (!next || !Array.isArray(next.items) || next.items.some(row => !Number.isSafeInteger(row.id) || row.id < 1 || row.platform !== platform || typeof row.name !== 'string')) throw new Error('Invalid accounts')
    if (alive && seq === accountsGeneration) accounts.value = next.items
  } catch { if (alive && seq === accountsGeneration) optionsError.value = true }
  finally { if (alive && seq === accountsGeneration) accountsLoading.value = false }
}
async function save() {
  if (saving.value || reading.value || !consent.value || uncertain.value || !draft.account_id || !draft.local_group_id) return
  saving.value = true; saved.value = null; error.value = ''
  const command: CreateProcurementSourceMapping = { version: 1, ...draft, confirm: true }
  try {
    const written = await createProcurementSource(command)
    const read = await getProcurementSource(written.id)
    if (read.revision !== written.revision || read.site_code !== command.site_code || read.external_group_id !== command.external_group_id || read.account_id !== command.account_id || read.local_group_id !== command.local_group_id || read.platform !== command.platform) throw new Error('Saved state changed')
    if (!alive) return
    if (!await load(1)) throw new Error('Read-back failed')
    saved.value = read; consent.value = false; uncertain.value = false
  } catch (e) {
    if (alive) { uncertain.value = true; error.value = status(e) === 409 ? 'conflict' : status(e) === 403 ? 'forbidden' : 'saveUnconfirmed' }
  } finally { if (alive) saving.value = false }
}
async function mutate() {
  if (!pending.value || saving.value) return
  const action = pending.value; pending.value = null; saving.value = true; saved.value = null
  try {
    if (action.kind === 'remove') {
      await deleteProcurementSource(action.item)
      let absent = false
      try { await getProcurementSource(action.item.id) } catch (e) { if (status(e) === 404) absent = true; else throw e }
      if (!absent) throw new Error('Deletion not confirmed')
    } else {
      const written = await setProcurementSourceEnabled(action.item)
      const read = await getProcurementSource(written.id)
      if (read.revision !== written.revision || read.enabled !== written.enabled) throw new Error('Saved state changed')
    }
    if (alive && !await load(snapshot.value?.page ?? 1)) throw new Error('Read-back failed')
  } catch (e) { if (alive) { snapshot.value = null; uncertain.value = true; error.value = status(e) === 409 ? 'conflict' : 'saveUnconfirmed' } }
  finally { if (alive) saving.value = false }
}
onBeforeUnmount(() => { alive = false; generation++; controller?.abort(); groupsGeneration++; groupsController?.abort(); accountsGeneration++; accountsController?.abort() })
</script>
