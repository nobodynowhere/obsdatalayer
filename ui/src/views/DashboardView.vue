<script setup>
import { ref, onMounted, computed } from 'vue'
import { TenantService, UserService, RoleService, InstanceService } from '@/services'

const tenants = ref([])
const users = ref([])
const roles = ref([])
const instances = ref([])
const loading = ref(true)
const error = ref('')

const tenantSvc = new TenantService()
const userSvc = new UserService()
const roleSvc = new RoleService()
const instanceSvc = new InstanceService()

const instanceCount = computed(() => instances.value.length)

const adminCount = computed(() => users.value.filter((u) => u.admin).length)
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
    const [t, u, r, i] = await Promise.all([
      tenantSvc.list(),
      userSvc.list(),
      roleSvc.list(),
      instanceSvc.list(),
    ])
    tenants.value = t
    users.value = u
    roles.value = r
    instances.value = i
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
