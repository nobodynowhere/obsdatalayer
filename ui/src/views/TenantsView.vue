<script setup>
import { ref, onMounted } from 'vue'
import { useToast } from 'primevue/usetoast'
import { useConfirm } from 'primevue/useconfirm'
import { TenantService } from '@/services'

const svc = new TenantService()
const toast = useToast()
const confirm = useConfirm()

const tenants = ref([])
const loading = ref(true)
const dialog = ref(false)
const saving = ref(false)
const editing = ref(null)
const form = ref({ id: '', name: '', grafana_id: null })
const formError = ref('')

async function load() {
  loading.value = true
  try {
    tenants.value = await svc.list()
  } catch (e) {
    toast.add({ severity: 'error', summary: 'Load failed', detail: message(e), life: 6000 })
  } finally {
    loading.value = false
  }
}

function message(e) {
  return e?.response?.data?.error || e.message || 'Unexpected error'
}

function openCreate() {
  editing.value = null
  form.value = { id: '', name: '', grafana_id: null }
  formError.value = ''
  dialog.value = true
}

function openEdit(tenant) {
  editing.value = tenant
  form.value = { id: tenant.id, name: tenant.name, grafana_id: tenant.grafana_id ?? null }
  formError.value = ''
  dialog.value = true
}

async function save() {
  saving.value = true
  formError.value = ''
  try {
    const payload = {
      name: form.value.name,
      grafana_id: form.value.grafana_id === null || form.value.grafana_id === '' ? null : Number(form.value.grafana_id),
    }
    if (editing.value) {
      await svc.update(editing.value.id, payload)
      toast.add({ severity: 'success', summary: 'Tenant updated', life: 3000 })
    } else {
      // An explicit UUID is optional; the gateway generates one when omitted.
      if (form.value.id) payload.id = form.value.id
      await svc.create(payload)
      toast.add({ severity: 'success', summary: 'Tenant created', life: 3000 })
    }
    dialog.value = false
    await load()
  } catch (e) {
    formError.value = message(e)
  } finally {
    saving.value = false
  }
}

function remove(tenant) {
  confirm.require({
    header: 'Delete tenant',
    message: `Delete "${tenant.name}"? This is refused if any grant still references it.`,
    icon: 'pi pi-exclamation-triangle',
    acceptProps: { label: 'Delete', severity: 'danger' },
    rejectProps: { label: 'Cancel', severity: 'secondary', outlined: true },
    accept: async () => {
      try {
        await svc.remove(tenant.id)
        toast.add({ severity: 'success', summary: 'Tenant deleted', life: 3000 })
        await load()
      } catch (e) {
        toast.add({ severity: 'error', summary: 'Delete refused', detail: message(e), life: 8000 })
      }
    },
  })
}

onMounted(load)
</script>

<template>
  <div class="page-title">
    <div>
      <h1>Tenants</h1>
      <p>
        The tenant ID is a UUID and is what the gateway injects upstream as
        <span class="mono">X-Scope-OrgID</span>. It is immutable once grants reference it.
      </p>
    </div>
    <PrimeButton icon="pi pi-plus" label="New tenant" @click="openCreate" />
  </div>

  <PrimeCard>
    <template #content>
      <PrimeDataTable :value="tenants" :loading="loading" data-key="id" size="small" striped-rows paginator :rows="15">
        <template #empty><div class="empty-state">No tenants defined.</div></template>
        <PrimeColumn field="name" header="Name" sortable />
        <PrimeColumn field="id" header="Tenant ID">
          <template #body="{ data }"><span class="mono">{{ data.id }}</span></template>
        </PrimeColumn>
        <PrimeColumn header="Grafana ID" sortable field="grafana_id">
          <template #body="{ data }">
            <span v-if="data.grafana_id">{{ data.grafana_id }}</span>
            <span v-else class="stat-card__hint">unassigned</span>
          </template>
        </PrimeColumn>
        <PrimeColumn header="" style="width: 7rem">
          <template #body="{ data }">
            <PrimeButton icon="pi pi-pencil" text rounded severity="secondary" v-tooltip="'Edit'" @click="openEdit(data)" />
            <PrimeButton icon="pi pi-trash" text rounded severity="danger" v-tooltip="'Delete'" @click="remove(data)" />
          </template>
        </PrimeColumn>
      </PrimeDataTable>
    </template>
  </PrimeCard>

  <PrimeDialog v-model:visible="dialog" modal :header="editing ? 'Edit tenant' : 'New tenant'" :style="{ width: '32rem' }">
    <PrimeMessage v-if="formError" severity="error" :closable="false" class="mb-3">{{ formError }}</PrimeMessage>
    <div class="form-grid">
      <div class="form-field">
        <label for="t-name">Name</label>
        <PrimeInputText id="t-name" v-model="form.name" autofocus />
        <small>Human-readable label. Safe to rename; it is not sent upstream.</small>
      </div>

      <div class="form-field" v-if="!editing">
        <label for="t-id">Tenant ID (optional)</label>
        <PrimeInputText id="t-id" v-model="form.id" placeholder="generated if left empty" class="mono" />
        <small>A UUID. Set this only when matching an ID that already exists upstream.</small>
      </div>
      <div class="form-field" v-else>
        <label>Tenant ID</label>
        <span class="mono">{{ form.id }}</span>
        <small>Immutable — grants and instances reference it.</small>
      </div>

      <div class="form-field">
        <label for="t-grafana">Grafana ID (optional)</label>
        <PrimeInputNumber input-id="t-grafana" v-model="form.grafana_id" :use-grouping="false" :min="1" show-buttons />
        <small>Reserved for the future Grafana traffic proxy. Not used for routing today.</small>
      </div>
    </div>
    <template #footer>
      <PrimeButton label="Cancel" severity="secondary" outlined @click="dialog = false" />
      <PrimeButton label="Save" :loading="saving" :disabled="!form.name" @click="save" />
    </template>
  </PrimeDialog>
</template>
