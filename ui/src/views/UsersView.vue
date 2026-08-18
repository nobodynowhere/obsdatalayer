<script setup>
import { ref, onMounted } from 'vue'
import { useToast } from 'primevue/usetoast'
import { useConfirm } from 'primevue/useconfirm'
import { UserService, RoleService, TenantService } from '@/services'
import GrantsEditor from '@/components/GrantsEditor.vue'

const svc = new UserService()
const roleSvc = new RoleService()
const tenantSvc = new TenantService()
const toast = useToast()
const confirm = useConfirm()

const users = ref([])
const roles = ref([])
const tenants = ref([])
const loading = ref(true)

const createDialog = ref(false)
const editDialog = ref(false)
const passwordDialog = ref(false)
const saving = ref(false)
const formError = ref('')

const newUser = ref({ name: '', password: '', roles: [] })
const editing = ref(null)
const editForm = ref({ roles: [], grants: [] })
const passwordFor = ref(null)
const newPassword = ref('')

const message = (e) => e?.response?.data?.error || e.message || 'Unexpected error'
const tenantName = (id) => tenants.value.find((t) => t.id === id)?.name ?? id.slice(0, 8) + '…'
const readPolicy = (grant) => grant.read_label_selector?.trim()

async function load() {
  loading.value = true
  try {
    const [u, r, t] = await Promise.all([svc.list(), roleSvc.list(), tenantSvc.list()])
    users.value = u
    roles.value = r
    tenants.value = t
  } catch (e) {
    toast.add({ severity: 'error', summary: 'Load failed', detail: message(e), life: 6000 })
  } finally {
    loading.value = false
  }
}

function openCreate() {
  newUser.value = { name: '', password: '', roles: [] }
  formError.value = ''
  createDialog.value = true
}

async function create() {
  saving.value = true
  formError.value = ''
  try {
    await svc.create(newUser.value)
    toast.add({ severity: 'success', summary: 'User created', life: 3000 })
    createDialog.value = false
    await load()
  } catch (e) {
    formError.value = message(e)
  } finally {
    saving.value = false
  }
}

function openEdit(user) {
  editing.value = user
  editForm.value = {
    roles: [...(user.roles ?? [])],
    grants: (user.grants ?? []).map((g) => ({
      ...g,
      tenant_ids: [...(g.tenant_ids ?? [])],
      read_label_selector: g.read_label_selector ?? '',
    })),
  }
  formError.value = ''
  editDialog.value = true
}

async function saveEdit() {
  saving.value = true
  formError.value = ''
  try {
    // Roles and direct grants are separate endpoints; send both.
    await svc.setRoles(editing.value.name, editForm.value.roles)
    await svc.setGrants(editing.value.name, editForm.value.grants)
    toast.add({ severity: 'success', summary: 'User updated', life: 3000 })
    editDialog.value = false
    await load()
  } catch (e) {
    formError.value = message(e)
  } finally {
    saving.value = false
  }
}

function openPassword(user) {
  passwordFor.value = user
  newPassword.value = ''
  formError.value = ''
  passwordDialog.value = true
}

async function savePassword() {
  saving.value = true
  formError.value = ''
  try {
    await svc.setPassword(passwordFor.value.name, newPassword.value)
    toast.add({ severity: 'success', summary: 'Password changed', life: 3000 })
    passwordDialog.value = false
  } catch (e) {
    formError.value = message(e)
  } finally {
    saving.value = false
  }
}

function remove(user) {
  confirm.require({
    header: 'Delete user',
    message: `Delete "${user.name}"? Deleting the last admin is refused.`,
    icon: 'pi pi-exclamation-triangle',
    acceptProps: { label: 'Delete', severity: 'danger' },
    rejectProps: { label: 'Cancel', severity: 'secondary', outlined: true },
    accept: async () => {
      try {
        await svc.remove(user.name)
        toast.add({ severity: 'success', summary: 'User deleted', life: 3000 })
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
      <h1>Users</h1>
      <p>Gateway accounts. Passwords are stored as bcrypt hashes and are never displayed.</p>
    </div>
    <PrimeButton icon="pi pi-plus" label="New user" @click="openCreate" />
  </div>

  <PrimeCard>
    <template #content>
      <PrimeDataTable :value="users" :loading="loading" data-key="name" size="small" striped-rows>
        <template #empty><div class="empty-state">No users defined.</div></template>
        <PrimeColumn field="name" header="User" sortable>
          <template #body="{ data }">
            <strong>{{ data.name }}</strong>
            <PrimeTag v-if="data.admin" value="admin" severity="danger" style="margin-left: 0.5rem" />
          </template>
        </PrimeColumn>
        <PrimeColumn header="Roles">
          <template #body="{ data }">
            <PrimeTag v-for="r in data.roles" :key="r" :value="r" severity="secondary" style="margin-right: 0.25rem" />
            <span v-if="!data.roles?.length" class="stat-card__hint">none</span>
          </template>
        </PrimeColumn>
        <PrimeColumn header="Direct grants">
          <template #body="{ data }">
            <div v-if="!data.grants?.length" class="stat-card__hint">none</div>
            <div v-for="(g, i) in data.grants" :key="i">
              <PrimeTag :value="`${g.backend}:${g.action}`" :severity="g.backend === 'admin' ? 'danger' : 'info'" />
              <span v-if="g.tenant_ids?.length" class="stat-card__hint">
                {{ g.tenant_ids.map(tenantName).join(', ') }}
              </span>
              <div v-if="readPolicy(g)" class="stat-card__hint">{{ readPolicy(g) }}</div>
            </div>
          </template>
        </PrimeColumn>
        <PrimeColumn header="" style="width: 10rem">
          <template #body="{ data }">
            <PrimeButton icon="pi pi-pencil" text rounded severity="secondary" v-tooltip="'Roles and grants'" @click="openEdit(data)" />
            <PrimeButton icon="pi pi-key" text rounded severity="secondary" v-tooltip="'Change password'" @click="openPassword(data)" />
            <PrimeButton icon="pi pi-trash" text rounded severity="danger" v-tooltip="'Delete'" @click="remove(data)" />
          </template>
        </PrimeColumn>
      </PrimeDataTable>
    </template>
  </PrimeCard>

  <PrimeDialog v-model:visible="createDialog" modal header="New user" :style="{ width: '32rem' }">
    <PrimeMessage v-if="formError" severity="error" :closable="false" class="mb-3">{{ formError }}</PrimeMessage>
    <div class="form-grid">
      <div class="form-field">
        <label for="u-name">Username</label>
        <PrimeInputText id="u-name" v-model="newUser.name" autofocus />
      </div>
      <div class="form-field">
        <label for="u-pass">Password</label>
        <PrimePassword input-id="u-pass" v-model="newUser.password" toggle-mask fluid :feedback="false" />
        <small>At least 12 characters.</small>
      </div>
      <div class="form-field">
        <label>Roles</label>
        <PrimeMultiSelect
          v-model="newUser.roles"
          :options="roles"
          option-label="name"
          option-value="name"
          display="chip"
          placeholder="Select roles"
        />
      </div>
    </div>
    <template #footer>
      <PrimeButton label="Cancel" severity="secondary" outlined @click="createDialog = false" />
      <PrimeButton
        label="Create"
        :loading="saving"
        :disabled="!newUser.name || newUser.password.length < 12"
        @click="create"
      />
    </template>
  </PrimeDialog>

  <PrimeDialog v-model:visible="editDialog" modal :header="`Edit ${editing?.name}`" :style="{ width: '46rem' }">
    <PrimeMessage v-if="formError" severity="error" :closable="false" class="mb-3">{{ formError }}</PrimeMessage>
    <div class="form-grid">
      <div class="form-field">
        <label>Roles</label>
        <PrimeMultiSelect
          v-model="editForm.roles"
          :options="roles"
          option-label="name"
          option-value="name"
          display="chip"
          placeholder="Select roles"
        />
        <small>Removing admin from the last admin account is refused.</small>
      </div>
      <GrantsEditor v-model="editForm.grants" :tenants="tenants" />
    </div>
    <template #footer>
      <PrimeButton label="Cancel" severity="secondary" outlined @click="editDialog = false" />
      <PrimeButton label="Save" :loading="saving" @click="saveEdit" />
    </template>
  </PrimeDialog>

  <PrimeDialog v-model:visible="passwordDialog" modal :header="`Change password for ${passwordFor?.name}`" :style="{ width: '30rem' }">
    <PrimeMessage v-if="formError" severity="error" :closable="false" class="mb-3">{{ formError }}</PrimeMessage>
    <div class="form-grid">
      <div class="form-field">
        <label for="u-newpass">New password</label>
        <PrimePassword input-id="u-newpass" v-model="newPassword" toggle-mask fluid :feedback="false" autofocus />
        <small>At least 12 characters.</small>
      </div>
    </div>
    <template #footer>
      <PrimeButton label="Cancel" severity="secondary" outlined @click="passwordDialog = false" />
      <PrimeButton label="Change" :loading="saving" :disabled="newPassword.length < 12" @click="savePassword" />
    </template>
  </PrimeDialog>
</template>
