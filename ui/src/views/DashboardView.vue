<script setup>
import { ref, onMounted, computed } from 'vue'
import { TenantService, UserService, RoleService, InstanceService, MetricsService } from '@/services'

const tenants = ref([])
const users = ref([])
const roles = ref([])
const instances = ref([])
const traffic = ref(null)
const loading = ref(true)
const error = ref('')

const tenantSvc = new TenantService()
const userSvc = new UserService()
const roleSvc = new RoleService()
const instanceSvc = new InstanceService()
const metricsSvc = new MetricsService()

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
  ]
})

function fmt(n) {
  return Number(n ?? 0).toLocaleString()
}
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
    const [t, u, r, i, mx] = await Promise.all([
      tenantSvc.list(),
      userSvc.list(),
      roleSvc.list(),
      instanceSvc.list(),
      // Counters are a nice-to-have on this page: a gateway that cannot report
      // them should still render its configuration.
      metricsSvc.get().catch(() => null),
    ])
    tenants.value = t
    users.value = u
    roles.value = r
    instances.value = i
    traffic.value = mx
  } catch (e) {
    error.value = e?.response?.data?.error || 'Failed to load gateway state.'
  } finally {
    loading.value = false
  }
}

onMounted(load)
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
    </div>

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
