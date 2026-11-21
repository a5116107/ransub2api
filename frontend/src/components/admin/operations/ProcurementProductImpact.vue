<template>
  <section class="card min-w-0 space-y-4 p-4" aria-labelledby="product-impact-title" data-testid="product-impact">
    <header>
      <h2 id="product-impact-title" class="font-semibold">{{ t(`${p}.title`) }}</h2>
      <p class="mt-1 text-sm leading-6 text-gray-500">{{ t(`${p}.intro`) }}</p>
    </header>
    <form class="grid min-w-0 items-end gap-3 sm:grid-cols-[minmax(0,1fr)_auto]" @submit.prevent="preview">
      <div class="min-w-0">
        <label for="impact-product" class="mb-1 block text-sm font-medium">{{ t(`${p}.product`) }}</label>
        <Select id="impact-product" v-model="productID" :options="options" :aria-label="t(`${p}.product`)" :placeholder="t(`${p}.choose`)"
          :searchable="true" :remote="true" :loading="productsBusy" :empty-text="t(`${p}.noProducts`)" aria-describedby="impact-product-help"
          @open="openProducts" @search="loadProducts" />
      </div>
      <div class="min-w-0 sm:col-start-1">
        <label for="impact-preset" class="mb-1 block text-sm font-medium">{{ t(`${p}.recipe.label`) }}</label>
        <select id="impact-preset" v-model="presetID" class="input w-full" :disabled="presetsBusy" aria-describedby="impact-recipe-help" @focus="loadPresets">
          <option :value="null">{{ t(`${p}.recipe.none`) }}</option>
          <option v-for="preset in recipePresets" :key="preset.id" :value="preset.id">{{ preset.name }} · #{{ preset.id }}</option>
        </select>
        <p id="impact-recipe-help" class="mt-1 text-xs leading-5 text-gray-500">{{ t(`${p}.recipe.help`) }}</p>
        <p v-if="presetsBusy" role="status" class="text-sm">{{ t(`${p}.recipe.loading`) }}</p>
        <div v-else-if="presetsError" role="alert" class="text-sm text-amber-800 dark:text-amber-300">
          {{ t(`${p}.recipe.loadFailed`) }} <button type="button" class="underline" @click="loadPresets(true)">{{ t(`${p}.retry`) }}</button>
        </div>
        <p v-else-if="presetsLoaded && !recipePresets.length" class="text-sm">{{ t(`${p}.recipe.empty`) }}</p>
        <a href="/admin/account-quality" class="text-xs text-primary-600 underline dark:text-primary-400">{{ t(`${p}.recipe.configure`) }}</a>
        <button type="button" class="ml-3 text-xs underline" :disabled="presetsBusy" @click="loadPresets(true)">{{ t(`${p}.recipe.refresh`) }}</button>
      </div>
      <button type="submit" class="btn btn-secondary" :disabled="busy || managementBusy || !productID" data-testid="product-impact-preview">{{ t(`${p}.${busy ? 'loading' : 'preview'}`) }}</button>
    </form>
    <p id="impact-product-help" class="text-xs leading-5 text-gray-500">{{ t(`${p}.readOnly`) }}</p>
    <section v-if="productID" class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" aria-labelledby="management-authorization-title" data-testid="management-authorization">
      <h3 id="management-authorization-title" class="font-medium">{{ t(`${p}.authorization.title`) }}</h3>
      <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.authorization.boundary`) }}</p>
      <p v-if="managementState" role="status" data-testid="management-state">{{ t(`${p}.authorization.modes.${managementState.mode}`) }} · {{ t(`${p}.authorization.counts`, { revision: managementState.revision, current: managementState.current_owned, retained: managementState.retained_owned, manual: managementState.manual_memberships }) }}</p>
      <p v-else class="text-sm">{{ t(`${p}.authorization.unchecked`) }}</p>
      <p v-if="managementError" role="alert" class="text-sm text-amber-800 dark:text-amber-300" data-testid="management-error">{{ t(`${p}.authorization.${managementError}`) }}</p>
      <p v-if="managementSaved" role="status" class="text-sm text-primary-700 dark:text-primary-300" data-testid="management-saved">{{ t(`${p}.authorization.saved`) }}</p>
      <div class="flex flex-wrap items-center gap-2">
        <button type="button" class="btn btn-secondary" :disabled="managementBusy" data-testid="management-refresh" @click="managementAction('read')">{{ t(`${p}.authorization.${managementBusy ? 'loading' : 'refresh'}`) }}</button>
        <button type="button" class="btn btn-secondary" :disabled="managementBusy || !managementState || !selectedMemberships.length" data-testid="management-preview" @click="managementAction('preview')">{{ t(`${p}.authorization.preview`, { count: selectedMemberships.length }) }}</button>
        <button v-if="managementState?.mode === 'review'" type="button" class="btn btn-secondary" :disabled="managementBusy" data-testid="management-manual" @click="managementAction('manual')">{{ t(`${p}.authorization.manual`) }}</button>
      </div>
      <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.authorization.choose`) }}</p>
      <div v-if="managementPreview" class="min-w-0 space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-800" data-testid="management-preview-result">
        <p class="text-sm font-medium">{{ t(`${p}.authorization.${managementPreview.can_confirm ? 'ready' : 'blocked'}`) }}</p>
        <p class="break-all text-xs">{{ t(`${p}.authorization.expires`, { time: managementPreview.expires_at }) }}</p>
        <ul class="min-w-0 space-y-2 text-sm">
          <li v-for="row in managementPreview.memberships" :key="row.account_id" class="break-words" data-testid="management-preview-row">
            <p class="font-medium">{{ row.account_name }} · #{{ row.account_id }} · {{ t(`${p}.authorization.${row.claimable ? 'claimable' : 'notClaimable'}`) }}</p>
            <p>{{ t(`${p}.authorization.${row.ownership ? 'retainedOwner' : 'manualOwner'}`, { revision: row.ownership?.revision }) }}</p>
            <p v-for="reason in row.reasons" :key="reason" class="text-amber-800 dark:text-amber-300">{{ t(`${p}.authorization.reasons.${reason}`) }}</p>
            <p v-for="protection in row.protections" :key="protection">{{ t(`${p}.authorization.protections.${protection}`) }}</p>
          </li>
        </ul>
        <label class="flex items-start gap-2 text-sm"><input v-model="managementConsent" type="checkbox" class="mt-1" :disabled="managementBusy || !managementPreview.can_confirm || managementExpired" data-testid="management-consent" /><span>{{ t(`${p}.authorization.consent`) }}</span></label>
        <p v-if="managementExpired" class="text-sm text-amber-800 dark:text-amber-300">{{ t(`${p}.authorization.expired`) }}</p>
        <button type="button" class="btn btn-primary" :disabled="managementBusy || !managementPreview.can_confirm || !managementConsent || managementExpired" data-testid="management-confirm" @click="managementAction('review')">{{ t(`${p}.authorization.confirm`) }}</button>
      </div>
    </section>
    <p v-if="productsTotal > products.length" class="text-xs text-gray-500">{{ t(`${p}.searchMore`, { count: products.length, total: productsTotal }) }}</p>
    <div v-if="productsError" class="flex flex-wrap items-center gap-2 text-sm" role="alert">
      <span class="text-red-600 dark:text-red-400">{{ t(`${p}.productsFailed`) }}</span>
      <button type="button" class="btn btn-secondary" :disabled="productsBusy" @click="loadProducts(lastSearch)">{{ t(`${p}.retry`) }}</button>
    </div>
    <p v-if="loadedProducts && !productsBusy && !productsError && !products.length" class="text-sm" role="status">{{ t(`${p}.noProducts`) }}</p>
    <div v-if="error" role="alert" class="space-y-2 text-sm text-red-600 dark:text-red-400" data-testid="product-impact-error">
      <p>{{ t(`${p}.${error}`) }}</p>
      <button type="button" class="btn btn-secondary" :disabled="busy || !productID" @click="preview">{{ t(`${p}.retry`) }}</button>
    </div>
    <section v-if="result" class="min-w-0 space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600" aria-live="polite" data-testid="product-impact-result">
      <div class="flex flex-wrap items-start justify-between gap-2">
        <h3 class="min-w-0 break-words font-medium">{{ result.report.group_name }} <span class="text-sm text-gray-500">#{{ result.report.group_id }} · {{ result.report.platform }}</span></h3>
        <span class="rounded-full bg-gray-100 px-3 py-1 text-sm dark:bg-dark-700" data-testid="product-impact-gate">{{ t(`${p}.${result.gate_enabled ? 'gateOn' : 'gateOff'}`) }}</span>
      </div>
      <p class="break-words text-xs text-gray-500">{{ t(`${p}.snapshot`, { time: result.observed_at }) }}</p>
      <section v-if="result.monitoring_recipe" class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" aria-labelledby="recipe-title" data-testid="product-monitoring-recipe">
        <h4 id="recipe-title" class="font-medium">{{ t(`${p}.recipe.title`) }} · {{ result.monitoring_recipe.preset_name }}</h4>
        <p class="rounded-lg bg-amber-50 p-3 text-sm font-medium text-amber-900 dark:bg-amber-900/20 dark:text-amber-200" data-testid="recipe-conclusion">{{ t(`${p}.recipe.states.${recipeState}`) }}</p>
        <template v-if="result.monitoring_recipe.demand">
          <dl class="grid min-w-0 grid-cols-2 gap-3 text-sm sm:grid-cols-4">
            <div v-for="[key, value] in recipeMetrics" :key="key" class="min-w-0"><dt class="text-xs text-gray-500">{{ t(`${p}.recipe.${key}`) }}</dt><dd class="break-all font-semibold tabular-nums" :data-testid="`recipe-${key}`">{{ key === 'delta' ? '+' : '' }}{{ value }}</dd></div>
          </dl>
          <p class="text-sm">{{ t(`${p}.recipe.plans`, { count: result.monitoring_recipe.demand.additional_plans }) }}</p>
          <p class="break-words text-xs text-gray-500">{{ t(`${p}.recipe.window`, { revision: result.monitoring_recipe.demand.coverage.budget_revision, start: result.monitoring_recipe.demand.coverage.schedule_window_start, end: result.monitoring_recipe.demand.coverage.schedule_window_end }) }}</p>
          <ul v-if="recipeReasons.length" class="list-inside list-disc text-sm"><li v-for="reason in recipeReasons" :key="reason">{{ budgetReason(reason) }}</li></ul>
          <p v-if="recipePartial" class="text-xs">{{ t(`${p}.recipe.unestimated`) }}</p>
        </template>
        <p class="break-words text-xs text-gray-500">{{ t(`${p}.recipe.identity`, { revision: result.monitoring_recipe.source_policy_revision, time: result.monitoring_recipe.preset_updated_at }) }}</p>
        <p class="text-xs leading-5">{{ t(`${p}.recipe.boundary`) }}</p>
      </section>
      <p v-if="result.group_status !== 'active'" class="rounded-lg bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-900/20 dark:text-amber-200">{{ t(`${p}.inactive`) }}</p>
      <p v-if="!result.report.effective_gate" class="text-sm" data-testid="product-impact-unsupported">{{ t(`${p}.unsupported`) }}</p>
      <template v-else>
        <p class="rounded-lg p-3 text-sm font-medium" :class="admitted ? 'bg-primary-50 text-primary-800 dark:bg-primary-900/20 dark:text-primary-200' : 'bg-amber-50 text-amber-900 dark:bg-amber-900/20 dark:text-amber-200'" data-testid="product-impact-conclusion">
          {{ t(`${p}.${!result.considered_accounts ? 'empty' : !admitted ? 'allRejected' : 'priceRemaining'}`, { admitted, total: result.considered_accounts }) }}
        </p>
        <dl class="grid min-w-0 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <div v-for="[key, value] in priceMetrics" :key="key" class="min-w-0 rounded-lg bg-gray-50 p-3 dark:bg-dark-800">
            <dt class="text-xs text-gray-500">{{ t(`${p}.${key}`) }}</dt><dd class="mt-1 break-all text-lg font-semibold tabular-nums">{{ formatRate(value) }}</dd>
          </div>
        </dl>
        <p class="text-sm" data-testid="product-impact-overrides">{{ t(`${p}.overrides`, { count: result.user_override_count, admitted: minAdmitted, total: result.considered_accounts }) }}</p>
        <p v-if="admitted && !minAdmitted" class="text-sm font-medium text-amber-800 dark:text-amber-300">{{ t(`${p}.overrideEmpty`) }}</p>
        <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.candidateScope`, { total: result.bound_accounts, considered: result.considered_accounts }) }}</p>
        <ul class="grid min-w-0 gap-3 lg:grid-cols-2">
          <li v-for="row in pageRows" :key="row.account_id" class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="product-impact-account">
            <p class="break-words text-sm font-medium">{{ row.name }} <span class="text-gray-500">#{{ row.account_id }}</span></p>
            <span class="inline-block max-w-full break-words rounded-full px-2 py-1 text-xs" :class="row.class === 'admitted' ? 'bg-primary-50 text-primary-800 dark:bg-primary-900/20 dark:text-primary-200' : 'bg-amber-50 text-amber-900 dark:bg-amber-900/20 dark:text-amber-200'">{{ t(`${p}.classes.${row.class}`) }}</span>
            <dl class="grid min-w-0 grid-cols-2 gap-3 text-sm">
              <div><dt class="text-xs text-gray-500">{{ t(`${p}.admissionU`) }}</dt><dd class="break-all font-semibold tabular-nums" data-testid="product-impact-u">{{ formatRate(row.effective_upstream_rate) }}</dd><dd class="text-xs text-gray-500">{{ t(`${p}.sources.${row.rate_source}`) }}</dd></div>
              <div><dt class="text-xs text-gray-500">{{ t(`${p}.estimateC`) }}</dt><dd class="break-all font-semibold tabular-nums" data-testid="product-impact-c">{{ formatRate(row.estimated_cost_rate) }}</dd><dd class="text-xs text-gray-500">{{ t(`${p}.estimates.${row.estimated_cost_source}`) }}</dd></div>
            </dl>
            <p v-if="row.rejected_under_min_d" class="text-xs text-amber-800 dark:text-amber-300">{{ t(`${p}.rejectedAtMin`) }}</p>
            <ul v-if="row.warnings?.length" class="space-y-1 text-xs text-amber-800 dark:text-amber-300">
              <li v-for="warning in row.warnings" :key="warning">{{ t(`${p}.warnings.${knownWarning(warning)}`) }}</li>
            </ul>
          </li>
        </ul>
        <nav v-if="result.report.verdicts.length > pageSize" class="flex flex-wrap items-center gap-3 text-sm" :aria-label="t(`${p}.pages`)">
          <button type="button" class="btn btn-secondary" :disabled="page <= 1" @click="page--">{{ t('upstreamOps.previous') }}</button>
          <span>{{ page }} / {{ Math.ceil(result.report.verdicts.length / pageSize) }}</span>
          <button type="button" class="btn btn-secondary" :disabled="page * pageSize >= result.report.verdicts.length" @click="page++">{{ t('upstreamOps.next') }}</button>
        </nav>
      </template>
      <section class="min-w-0 space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600" aria-labelledby="product-management-title" data-testid="product-management-review">
        <h3 id="product-management-title" class="font-medium">{{ t(`${p}.review.title`) }}</h3>
        <p v-if="!result.management_review" class="text-sm text-gray-500">{{ t(`${p}.review.legacy`) }}</p>
        <template v-else>
          <p class="rounded-lg bg-gray-50 p-3 text-sm leading-6 dark:bg-dark-800" data-testid="product-management-mode">{{ t(`${p}.review.${result.management_review.mode === 'review' ? 'reviewMode' : 'manual'}`) }}</p>
          <section class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" aria-labelledby="product-sources-title" data-testid="product-source-inventory">
            <h4 id="product-sources-title" class="text-sm font-medium">{{ t(`${p}.review.sources.title`) }}</h4>
            <p v-if="!sourceInventory" class="text-sm text-gray-500">{{ t(`${p}.review.sources.legacy`) }}</p>
            <template v-else>
              <p class="text-sm font-medium" data-testid="product-source-state">{{ t(`${p}.review.sources.states.${sourceInventory.state}`) }}</p>
              <p v-if="sourceInventory.state === 'approved_sources'" class="break-words text-sm tabular-nums" data-testid="product-source-counts">{{ t(`${p}.review.sources.counts`, { approved: sourceInventory.approved_accounts, unbound: sourceInventory.unbound_approved_accounts, bound: result.bound_accounts }) }}</p>
              <p v-if="sourceInventory.state === 'approved_sources' && sourceInventory.approved_accounts === 0" class="text-sm text-amber-800 dark:text-amber-300">{{ t(`${p}.review.sources.empty`) }}</p>
              <ul v-if="sourceInventory.groups.length" class="flex flex-wrap gap-2 text-xs" data-testid="product-source-groups">
                <li v-for="group in sourceInventory.groups" :key="group.id" class="min-w-0 max-w-full break-words rounded-lg bg-gray-50 px-2 py-1 dark:bg-dark-800">{{ group.name || t('upstreamOps.unknown') }} · #{{ group.id }} · {{ t(`${p}.review.sources.groupStates.${group.state}`) }}</li>
              </ul>
              <details v-if="sourceInventory.accounts.length" class="min-w-0 space-y-3" data-testid="product-source-details">
                <summary class="cursor-pointer text-sm font-medium">{{ t(`${p}.review.sources.details`) }}</summary>
                <ul class="grid min-w-0 gap-3 lg:grid-cols-2">
                  <li v-for="account in sourceRows" :key="account.account_id" class="min-w-0 space-y-1 rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-800" data-testid="product-source-account">
                    <p class="break-words text-sm font-medium">{{ account.name }} · #{{ account.account_id }}</p>
                    <p class="break-words">{{ t(`${p}.review.sources.${account.in_product ? 'bound' : 'unbound'}`) }} · {{ t(`${p}.review.sources.${account.in_approved_sources === null ? 'unconfirmed' : account.in_approved_sources ? 'approved' : 'outside'}`) }}</p>
                    <p class="break-words text-gray-500">{{ t(`${p}.review.sources.nativeState`, { platform: account.platform, status: nativeStatus(account.status) }) }} · {{ t(`${p}.review.sources.${account.schedulable ? 'schedulingOn' : 'schedulingOff'}`) }}</p>
                    <label v-if="managementState && account.in_product && account.in_approved_sources === true" class="flex items-start gap-2 text-sm"><input v-model="selectedMemberships" type="checkbox" :value="account.account_id" :disabled="managementBusy" :data-testid="`management-select-${account.account_id}`" /><span>{{ t(`${p}.authorization.select`, { name: account.name }) }}</span></label>
                  </li>
                </ul>
                <nav v-if="sourceInventory.accounts.length > pageSize" class="flex flex-wrap items-center gap-3 text-sm" :aria-label="t(`${p}.review.sources.pages`)">
                  <button type="button" class="btn btn-secondary" :disabled="sourcePage <= 1" @click="sourcePage--">{{ t('upstreamOps.previous') }}</button>
                  <span>{{ sourcePage }} / {{ Math.ceil(sourceInventory.accounts.length / pageSize) }}</span>
                  <button type="button" class="btn btn-secondary" :disabled="sourcePage * pageSize >= sourceInventory.accounts.length" @click="sourcePage++">{{ t('upstreamOps.next') }}</button>
                </nav>
              </details>
              <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.review.sources.boundary`, { revision: sourceInventory.policy_revision }) }}</p>
            </template>
          </section>
          <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.review.${sourceInventory?.state === 'approved_sources' ? 'scopeApproved' : 'scope'}`, { count: result.management_review.rules_examined }) }}</p>
          <p v-if="!result.management_review.inventory_complete" class="text-sm text-amber-800 dark:text-amber-300" data-testid="product-management-incomplete">{{ t(`${p}.review.incomplete`) }}</p>
          <dl class="grid min-w-0 grid-cols-2 gap-3 lg:grid-cols-4">
            <div v-for="key in reviewCountKeys" :key="key" class="min-w-0 rounded-lg bg-gray-50 p-3 dark:bg-dark-800"><dt class="text-xs text-gray-500">{{ t(`${p}.review.${key}`) }}</dt><dd class="mt-1 text-xl font-semibold tabular-nums">{{ result.management_review[key] }}</dd></div>
          </dl>
          <p v-if="!result.management_review.rules.length" class="text-sm" data-testid="product-management-empty">{{ t(`${p}.review.empty`) }}</p>
          <details v-else class="min-w-0 space-y-3" data-testid="product-management-rules">
            <summary class="cursor-pointer text-sm font-medium">{{ t(`${p}.review.details`) }}</summary>
            <ul class="space-y-3">
              <li v-for="rule in reviewRows" :key="rule.plan_id" class="min-w-0 space-y-1 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600" data-testid="product-management-rule">
                <p class="break-words font-medium">{{ rule.account_name }} · {{ t(`${p}.review.rule`, { id: rule.plan_id }) }}</p>
                <p class="break-words">{{ t(`${p}.review.actions.${rule.action}`) }} · {{ t(`${p}.review.${rule.enabled ? 'enabled' : 'paused'}`) }}</p>
                <p class="break-words font-medium" :class="rule.risk === 'observation' ? 'text-gray-600 dark:text-gray-300' : 'text-amber-800 dark:text-amber-300'">{{ t(`${p}.review.risks.${rule.risk}`) }}</p>
                <p v-if="rule.retained_ownership" class="text-xs leading-5">{{ t(`${p}.review.retained`) }}</p>
                <p v-if="rule.judge_depends_on_product" class="text-xs leading-5 text-amber-800 dark:text-amber-300">{{ t(`${p}.review.judge`) }}</p>
              </li>
            </ul>
            <nav v-if="result.management_review.rules.length > pageSize" class="flex flex-wrap items-center gap-3 text-sm" :aria-label="t(`${p}.review.pages`)">
              <button type="button" class="btn btn-secondary" :disabled="reviewPage <= 1" @click="reviewPage--">{{ t('upstreamOps.previous') }}</button>
              <span>{{ reviewPage }} / {{ Math.ceil(result.management_review.rules.length / pageSize) }}</span>
              <button type="button" class="btn btn-secondary" :disabled="reviewPage * pageSize >= result.management_review.rules.length" @click="reviewPage++">{{ t('upstreamOps.next') }}</button>
            </nav>
          </details>
          <section class="min-w-0 space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" aria-labelledby="product-writers-title" data-testid="product-writer-review">
            <h4 id="product-writers-title" class="text-sm font-medium">{{ t(`${p}.review.writers.title`) }}</h4>
            <p v-if="!writerReview" class="text-sm text-gray-500">{{ t(`${p}.review.writers.legacy`) }}</p>
            <template v-else>
              <p class="rounded-lg bg-amber-50 p-3 text-sm font-medium text-amber-900 dark:bg-amber-900/20 dark:text-amber-200" data-testid="product-writer-state">{{ t(`${p}.review.writers.states.${writerState}`) }}</p>
              <dl class="grid min-w-0 grid-cols-2 gap-3 lg:grid-cols-4">
                <div v-for="key in writerCountKeys" :key="key" class="min-w-0 rounded-lg bg-gray-50 p-3 dark:bg-dark-800"><dt class="text-xs text-gray-500">{{ t(`${p}.review.writers.${key}`) }}</dt><dd class="mt-1 text-xl font-semibold tabular-nums">{{ writerReview[key] }}</dd></div>
              </dl>
              <details v-if="writerReview.writers.length" class="min-w-0 space-y-3" data-testid="product-writer-details">
                <summary class="cursor-pointer text-sm font-medium">{{ t(`${p}.review.writers.details`) }}</summary>
                <ul class="min-w-0 space-y-2">
                  <li v-for="row in writerRows" :key="`${row.kind}:${row.account_id}`" class="min-w-0 space-y-1 rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-800" data-testid="product-writer-row">
                    <p class="break-words font-medium">{{ row.account_id === null ? t(`${p}.review.writers.global`) : `${row.account_name} · #${row.account_id}` }}</p>
                    <p class="break-words">{{ t(`${p}.review.writers.kinds.${row.kind}`) }} · {{ t(`${p}.review.writers.${row.enabled === null ? 'unconfirmed' : row.enabled ? 'enabled' : 'inactive'}`) }}</p>
                    <p class="break-words font-medium">{{ t(`${p}.review.writers.risks.${row.risk}`) }}</p>
                    <ul class="list-inside list-disc text-xs leading-5"><li v-for="reason in row.reasons" :key="reason">{{ t(`${p}.review.writers.reasons.${reason}`) }}</li></ul>
                    <a :href="writerLink(row.kind)" class="inline-block text-primary-600 underline dark:text-primary-400">{{ t(`${p}.review.writers.configure`) }}</a>
                  </li>
                </ul>
                <nav v-if="writerReview.writers.length > pageSize" class="flex flex-wrap items-center gap-3 text-sm" :aria-label="t(`${p}.review.writers.pages`)">
                  <button type="button" class="btn btn-secondary" :disabled="writerPage <= 1" @click="writerPage--">{{ t('upstreamOps.previous') }}</button>
                  <span>{{ writerPage }} / {{ Math.ceil(writerReview.writers.length / pageSize) }}</span>
                  <button type="button" class="btn btn-secondary" :disabled="writerPage * pageSize >= writerReview.writers.length" @click="writerPage++">{{ t('upstreamOps.next') }}</button>
                </nav>
              </details>
              <p class="text-xs leading-5 text-gray-500">{{ t(`${p}.review.writers.boundary`, { count: writerReview.accounts_examined }) }}</p>
            </template>
          </section>
          <div class="min-w-0 space-y-2 rounded-lg bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-900/20 dark:text-amber-200" data-testid="product-management-budget">
            <h4 class="font-medium">{{ t(`${p}.review.budgetTitle`) }}</h4>
            <p class="font-medium" data-testid="product-management-budget-state">{{ t(`${p}.review.budgetStates.${result.management_review.budget.state}`) }}</p>
            <p class="break-words tabular-nums">{{ t(`${p}.review.budgetCounts`, { minimum: result.management_review.budget.minimum_requests_per_day, capacity: result.management_review.budget.regular_day_capacity, unknown: result.management_review.budget.unestimated_sources }) }}</p>
            <p class="break-words text-xs">{{ t(`${p}.review.budgetWindow`, { revision: result.management_review.budget.revision, start: result.management_review.budget.window_start, end: result.management_review.budget.window_end }) }}</p>
            <ul v-if="budgetMessages.length" class="list-inside list-disc"><li v-for="reason in budgetMessages" :key="reason">{{ budgetReason(reason) }}</li></ul>
            <p class="text-xs leading-5">{{ t(`${p}.review.budgetBoundary`) }}</p>
          </div>
          <div class="flex flex-wrap gap-x-5 gap-y-2 text-sm">
            <a href="/admin/account-quality" class="text-primary-600 underline dark:text-primary-400" data-testid="product-management-quality-link">{{ t(`${p}.review.qualityLink`) }}</a>
            <a href="/admin/settings?tab=gateway#settings-section-upstream-billing-probe" class="text-primary-600 underline dark:text-primary-400" data-testid="product-management-budget-link">{{ t(`${p}.review.budgetLink`) }}</a>
          </div>
        </template>
      </section>
      <p class="rounded-lg bg-gray-50 p-3 text-sm leading-6 dark:bg-dark-800" data-testid="product-impact-boundary">{{ t(`${p}.boundary`) }}</p>
    </section>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import { list as listGroups } from '@/api/admin/groups'
import { listQualityPresets, type QualityRulePreset } from '@/api/admin/accountQuality'
import { previewProcurementProductImpact, readProcurementManagement, previewProcurementManagement, saveProcurementManagement, type ProcurementManagementPreview, type ProcurementManagementState, type ProcurementProductImpact } from '@/api/admin/procurementCatalog'
import { validProcurementProductImpact, validProcurementManagementState, validProcurementManagementPreview, observationRecipePreset } from '@/utils/procurementProductImpact'
import type { AdminGroup } from '@/types'

const { t } = useI18n(), p = 'upstreamOps.productImpact'
const productID = ref<number | null>(null), products = ref<AdminGroup[]>([]), productsTotal = ref(0)
const loadedProducts = ref(false), productsBusy = ref(false), productsError = ref(false)
const presetID = ref<number | null>(null), presets = ref<QualityRulePreset[]>([]), presetsBusy = ref(false), presetsError = ref(false), presetsLoaded = ref(false)
const recipePresets = computed(() => presets.value.filter(observationRecipePreset))
let presetsController: AbortController | undefined
async function loadPresets(force: boolean | Event = false) {
  if (presetsBusy.value || presetsLoaded.value && force !== true) return
  if (force === true) { previewController?.abort(); previewGeneration++; busy.value = false; result.value = null; error.value = '' }
  presetsController = new AbortController(); presetsBusy.value = true; presetsError.value = false
  try {
    const next = await listQualityPresets(presetsController.signal)
    if (!Array.isArray(next)) throw new Error('invalid presets')
    if (alive) {
      presets.value = next; presetsLoaded.value = true
      if (presetID.value != null && !recipePresets.value.some(p => p.id === presetID.value)) presetID.value = null
    }
  } catch { if (alive) presetsError.value = true }
  finally { if (alive) presetsBusy.value = false }
}
const result = ref<ProcurementProductImpact | null>(null), busy = ref(false), error = ref(''), page = ref(1), reviewPage = ref(1), sourcePage = ref(1), writerPage = ref(1), pageSize = 20
let alive = true, productsGeneration = 0, previewGeneration = 0, lastSearch = ''
let productsController: AbortController | undefined, previewController: AbortController | undefined
const selected = ref<AdminGroup | null>(null)
const managementState = ref<ProcurementManagementState | null>(null), managementPreview = ref<ProcurementManagementPreview | null>(null)
const selectedMemberships = ref<number[]>([]), managementConsent = ref(false), managementBusy = ref(false), managementError = ref(''), managementSaved = ref(false)
const managementClock = ref(Date.now()), managementReceived = ref(0)
let managementGeneration = 0, managementController: AbortController | undefined
const managementExpired = computed(() => managementClock.value >= (managementPreview.value ? managementReceived.value + Date.parse(managementPreview.value.expires_at) - Date.parse(managementPreview.value.observed_at) : Infinity))
const managementTimer = setInterval(() => { managementClock.value = Date.now() }, 1000)
watch(selectedMemberships, () => { managementPreview.value = null; managementConsent.value = false; managementSaved.value = false }, { deep: true })
watch(productID, () => {
  managementGeneration++; managementController?.abort()
  managementState.value = null; managementPreview.value = null; selectedMemberships.value = []; managementConsent.value = false
  managementError.value = ''; managementSaved.value = false; managementBusy.value = false
  if (productID.value) void managementAction('read')
})
async function managementAction(action: 'read' | 'preview' | 'review' | 'manual') {
  if (!productID.value || managementBusy.value) return
  const id = productID.value, generation = ++managementGeneration, ids = selectedMemberships.value.slice()
  const draft = managementPreview.value, state = managementState.value
  if (action === 'review' && (!draft?.can_confirm || !managementConsent.value || managementExpired.value)) return
  if (action === 'manual' && !state) return
  managementController?.abort(); managementController = new AbortController()
  const signal = managementController.signal
  managementBusy.value = true; managementError.value = ''; managementSaved.value = false
  let saved = false
  try {
    if (action === 'preview') {
      managementPreview.value = null; managementConsent.value = false
      const next = await previewProcurementManagement(id, ids, signal)
      if (!validProcurementManagementPreview(next, id, ids)) throw new Error('invalid review preview')
      if (alive && generation === managementGeneration) {
        managementPreview.value = next; managementState.value = next.state
        managementReceived.value = Date.now(); managementClock.value = Date.now()
      }
    } else {
      let written: ProcurementManagementState | undefined
      let refreshImpact = false
      if (action === 'review' && draft) written = await saveProcurementManagement(id, { version: 1, mode: 'review', expected_revision: draft.state.revision, account_ids: ids, observed_at: draft.observed_at, fingerprint: draft.fingerprint }, signal)
      if (action === 'manual' && state) written = await saveProcurementManagement(id, { version: 1, mode: 'manual', expected_revision: state.revision }, signal)
      saved = action !== 'read'
      if (saved && alive && generation === managementGeneration) {
        // A successful write invalidates every pre-save snapshot, including a
        // still-pending read. Its late response must not restore the old mode.
        refreshImpact = !!result.value || busy.value
        previewController?.abort(); previewGeneration++
        busy.value = false; result.value = null; error.value = ''
      }
      if (saved && (!written || !validProcurementManagementState(written, id))) throw new Error('invalid review save')
      const next = await readProcurementManagement(id, signal)
      if (!validProcurementManagementState(next, id)) throw new Error('invalid review state')
      if (alive && generation === managementGeneration) {
        managementState.value = next; managementPreview.value = null; managementConsent.value = false
        managementSaved.value = saved && written?.revision === next.revision && written?.mode === next.mode
        if (saved && !managementSaved.value) managementError.value = 'conflict'
        if (managementSaved.value && refreshImpact) await preview()
      }
    }
  } catch (e) {
    if (alive && generation === managementGeneration) {
      const status = (e as { status?: number })?.status
      managementError.value = saved ? 'savedReadFailed' : status === 409 ? 'conflict' : status === 403 ? 'forbidden' : 'failed'
      managementPreview.value = null; managementConsent.value = false
      if (saved || action === 'read') managementState.value = null
    }
  } finally { if (alive && generation === managementGeneration) managementBusy.value = false }
}
onBeforeUnmount(() => { clearInterval(managementTimer); managementGeneration++; managementController?.abort() })
const options = computed(() => {
  const rows = products.value.slice()
  if (selected.value && !rows.some(row => row.id === selected.value?.id)) rows.unshift(selected.value)
  return rows.map(row => ({ value: row.id, label: `${row.name} · ${row.platform} #${row.id}${row.status === 'inactive' ? ' · ' + t(`${p}.disabledProduct`) : ''}` }))
})
const admitted = computed(() => result.value?.report.verdicts.filter(row => row.class === 'admitted').length ?? 0)
const minAdmitted = computed(() => result.value?.report.verdicts.filter(row => row.class === 'admitted' && !row.rejected_under_min_d).length ?? 0)
const pageRows = computed(() => result.value?.report.verdicts.slice((page.value - 1) * pageSize, page.value * pageSize) ?? [])
const reviewRows = computed(() => result.value?.management_review?.rules.slice((reviewPage.value - 1) * pageSize, reviewPage.value * pageSize) ?? [])
const sourceInventory = computed(() => result.value?.management_review?.source_inventory)
const writerReview = computed(() => result.value?.management_review?.writer_review)
const writerRows = computed(() => writerReview.value?.writers.slice((writerPage.value - 1) * pageSize, writerPage.value * pageSize) ?? [])
const writerState = computed(() => {
  const review = writerReview.value
  if (!review) return 'unknown'
  return review.membership_conflicts ? 'conflict' : review.unknown || !review.inventory_complete ? 'unknown' : review.account_protections ? 'protected' : 'no_membership_conflict'
})
const writerCountKeys = ['membership_conflicts', 'account_protections', 'historical_only', 'unknown'] as const
function writerLink(kind: string): string {
  if (kind === 'two_fa_admission') return '/admin/account-quality'
  if (kind === 'account_configuration' || kind === 'excel_bps_403') return '/admin/accounts'
  return kind === 'astra_scheduling' ? '/admin/harvest-flow' : '/admin/auto-config'
}
const sourceRows = computed(() => sourceInventory.value?.accounts.slice((sourcePage.value - 1) * pageSize, sourcePage.value * pageSize) ?? [])
const nativeStatus = (status: string) => ['active', 'inactive', 'expired', 'error'].includes(status) ? t(`admin.accounts.status.${status}`) : t('upstreamOps.unknown')
const reviewCountKeys = ['conflicting_rules', 'protection_rules', 'unknown_rules', 'judge_dependency_rules'] as const
const budgetMessages = computed(() => [...new Set([...(result.value?.management_review?.budget.reasons ?? []), ...(result.value?.management_review?.budget.warnings ?? [])])])
const budgetReasons = new Set(['runner_capacity', 'minute_capacity', 'day_capacity', 'site_capacity', 'key_capacity', 'billing_scope_unresolved', 'periodic_site_capacity', 'periodic_site_unresolved', 'periodic_key_capacity', 'periodic_key_unresolved', 'runner_no_headroom', 'invalid_interval', 'scheduled_unresolved', 'scheduled_inventory_truncated', 'candy_unresolved', 'candy_inventory_truncated', 'active_unresolved', 'active_inventory_truncated', 'active_quota_unestimated'])
const budgetReason = (reason: string) => t(`admin.settings.upstreamBillingProbe.budget.capacityReason.${budgetReasons.has(reason) ? reason : 'unknown'}`)
const recipeCoverage = computed(() => result.value?.monitoring_recipe?.demand?.coverage)
const recipePartial = computed(() => {
  const c = recipeCoverage.value
  return !!c && (c.inventory_complete !== true || c.plan_inventory_complete !== true || c.candy_inventory_complete !== true || c.active_inventory_complete !== true ||
    c.warnings.length > 0 || !Array.isArray(c.unestimated_demand_sources) || c.unestimated_demand_sources.length > 0)
})
const recipeState = computed(() => {
  const r = result.value?.monitoring_recipe, c = recipeCoverage.value
  return r?.state !== 'previewed' ? r?.state : !c?.enforced ? 'not_enforced' : !c.can_save ? 'insufficient' : recipePartial.value ? 'partial' : 'lower_bound_fits'
})
const recipeReasons = computed(() => [...new Set([...(recipeCoverage.value?.blocking_reasons ?? []), ...(recipeCoverage.value?.warnings ?? [])])])
const recipeMetrics = computed(() => [
  ['current', result.value?.monitoring_recipe?.demand?.current_minimum_requests_per_day], ['delta', result.value?.monitoring_recipe?.demand?.delta_minimum_requests_per_day],
  ['proposed', recipeCoverage.value?.minimum_total_requests_per_day], ['capacity', recipeCoverage.value?.regular_day_capacity]
] as const)
const priceMetrics = computed<Array<[string, number]>>(() => !result.value ? [] : [
  ['saleD', result.value.report.default_d], ['threshold', result.value.report.threshold_default],
  ['minimumD', result.value.report.min_effective_d], ['minimumThreshold', result.value.report.threshold_min_d]
])
const warnings = new Set(['probe_snapshot_missing', 'probe_snapshot_stale', 'probe_failed', 'probe_unsupported', 'probe_rate_evidence_invalid', 'probe_request_scope_not_evaluated', 'manual_rate_1_suspected_unmaintained'])
const knownWarning = (warning: string) => warnings.has(warning) ? warning : 'unknown'
const formatRate = (value: number | undefined) => value == null ? t('upstreamOps.unknown') : new Intl.NumberFormat(undefined, { maximumFractionDigits: 8 }).format(value)
watch(productID, id => {
  selected.value = products.value.find(row => row.id === id) ?? (selected.value?.id === id ? selected.value : null)
  previewController?.abort(); previewGeneration++; busy.value = false; result.value = null; error.value = ''; page.value = 1; reviewPage.value = 1; sourcePage.value = 1; writerPage.value = 1
}, { flush: 'sync' })
watch(presetID, () => { previewController?.abort(); previewGeneration++; busy.value = false; result.value = null; error.value = '' }, { flush: 'sync' })
function openProducts() { if (!loadedProducts.value && !productsBusy.value) void loadProducts('') }
async function loadProducts(search: string) {
  lastSearch = search
  productsController?.abort(); productsController = new AbortController()
  const generation = ++productsGeneration
  productsBusy.value = true; productsError.value = false
  try {
    const next = await listGroups(1, 100, { search, sort_by: 'name', sort_order: 'asc' }, { signal: productsController.signal })
    if (!next || !Array.isArray(next.items) || !Number.isSafeInteger(next.total) || next.total < next.items.length ||
      next.items.some(row => !Number.isSafeInteger(row.id) || row.id < 1 || typeof row.name !== 'string' || typeof row.platform !== 'string')) throw new Error('invalid product list')
    if (alive && generation === productsGeneration) { products.value = next.items; productsTotal.value = next.total; loadedProducts.value = true }
  } catch { if (alive && generation === productsGeneration) productsError.value = true }
  finally { if (alive && generation === productsGeneration) productsBusy.value = false }
}
async function preview() {
  if (busy.value || !productID.value) return
  const id = productID.value, generation = ++previewGeneration
  previewController?.abort(); previewController = new AbortController()
  busy.value = true; error.value = ''; result.value = null; page.value = 1; reviewPage.value = 1; sourcePage.value = 1; writerPage.value = 1
  try {
    const next = presetID.value == null ? await previewProcurementProductImpact(id, previewController.signal) : await previewProcurementProductImpact(id, previewController.signal, presetID.value)
    if (!validProcurementProductImpact(next, id, presetID.value ?? undefined)) throw new Error('invalid product impact')
    if (alive && generation === previewGeneration) result.value = next
  } catch (e) {
    if (alive && generation === previewGeneration) {
      const status = (e as { status?: number })?.status, reason = (e as { reason?: string })?.reason
      error.value = reason === 'PROCUREMENT_MONITORING_RECIPE_INVALID' || reason === 'PROCUREMENT_MONITORING_RECIPE_NOT_FOUND' ? 'recipe.invalid' : status === 403 ? 'forbidden' : status === 404 ? 'notFound' : reason === 'PROCUREMENT_PRODUCT_IMPACT_TOO_LARGE' ? 'tooLarge' : 'failed'
    }
  } finally { if (alive && generation === previewGeneration) busy.value = false }
}
onBeforeUnmount(() => { alive = false; productsController?.abort(); previewController?.abort(); presetsController?.abort() })
</script>
