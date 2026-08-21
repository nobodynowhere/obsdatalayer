<script setup>
import { ref, onMounted, onUnmounted, computed } from 'vue'
import {
  TenantService,
  UserService,
  RoleService,
  InstanceService,
  MetricsService,
  SettingsService,
} from '@/services'

const tenants = ref([])
const users = ref([])
const roles = ref([])
const instances = ref([])
const traffic = ref(null)
const settings = ref(null)
const loading = ref(true)
const error = ref('')

// Config age is reported by the gateway, measured against its own clock, and
// then advanced locally so a page left open does not keep showing the age at
// the moment it was fetched. fetchedAt is the browser clock at that moment, so
// only the elapsed time since the fetch is ever taken from this machine.
const fetchedAt = ref(Date.now())
const now = ref(Date.now())
let ticker = null

const tenantSvc = new TenantService()
const userSvc = new UserService()
const roleSvc = new RoleService()
const instanceSvc = new InstanceService()
const metricsSvc = new MetricsService()
const settingsSvc = new SettingsService()

const instanceCount = computed(() => instances.value.length)

const adminCount = computed(() => users.value.filter((u) => u.admin).length)

// Counters are cumulative since process start, so they reset on restart. They
// are also pruned when an instance is deleted, which is why a total can fall.
const trafficCards = computed(() => {
  const t = traffic.value
  if (!t) return []
  return [
    { key: 'fanout', label: 'Fan-out pushes', value: t.fanout_requests, hint: 'Upstream write attempts' },
    { key: 'failures', label: 'Failed pushes', value: t.fanout_failures, hint: 'No response, or 4xx/5xx', alert: t.fanout_failures > 0 },
    { key: 'suppressed', label: 'Suppressed errors', value: t.suppressed_errors, hint: 'Mimir 400s reported as success', alert: t.suppressed_errors > 0 },
    { key: 'partial', label: 'Partial failures', value: t.partial_failures, hint: 'Succeeded with a target down', alert: t.partial_failures > 0 },
    { key: 'items', label: 'Items forwarded', value: t.items_forwarded, hint: 'Series and log streams' },
    { key: 'labels', label: 'Labels rewritten', value: t.labels_rewritten, hint: 'Dropped, injected or overwritten' },
    { key: 'reads-ok', label: 'Reads served', value: t.read_successes, hint: 'Queries answered upstream' },
    { key: 'reads-fail', label: 'Read failures', value: t.read_failures, hint: 'Target unreachable or 5xx', alert: t.read_failures > 0 },
    {
      key: 'reads-failover',
      label: 'Read failovers',
      value: t.read_failovers,
      hint: 'Served only after a target failed',
      alert: t.read_failovers > 0,
    },
    {
      key: 'reads-truncated',
      label: 'Truncated reads',
      value: t.read_truncated,
      hint: 'Answered 200, body cut short',
      alert: t.read_truncated > 0,
    },
    // No alert: a caller closing a panel or cancelling a query lands here, and
    // a non-zero count is normal traffic rather than a fault.
    {
      key: 'reads-disconnected',
      label: 'Client disconnects',
      value: t.read_client_disconnects,
      hint: 'Caller left before the body finished',
    },
  ]
})

// Per-target read health. Only instances that have actually served a read
// appear: an instance nobody has queried has nothing to report, and showing it
// at zero would read as a problem rather than as an absence of traffic.
const readTargets = computed(() => {
  const out = []
  for (const inst of traffic.value?.instances ?? []) {
    const rows = (inst.read_targets ?? []).map((t) => {
      const total = (t.successes ?? 0) + (t.failures ?? 0)
      const successes = t.successes ?? 0
      const failures = t.failures ?? 0
      const okPct = total ? Math.round((successes / total) * 100) : 0
      const failPct = total ? 100 - okPct : 0
      const lastResult = t.last_result || (failures && !successes ? 'failure' : 'success')
      const statusText = lastResult === 'failure' ? 'latest failure' : 'latest success'
      const history = Array.isArray(t.recent_results) && t.recent_results.length
        ? t.recent_results.filter((result) => result === 'success' || result === 'failure')
        : []
      const recentResults = history.length ? history : [lastResult]
      return {
        target: t.target,
        successes,
        failures,
        total,
        okPct,
        failPct,
        lastResult,
        recentResults,
        statusText,
        hover: `${successes.toLocaleString()} served (${okPct}%), ${failures.toLocaleString()} failed (${failPct}%), ${statusText}`,
      }
    })
    if (rows.length) out.push({ instance: inst.instance, rows })
  }
  return out
})

function fmt(n) {
  return Number(n ?? 0).toLocaleString()
}

// Go renders durations as "30s", "1m0s", "1h30m0s". Parsed rather than assumed
// so the staleness threshold below tracks whatever the operator configured.
function parseGoDuration(text) {
  if (typeof text !== 'string') return null
  let total = 0
  let matched = false
  const units = { h: 3600, m: 60, s: 1, ms: 0.001, us: 1e-6, ns: 1e-9 }
  for (const [, value, unit] of text.matchAll(/([\d.]+)(ms|us|ns|h|m|s)/g)) {
    const scale = units[unit]
    if (scale === undefined) return null
    total += Number(value) * scale
    matched = true
  }
  return matched ? total : null
}

function humanizeAge(seconds) {
  const s = Math.max(0, Math.floor(seconds))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`
}

const reloadIntervalSeconds = computed(() => parseGoDuration(settings.value?.reload_interval) ?? 30)

// Three intervals: two consecutive missed reloads. One is a tick that has not
// landed yet, which is normal and would make this flag flicker on every page
// load.
const staleAfterSeconds = computed(() => reloadIntervalSeconds.value * 3)

// The config the gateway is actually serving, and how old it is. Absent means
// no reload has succeeded since the process started, which for a running
// gateway means the very first tick has not landed yet.
const configFreshness = computed(() => {
  const t = traffic.value
  if (!t) return null

  const failures = t.config_reload_failures ?? 0
  if (t.config_age_seconds === null || t.config_age_seconds === undefined) {
    return { unknown: true, failures }
  }

  const elapsed = Math.max(0, (now.value - fetchedAt.value) / 1000)
  const age = t.config_age_seconds + elapsed
  const stale = age > staleAfterSeconds.value

  let hint
  if (stale) {
    hint = `Reloads are failing; expected every ${settings.value?.reload_interval ?? '30s'}`
  } else if (failures > 0) {
    hint = `Current. ${fmt(failures)} reload failure(s) since start`
  } else {
    hint = `Re-read from the database every ${settings.value?.reload_interval ?? '30s'}`
  }

  return { unknown: false, age, stale, failures, hint, text: humanizeAge(age) }
})
const unmappedTenants = computed(() => {
  const referenced = new Set()
  for (const r of roles.value) for (const g of r.grants ?? []) for (const t of g.tenant_ids ?? []) referenced.add(t)
  for (const u of users.value) for (const g of u.grants ?? []) for (const t of g.tenant_ids ?? []) referenced.add(t)
  return tenants.value.filter((t) => !referenced.has(t.id))
})

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [t, u, r, i, mx, st] = await Promise.all([
      tenantSvc.list(),
      userSvc.list(),
      roleSvc.list(),
      instanceSvc.list(),
      // Counters are a nice-to-have on this page: a gateway that cannot report
      // them should still render its configuration.
      metricsSvc.get().catch(() => null),
      // Only for the reload interval, which sets the staleness threshold. A
      // failure here falls back to the 30s default rather than hiding the card.
      settingsSvc.get().catch(() => null),
    ])
    tenants.value = t
    users.value = u
    roles.value = r
    instances.value = i
    traffic.value = mx
    settings.value = st
    fetchedAt.value = Date.now()
    now.value = fetchedAt.value
  } catch (e) {
    error.value = e?.response?.data?.error || 'Failed to load gateway state.'
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  // Advances the displayed config age between refreshes, so a dashboard left
  // open on a wall keeps counting up while reloads are failing rather than
  // freezing at whatever it read on load.
  ticker = window.setInterval(() => {
    now.value = Date.now()
  }, 1000)
})

onUnmounted(() => {
  if (ticker) window.clearInterval(ticker)
})
</script>

<template>
  <div class="page-title">
    <div>
      <h1>Overview</h1>
      <p>Current state of the gateway's tenants, accounts and routing.</p>
    </div>
    <PrimeButton icon="pi pi-refresh" label="Refresh" severity="secondary" outlined @click="load" />
  </div>

  <PrimeMessage v-if="error" severity="error" :closable="false">{{ error }}</PrimeMessage>

  <div v-if="loading" class="empty-state"><PrimeProgressSpinner style="width: 2.5rem" /></div>

  <template v-else>
    <div class="stat-grid">
      <div class="stat-card">
        <div class="stat-card__label">Tenants</div>
        <div class="stat-card__value">{{ tenants.length }}</div>
        <div class="stat-card__hint">Injected upstream as X-Scope-OrgID</div>
      </div>
      <div class="stat-card">
        <div class="stat-card__label">Instances</div>
        <div class="stat-card__value">{{ instanceCount }}</div>
        <div class="stat-card__hint">Configured backends</div>
      </div>
      <div class="stat-card">
        <div class="stat-card__label">Users</div>
        <div class="stat-card__value">{{ users.length }}</div>
        <div class="stat-card__hint">{{ adminCount }} with admin access</div>
      </div>
      <div class="stat-card">
        <div class="stat-card__label">Roles</div>
        <div class="stat-card__value">{{ roles.length }}</div>
        <div class="stat-card__hint">Grant bundles</div>
      </div>
      <div v-if="configFreshness" class="stat-card">
        <div class="stat-card__label">Config age</div>
        <div class="stat-card__value">
          <template v-if="configFreshness.unknown">&mdash;</template>
          <template v-else>{{ configFreshness.text }}</template>
          <PrimeTag
            v-if="configFreshness.stale"
            severity="danger"
            value="STALE"
            class="stat-card__flag"
          />
        </div>
        <div class="stat-card__hint">
          <template v-if="configFreshness.unknown">No reload has completed yet</template>
          <template v-else>{{ configFreshness.hint }}</template>
        </div>
      </div>
    </div>

    <PrimeMessage
      v-if="configFreshness && configFreshness.stale"
      severity="error"
      :closable="false"
      class="mb-3"
    >
      The gateway has not reloaded its configuration for
      {{ configFreshness.text }} and is still serving the last copy it read
      successfully. Traffic is unaffected, but nothing changed here &mdash; in the admin UI or in
      the database &mdash; is taking effect. The usual cause is the configuration database being
      unreachable; check the gateway log for
      <span class="mono">auto reload failed</span>.
    </PrimeMessage>

    <div class="section-label">
      Traffic
      <span class="section-label__note">cumulative since gateway start</span>
    </div>

    <div v-if="!traffic" class="stat-card stat-card--empty">
      Counters are unavailable. The gateway is reachable, but /api/metrics did not respond.
    </div>

    <div v-else class="stat-grid">
      <div v-for="card in trafficCards" :key="card.key" class="stat-card">
        <div class="stat-card__label">{{ card.label }}</div>
        <div class="stat-card__value">
          {{ fmt(card.value) }}
          <PrimeTag v-if="card.alert" severity="warn" value="!" class="stat-card__flag" />
        </div>
        <div class="stat-card__hint">{{ card.hint }}</div>
      </div>
    </div>

    <template v-if="readTargets.length">
      <div class="section-label">
        Read targets
        <span class="section-label__note">which upstream answered, per instance</span>
      </div>

      <PrimeCard class="mb-3">
        <template #content>
          <div v-for="group in readTargets" :key="group.instance" class="read-group">
            <div class="read-group__name mono">{{ group.instance }}</div>
            <div v-for="(row, i) in group.rows" :key="row.target" class="read-target">
              <div class="read-target__name mono" :title="row.target">
                <span class="target-rank">{{ i + 1 }}</span>{{ row.target }}
              </div>
              <div
                class="read-strip"
                :title="row.hover"
                :aria-label="`${row.target} read history, ${row.statusText}`"
                :style="{ '--sample-count': row.recentResults.length }"
              >
                <span
                  v-for="(result, sampleIndex) in row.recentResults"
                  :key="`${sampleIndex}-${result}`"
                  class="read-strip__sample"
                  :class="`read-strip__sample--${result}`"
                ></span>
              </div>
              <div class="read-target__counts">
                <span class="read-count read-count--ok">{{ fmt(row.successes) }}</span>
                <span class="read-count read-count--fail" :class="{ 'is-zero': !row.failures }">
                  {{ fmt(row.failures) }}
                </span>
              </div>
            </div>
          </div>
          <small>
            Every target receives every write; reads try them in configured order. A target with
            failures is being skipped for a short cool-off and retried after it.
          </small>
        </template>
      </PrimeCard>
    </template>

    <PrimeMessage
      v-if="unmappedTenants.length"
      severity="warn"
      :closable="false"
      class="mb-3"
    >
      {{ unmappedTenants.length }} tenant(s) are not referenced by any role or user grant, so no
      one can read or write their data:
      <span class="mono">{{ unmappedTenants.map((t) => t.name).join(', ') }}</span>
    </PrimeMessage>

    <PrimeCard>
      <template #title>Tenants</template>
      <template #content>
        <PrimeDataTable :value="tenants" size="small" data-key="id" striped-rows>
          <template #empty>
            <div class="empty-state">No tenants yet.</div>
          </template>
          <PrimeColumn field="name" header="Name" sortable />
          <PrimeColumn field="id" header="Tenant ID (X-Scope-OrgID)">
            <template #body="{ data }"><span class="mono">{{ data.id }}</span></template>
          </PrimeColumn>
          <PrimeColumn header="Grafana ID">
            <template #body="{ data }">
              <span v-if="data.grafana_id">{{ data.grafana_id }}</span>
              <span v-else class="stat-card__hint">unassigned</span>
            </template>
          </PrimeColumn>
        </PrimeDataTable>
      </template>
    </PrimeCard>
  </template>
</template>
