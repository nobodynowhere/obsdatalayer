<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useToast } from 'primevue/usetoast'
import { TenantService } from '@/services'

const svc = new TenantService()
const route = useRoute()
const router = useRouter()
const toast = useToast()

const loading = ref(true)
const error = ref('')
const doc = ref(null)
const activeTab = ref('rules')

const tenantID = computed(() => String(route.params.id ?? ''))
const tenant = computed(() => doc.value?.tenant)
const instance = computed(() => doc.value?.instance ?? 'unavailable')
const groups = computed(() => doc.value?.rules?.data?.groups ?? [])
const rules = computed(() => groups.value.flatMap((group) => (group.rules ?? []).map((rule) => ({ ...rule, group: group.name }))))
const alerts = computed(() => doc.value?.alerts?.data?.alerts ?? [])

async function load() {
  loading.value = true
  error.value = ''
  try {
    doc.value = await svc.mimirObservability(tenantID.value)
  } catch (e) {
    error.value = message(e)
    toast.add({ severity: 'error', summary: 'Mimir view failed', detail: error.value, life: 8000 })
  } finally {
    loading.value = false
  }
}

function message(e) {
  return e?.response?.data?.error || e.message || 'Unexpected error'
}

function labelText(labels) {
  if (!labels || !Object.keys(labels).length) return 'none'
  return Object.entries(labels)
    .map(([key, value]) => `${key}=${value}`)
    .join(', ')
}

function ruleName(rule) {
  return rule.name || rule.alert || rule.record || 'unnamed'
}

function alertName(alert) {
  return alert.labels?.alertname || alert.name || 'unnamed'
}

function tagSeverity(value) {
  switch (String(value ?? '').toLowerCase()) {
    case 'firing':
    case 'critical':
    case 'error':
      return 'danger'
    case 'pending':
    case 'warning':
    case 'warn':
      return 'warn'
    case 'inactive':
    case 'ok':
    case 'success':
      return 'success'
    default:
      return 'secondary'
  }
}

onMounted(load)
</script>

<template>
  <div class="page-title">
    <div>
      <h1>Mimir rules and alerts</h1>
      <p>
        <span v-if="tenant">{{ tenant.name }}</span>
        <span v-else class="mono">{{ tenantID }}</span>
      </p>
    </div>
    <div class="mimir-actions">
      <PrimeButton icon="pi pi-arrow-left" label="Back" severity="secondary" outlined @click="router.push({ name: 'tenants' })" />
      <PrimeButton icon="pi pi-refresh" label="Refresh" :loading="loading" @click="load" />
    </div>
  </div>

  <PrimeMessage v-if="error" severity="error" :closable="false" class="mb-3">{{ error }}</PrimeMessage>

  <div v-if="loading && !doc" class="empty-state">
    <PrimeProgressSpinner style="width: 2.5rem" />
  </div>

  <template v-else-if="doc">
    <div class="stat-grid">
      <div class="stat-card">
        <div class="stat-card__label">Tenant</div>
        <div class="stat-card__value mimir-stat-value">{{ tenant?.name }}</div>
        <div class="stat-card__hint mono">{{ tenant?.id }}</div>
      </div>
      <div class="stat-card">
        <div class="stat-card__label">Mimir instance</div>
        <div class="stat-card__value mimir-stat-value">{{ instance }}</div>
        <div class="stat-card__hint">Query target selected for this tenant</div>
      </div>
      <div class="stat-card">
        <div class="stat-card__label">Rules</div>
        <div class="stat-card__value">{{ rules.length }}</div>
        <div class="stat-card__hint">{{ groups.length }} groups</div>
      </div>
      <div class="stat-card">
        <div class="stat-card__label">Alerts</div>
        <div class="stat-card__value">{{ alerts.length }}</div>
        <div class="stat-card__hint">Current upstream alert state</div>
      </div>
    </div>

    <PrimeToolbar class="mb-3">
      <template #start>
        <div class="mimir-tabs" role="tablist" aria-label="Mimir views">
          <PrimeButton label="Rules" icon="pi pi-list" :outlined="activeTab !== 'rules'" :severity="activeTab === 'rules' ? undefined : 'secondary'" @click="activeTab = 'rules'" />
          <PrimeButton label="Alerts" icon="pi pi-bell" :outlined="activeTab !== 'alerts'" :severity="activeTab === 'alerts' ? undefined : 'secondary'" @click="activeTab = 'alerts'" />
        </div>
      </template>
    </PrimeToolbar>

    <PrimeCard v-if="activeTab === 'rules'">
      <template #content>
        <PrimeDataTable :value="rules" data-key="name" size="small" striped-rows paginator :rows="15">
          <template #empty><div class="empty-state">No rules returned by Mimir.</div></template>
          <PrimeColumn header="Rule">
            <template #body="{ data }">
              <div>{{ ruleName(data) }}</div>
              <div class="stat-card__hint">{{ data.group }}</div>
            </template>
          </PrimeColumn>
          <PrimeColumn header="Type" style="width: 8rem">
            <template #body="{ data }"><PrimeTag :value="data.type || 'unknown'" severity="info" /></template>
          </PrimeColumn>
          <PrimeColumn header="State" style="width: 8rem">
            <template #body="{ data }"><PrimeTag :value="data.state || 'unknown'" :severity="tagSeverity(data.state)" /></template>
          </PrimeColumn>
          <PrimeColumn header="Health" style="width: 8rem">
            <template #body="{ data }"><PrimeTag :value="data.health || 'unknown'" :severity="tagSeverity(data.health)" /></template>
          </PrimeColumn>
          <PrimeColumn header="Query">
            <template #body="{ data }"><span class="mono mimir-code">{{ data.query || 'none' }}</span></template>
          </PrimeColumn>
          <PrimeColumn header="Labels">
            <template #body="{ data }"><span class="stat-card__hint mimir-labels">{{ labelText(data.labels) }}</span></template>
          </PrimeColumn>
        </PrimeDataTable>
      </template>
    </PrimeCard>

    <PrimeCard v-else>
      <template #content>
        <PrimeDataTable :value="alerts" data-key="fingerprint" size="small" striped-rows paginator :rows="15">
          <template #empty><div class="empty-state">No active alerts returned by Mimir.</div></template>
          <PrimeColumn header="Alert">
            <template #body="{ data }">
              <div>{{ alertName(data) }}</div>
              <div class="stat-card__hint">{{ data.annotations?.summary || data.annotations?.description || 'no annotation' }}</div>
            </template>
          </PrimeColumn>
          <PrimeColumn header="State" style="width: 8rem">
            <template #body="{ data }"><PrimeTag :value="data.state || 'unknown'" :severity="tagSeverity(data.state)" /></template>
          </PrimeColumn>
          <PrimeColumn header="Severity" style="width: 8rem">
            <template #body="{ data }"><PrimeTag :value="data.labels?.severity || 'none'" :severity="tagSeverity(data.labels?.severity)" /></template>
          </PrimeColumn>
          <PrimeColumn header="Active since" style="width: 13rem">
            <template #body="{ data }"><span class="stat-card__hint">{{ data.activeAt || data.startsAt || 'unknown' }}</span></template>
          </PrimeColumn>
          <PrimeColumn header="Labels">
            <template #body="{ data }"><span class="stat-card__hint mimir-labels">{{ labelText(data.labels) }}</span></template>
          </PrimeColumn>
        </PrimeDataTable>
      </template>
    </PrimeCard>
  </template>
</template>

<style scoped>
.mimir-actions,
.mimir-tabs {
  display: flex;
  gap: 0.5rem;
  flex-wrap: wrap;
}

.mimir-stat-value {
  font-size: 1.05rem;
  font-weight: 500;
  overflow-wrap: anywhere;
}

.mimir-code,
.mimir-labels {
  display: inline-block;
  max-width: 34rem;
  overflow-wrap: anywhere;
  white-space: normal;
}

@media (max-width: 720px) {
  .page-title {
    align-items: flex-start;
    flex-direction: column;
  }
}
</style>
