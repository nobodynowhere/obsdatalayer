<script setup>
import { ref, onMounted } from 'vue'
import { useToast } from 'primevue/usetoast'
import { useConfirm } from 'primevue/useconfirm'
import { RoleService, TenantService } from '@/services'
import GrantsEditor from '@/components/GrantsEditor.vue'

const svc = new RoleService()
const tenantSvc = new TenantService()
const toast = useToast()
const confirm = useConfirm()

const roles = ref([])
const tenants = ref([])
const loading = ref(true)
const dialog = ref(false)
const saving = ref(false)
const editing = ref(null)
const form = ref({ name: '', description: '', grants: [] })
const formError = ref('')

const message = (e) => e?.response?.data?.error || e.message || 'Unexpected error'
const tenantName = (id) => tenants.value.find((t) => t.id === id)?.name ?? id.slice(0, 8) + '…'

async function load() {
  loading.value = true
  try {
    const [r, t] = await Promise.all([svc.list(), tenantSvc.list()])
    roles.value = r
    tenants.value = t
  } catch (e) {
    toast.add({ severity: 'error', summary: 'Load failed', detail: message(e), life: 6000 })
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = null
  form.value = { name: '', description: '', grants: [] }
  formError.value = ''
  dialog.value = true
}

function openEdit(role) {
  editing.value = role
  form.value = {
    name: role.name,
    description: role.description ?? '',
    grants: (role.grants ?? []).map((g) => ({ ...g, tenant_ids: [...(g.tenant_ids ?? [])] })),
  }
  formError.value = ''
  dialog.value = true
}

async function save() {
  saving.value = true
  formError.value = ''
  try {
    if (editing.value) {
      // Only grants are mutable on an existing role.
      await svc.setGrants(editing.value.name, form.value.grants)
      toast.add({ severity: 'success', summary: 'Role updated', life: 3000 })
    } else {
      await svc.create(form.value)
      toast.add({ severity: 'success', summary: 'Role created', life: 3000 })
    }
    dialog.value = false
    await load()
  } catch (e) {
    formError.value = message(e)
  } finally {
    saving.value = false
  }
}

function remove(role) {
  confirm.require({
    header: 'Delete role',
    message: `Delete "${role.name}"? Members lose the access it grants immediately.`,
    icon: 'pi pi-exclamation-triangle',
    acceptProps: { label: 'Delete', severity: 'danger' },
    rejectProps: { label: 'Cancel', severity: 'secondary', outlined: true },
    accept: async () => {
      try {
        await svc.remove(role.name)
        toast.add({ severity: 'success', summary: 'Role deleted', life: 3000 })
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
      <h1>Roles</h1>
      <p>Reusable bundles of grants. Assign them to users on the Users page.</p>
    </div>
    <PrimeButton icon="pi pi-plus" label="New role" @click="openCreate" />
  </div>

  <PrimeCard>
    <template #content>
      <PrimeDataTable :value="roles" :loading="loading" data-key="name" size="small" striped-rows>
        <template #empty><div class="empty-state">No roles defined.</div></template>
        <PrimeColumn field="name" header="Role" sortable>
          <template #body="{ data }">
            <strong>{{ data.name }}</strong>
            <div v-if="data.description" class="stat-card__hint">{{ data.description }}</div>
          </template>
        </PrimeColumn>
        <PrimeColumn header="Grants">
          <template #body="{ data }">
            <div v-if="!data.grants?.length" class="stat-card__hint">none</div>
            <div v-for="(g, i) in data.grants" :key="i" style="margin-bottom: 0.2rem">
              <PrimeTag
                :value="`${g.backend}:${g.action}`"
                :severity="g.backend === 'admin' ? 'danger' : 'info'"
              />
              <span v-if="g.tenant_ids?.length" class="stat-card__hint">
                {{ g.tenant_ids.map(tenantName).join(', ') }}
              </span>
            </div>
          </template>
        </PrimeColumn>
        <PrimeColumn header="Members">
          <template #body="{ data }">
            <span v-if="data.members?.length">{{ data.members.join(', ') }}</span>
            <span v-else class="stat-card__hint">none</span>
          </template>
        </PrimeColumn>
        <PrimeColumn header="" style="width: 7rem">
          <template #body="{ data }">
            <PrimeButton icon="pi pi-pencil" text rounded severity="secondary" v-tooltip="'Edit grants'" @click="openEdit(data)" />
            <PrimeButton icon="pi pi-trash" text rounded severity="danger" v-tooltip="'Delete'" @click="remove(data)" />
          </template>
        </PrimeColumn>
      </PrimeDataTable>
    </template>
  </PrimeCard>

  <PrimeDialog v-model:visible="dialog" modal :header="editing ? `Edit ${editing.name}` : 'New role'" :style="{ width: '46rem' }">
    <PrimeMessage v-if="formError" severity="error" :closable="false" class="mb-3">{{ formError }}</PrimeMessage>
    <div class="form-grid">
      <template v-if="!editing">
        <div class="form-field">
          <label for="r-name">Name</label>
          <PrimeInputText id="r-name" v-model="form.name" autofocus />
        </div>
        <div class="form-field">
          <label for="r-desc">Description</label>
          <PrimeInputText id="r-desc" v-model="form.description" />
        </div>
      </template>
      <GrantsEditor v-model="form.grants" :tenants="tenants" />
    </div>
    <template #footer>
      <PrimeButton label="Cancel" severity="secondary" outlined @click="dialog = false" />
      <PrimeButton label="Save" :loading="saving" :disabled="!editing && !form.name" @click="save" />
    </template>
  </PrimeDialog>
</template>
