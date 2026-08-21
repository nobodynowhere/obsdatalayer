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

// API keys are bearer credentials for a user. The secret is shown once, at
// creation, and is not recoverable afterwards.
const keysDialog = ref(false)
const keysFor = ref(null)
const keys = ref([])
const keysLoading = ref(false)
const newKeyLabel = ref('')
const newKeyExpires = ref('')
const issuedSecret = ref('')
const copied = ref(false)

const message = (e) => e?.response?.data?.error || e.message || 'Unexpected error'
const tenantName = (id) => tenants.value.find((t) => t.id === id)?.name ?? id.slice(0, 8) + '…'
const readPolicy = (grant) => grant.read_label_selector?.trim()

async function openKeys(user) {
  keysFor.value = user
  keys.value = []
  newKeyLabel.value = ''
  newKeyExpires.value = ''
  issuedSecret.value = ''
  copied.value = false
  formError.value = ''
  keysDialog.value = true
  await loadKeys()
}

async function loadKeys() {
  keysLoading.value = true
  try {
    keys.value = await svc.listApiKeys(keysFor.value.name)
  } catch (e) {
    formError.value = message(e)
  } finally {
    keysLoading.value = false
  }
}

async function issueKey() {
  saving.value = true
  formError.value = ''
  try {
    const expires = newKeyExpires.value ? new Date(newKeyExpires.value).toISOString() : ''
    const created = await svc.createApiKey(keysFor.value.name, newKeyLabel.value.trim(), expires)
    issuedSecret.value = created.secret
    newKeyLabel.value = ''
    newKeyExpires.value = ''
    await loadKeys()
  } catch (e) {
    formError.value = message(e)
  } finally {
    saving.value = false
  }
}

async function copySecret() {
  try {
    await navigator.clipboard.writeText(issuedSecret.value)
    copied.value = true
  } catch {
    // Clipboard access can be refused; the secret is on screen to copy by hand.
    copied.value = false
  }
}

function revokeKey(key) {
  confirm.require({
    message: `Revoke key "${key.label}"? Anything using it stops working immediately.`,
    header: 'Revoke API key',
    icon: 'pi pi-exclamation-triangle',
    acceptClass: 'p-button-danger',
    accept: async () => {
      try {
        await svc.deleteApiKey(keysFor.value.name, key.id)
        toast.add({ severity: 'success', summary: 'Key revoked', life: 3000 })
        await loadKeys()
      } catch (e) {
        toast.add({ severity: 'error', summary: message(e), life: 5000 })
      }
    },
  })
}

const fmtDate = (v) => (v ? new Date(v).toLocaleString() : '—')

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
            <PrimeButton icon="pi pi-ticket" text rounded severity="secondary" v-tooltip="'API keys'" @click="openKeys(data)" />
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

  <PrimeDialog
    v-model:visible="keysDialog"
    modal
    :header="`API keys for ${keysFor?.name}`"
    :style="{ width: '46rem' }"
  >
    <PrimeMessage v-if="formError" severity="error" :closable="false" class="mb-3">{{ formError }}</PrimeMessage>

    <PrimeMessage severity="secondary" :closable="false" class="mb-3">
      A key is a bearer credential for this user and carries exactly the same grants. Send it as
      <span class="mono">Authorization: Bearer &lt;key&gt;</span> on the data plane. The admin API
      still requires a password.
    </PrimeMessage>

    <div v-if="issuedSecret" class="issued-key mb-3">
      <div class="issued-key__label">
        Copy this now — it is shown once and cannot be retrieved again.
      </div>
      <code class="issued-key__value mono">{{ issuedSecret }}</code>
      <PrimeButton
        :icon="copied ? 'pi pi-check' : 'pi pi-copy'"
        :label="copied ? 'Copied' : 'Copy'"
        size="small"
        severity="secondary"
        outlined
        @click="copySecret"
      />
    </div>

    <div class="form-grid mb-3">
      <div class="form-field">
        <label for="k-label">New key label</label>
        <PrimeInputText id="k-label" v-model="newKeyLabel" placeholder="promtail-prod" />
        <small>Names the key so it can be identified and retired later.</small>
      </div>
      <div class="form-field">
        <label for="k-expires">Expires (optional)</label>
        <PrimeInputText id="k-expires" v-model="newKeyExpires" placeholder="2027-01-31" class="mono" />
        <small>Leave empty for a key that never expires, which suits unattended shippers.</small>
      </div>
      <div>
        <PrimeButton
          icon="pi pi-plus"
          label="Issue key"
          size="small"
          :loading="saving"
          :disabled="!newKeyLabel.trim()"
          @click="issueKey"
        />
      </div>
    </div>

    <div v-if="keysLoading" class="empty-state"><PrimeProgressSpinner style="width: 2rem" /></div>
    <PrimeDataTable v-else :value="keys" size="small" data-key="id">
      <template #empty>
        <div class="empty-state">No keys yet.</div>
      </template>
      <PrimeColumn field="label" header="Label" />
      <PrimeColumn header="Key">
        <template #body="{ data }">
          <span class="mono">obsgw_{{ data.handle }}_…</span>
        </template>
      </PrimeColumn>
      <PrimeColumn header="Created">
        <template #body="{ data }">{{ fmtDate(data.created_at) }}</template>
      </PrimeColumn>
      <PrimeColumn header="Last used">
        <template #body="{ data }">
          <span :class="{ 'text-muted': !data.last_used_at }">{{ fmtDate(data.last_used_at) }}</span>
        </template>
      </PrimeColumn>
      <PrimeColumn header="Expires">
        <template #body="{ data }">{{ fmtDate(data.expires_at) }}</template>
      </PrimeColumn>
      <PrimeColumn style="width: 4rem">
        <template #body="{ data }">
          <PrimeButton icon="pi pi-trash" text rounded severity="danger" v-tooltip="'Revoke'" @click="revokeKey(data)" />
        </template>
      </PrimeColumn>
    </PrimeDataTable>

    <template #footer>
      <PrimeButton label="Close" severity="secondary" outlined @click="keysDialog = false" />
    </template>
  </PrimeDialog>
</template>
