<script setup>
import { ref, computed, onMounted } from 'vue'
import { useToast } from 'primevue/usetoast'
import { useConfirm } from 'primevue/useconfirm'
import { InstanceService, TenantService } from '@/services'

const svc = new InstanceService()
const tenantSvc = new TenantService()
const toast = useToast()
const confirm = useConfirm()

const instances = ref([])
const tenants = ref([])
const loading = ref(true)
const dialog = ref(false)
const saving = ref(false)
const editing = ref(null)
const formError = ref('')

const backends = [
  { label: 'loki', value: 'loki' },
  { label: 'mimir', value: 'mimir' },
  { label: 'tempo', value: 'tempo' },
]
const fanOutModes = [
  { label: 'any — succeed if one target accepts', value: 'any' },
  { label: 'all — every target must accept', value: 'all' },
]
const filterModes = [
  { label: 'allowlist', value: 'allowlist' },
  { label: 'denylist', value: 'denylist' },
]

const blank = () => ({
  name: '',
  backend: 'loki',
  mode: 'single', // single url, or fan-out push targets
  url: '',
  push_urls: [],
  fan_out_mode: 'any',
  basic_auth: '',
  tenant_id: '',
  skip_tls_verify: false,
  labelsEnabled: false,
  filterEnabled: false,
  filter: { mode: 'allowlist', names: [] },
  injectPairs: [],
})
const form = ref(blank())

const message = (e) => e?.response?.data?.error || e.message || 'Unexpected error'
const tenantName = (id) => tenants.value.find((t) => t.id === id)?.name ?? id
const tenantOptions = computed(() => tenants.value.map((t) => ({ label: t.name, value: t.id })))

// Tempo has no label policy, so the form hides those controls.
const isTempo = computed(() => form.value.backend === 'tempo')

async function load() {
  loading.value = true
  try {
    const [i, t] = await Promise.all([svc.list(), tenantSvc.list()])
    instances.value = i
    tenants.value = t
  } catch (e) {
    toast.add({ severity: 'error', summary: 'Load failed', detail: message(e), life: 6000 })
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = null
  form.value = blank()
  formError.value = ''
  dialog.value = true
}

function openEdit(inst) {
  editing.value = inst
  form.value = {
    name: inst.name,
    backend: inst.backend,
    mode: inst.push_urls?.length ? 'fanout' : 'single',
    url: inst.url ?? '',
    push_urls: (inst.push_urls ?? []).map((t) => ({ timeout_seconds: 0, ...t })),
    fan_out_mode: inst.fan_out_mode || 'any',
    basic_auth: inst.basic_auth ?? '',
    tenant_id: inst.tenant_id ?? '',
    skip_tls_verify: !!inst.skip_tls_verify,
    labelsEnabled: !!inst.labels,
    filterEnabled: !!inst.labels?.filter,
    filter: inst.labels?.filter
      ? { mode: inst.labels.filter.mode, names: [...(inst.labels.filter.names ?? [])] }
      : { mode: 'allowlist', names: [] },
    injectPairs: Object.entries(inst.labels?.inject ?? {}).map(([key, value]) => ({ key, value })),
  }
  formError.value = ''
  dialog.value = true
}

// Build the API document from the form, dropping the UI-only helper fields.
function toPayload() {
  const f = form.value
  const doc = {
    name: f.name,
    backend: f.backend,
    basic_auth: f.basic_auth || undefined,
    tenant_id: f.tenant_id || undefined,
    skip_tls_verify: f.skip_tls_verify || undefined,
  }
  if (f.mode === 'single') {
    doc.url = f.url
  } else {
    doc.push_urls = f.push_urls.map((t) => ({
      url: t.url,
      basic_auth: t.basic_auth || undefined,
      tenant_id: t.tenant_id || undefined,
      skip_tls_verify: t.skip_tls_verify || undefined,
      // 0 means "use the gateway default", which the API omits.
      timeout_seconds: Number(t.timeout_seconds) > 0 ? Number(t.timeout_seconds) : undefined,
    }))
    doc.fan_out_mode = f.fan_out_mode
  }
  if (!isTempo.value && f.labelsEnabled) {
    const labels = {}
    if (f.filterEnabled && f.filter.names.length) {
      labels.filter = { mode: f.filter.mode, names: f.filter.names }
    }
    const inject = {}
    for (const p of f.injectPairs) if (p.key) inject[p.key] = p.value
    if (Object.keys(inject).length) labels.inject = inject
    if (labels.filter || labels.inject) doc.labels = labels
  }
  return doc
}

async function save() {
  saving.value = true
  formError.value = ''
  try {
    if (editing.value) {
      await svc.update(editing.value.name, toPayload())
      toast.add({ severity: 'success', summary: 'Instance updated', life: 3000 })
    } else {
      await svc.create(toPayload())
      toast.add({ severity: 'success', summary: 'Instance created', life: 3000 })
    }
    dialog.value = false
    await load()
  } catch (e) {
    formError.value = message(e)
  } finally {
    saving.value = false
  }
}

function remove(inst) {
  confirm.require({
    header: 'Delete instance',
    message: `Delete "${inst.name}"? Clients pushing or querying it will start receiving 404s.`,
    icon: 'pi pi-exclamation-triangle',
    acceptProps: { label: 'Delete', severity: 'danger' },
    rejectProps: { label: 'Cancel', severity: 'secondary', outlined: true },
    accept: async () => {
      try {
        await svc.remove(inst.name)
        toast.add({ severity: 'success', summary: 'Instance deleted', life: 3000 })
        await load()
      } catch (e) {
        toast.add({ severity: 'error', summary: 'Delete failed', detail: message(e), life: 8000 })
      }
    },
  })
}

const addTarget = () =>
  form.value.push_urls.push({ url: '', basic_auth: '', tenant_id: '', skip_tls_verify: false, timeout_seconds: 0 })
const removeTarget = (i) => form.value.push_urls.splice(i, 1)

// Order is meaningful: writes go to every target, but reads try them in this
// order and prefer the first, so moving a target up makes it the one queried.
const moveTarget = (from, to) => {
  const list = form.value.push_urls
  if (to < 0 || to >= list.length) return
  const [t] = list.splice(from, 1)
  list.splice(to, 0, t)
}
const addInject = () => form.value.injectPairs.push({ key: '', value: '' })
const removeInject = (i) => form.value.injectPairs.splice(i, 1)

onMounted(load)
</script>

<template>
  <div class="page-title">
    <div>
      <h1>Instances</h1>
      <p>
        Clients reach backends at <span class="mono">/api/&lt;backend&gt;/…</span>;
        Grafana Mimir reads use <span class="mono">/prometheus/…</span>.
      </p>
    </div>
    <PrimeButton icon="pi pi-plus" label="New instance" @click="openCreate" />
  </div>

  <PrimeMessage v-if="!loading && !instances.length" severity="info" :closable="false" class="mb-3">
    No instances configured yet. Add one to start accepting telemetry.
  </PrimeMessage>

  <PrimeCard>
    <template #content>
      <PrimeDataTable :value="instances" :loading="loading" data-key="name" size="small" striped-rows>
        <template #empty><div class="empty-state">No instances configured.</div></template>
        <PrimeColumn field="name" header="Instance" sortable>
          <template #body="{ data }"><strong>{{ data.name }}</strong></template>
        </PrimeColumn>
        <PrimeColumn field="backend" header="Backend" sortable>
          <template #body="{ data }"><PrimeTag :value="data.backend" severity="info" /></template>
        </PrimeColumn>
        <PrimeColumn header="Targets">
          <template #body="{ data }">
            <div v-if="data.url" class="mono">{{ data.url }}</div>
            <div v-if="data.skip_tls_verify" class="stat-card__hint">TLS verification skipped</div>
            <div v-for="(t, i) in data.push_urls" :key="i" class="mono">
              {{ t.url }}<span v-if="t.skip_tls_verify" class="stat-card__hint"> TLS verification skipped</span>
            </div>
            <div v-if="data.push_urls?.length" class="stat-card__hint">
              fan-out: {{ data.fan_out_mode }}
            </div>
          </template>
        </PrimeColumn>
        <PrimeColumn header="Tenant">
          <template #body="{ data }">
            <span v-if="data.tenant_id">{{ tenantName(data.tenant_id) }}</span>
            <span v-else class="stat-card__hint">from caller grant</span>
          </template>
        </PrimeColumn>
        <PrimeColumn header="Labels">
          <template #body="{ data }">
            <span v-if="!data.labels" class="stat-card__hint">none</span>
            <template v-else>
              <PrimeTag
                v-if="data.labels.filter"
                :value="`${data.labels.filter.mode} (${data.labels.filter.names?.length ?? 0})`"
                severity="secondary"
              />
              <PrimeTag
                v-if="data.labels.inject"
                :value="`inject ${Object.keys(data.labels.inject).length}`"
                severity="secondary"
              />
            </template>
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

  <PrimeDialog
    v-model:visible="dialog"
    modal
    :header="editing ? `Edit ${editing.name}` : 'New instance'"
    :style="{ width: '52rem' }"
  >
    <PrimeMessage v-if="formError" severity="error" :closable="false" class="mb-3">{{ formError }}</PrimeMessage>

    <div class="form-grid">
      <div class="form-field">
        <label for="i-name">Name</label>
        <PrimeInputText id="i-name" v-model="form.name" autofocus />
        <small>Operator-facing name. Letters, digits, dashes and underscores.</small>
      </div>

      <div class="form-field">
        <label>Backend</label>
        <PrimeSelect v-model="form.backend" :options="backends" option-label="label" option-value="value" />
      </div>

      <div class="form-field">
        <label>Targets</label>
        <PrimeSelect
          v-model="form.mode"
          :options="[
            { label: 'Single URL', value: 'single' },
            { label: 'Fan out to several targets', value: 'fanout' },
          ]"
          option-label="label"
          option-value="value"
        />
      </div>

      <div class="form-field" v-if="form.mode === 'single'">
        <label for="i-url">URL</label>
        <PrimeInputText id="i-url" v-model="form.url" :placeholder="`http://${form.backend}.local:3100`" class="mono" />
        <small>Must include an http:// or https:// scheme and a host.</small>
      </div>

      <template v-else>
        <div class="form-field">
          <label>Fan-out mode</label>
          <PrimeSelect v-model="form.fan_out_mode" :options="fanOutModes" option-label="label" option-value="value" />
        </div>
        <div class="form-field">
          <label>Push targets</label>
          <div v-if="!form.push_urls.length" class="empty-state" style="padding: 1rem">
            No targets yet.
          </div>
          <div
            v-for="(t, i) in form.push_urls"
            :key="i"
            class="target-row"
            style="margin-top: 0.5rem"
          >
            <span class="target-rank" :title="i === 0 ? 'Preferred for reads' : `Read fallback ${i}`">{{ i + 1 }}</span>
            <PrimeInputText v-model="t.url" placeholder="http://target.local" class="mono" />
            <PrimeSelect
              v-model="t.tenant_id"
              :options="tenantOptions"
              option-label="label"
              option-value="value"
              placeholder="Tenant (optional)"
              show-clear
            />
            <PrimeSelect
              v-model="t.skip_tls_verify"
              :options="[
                { label: 'Use instance setting', value: false },
                { label: 'Skip TLS verify', value: true },
              ]"
              option-label="label"
              option-value="value"
            />
            <PrimeInputNumber
              v-model="t.timeout_seconds"
              :min="0"
              suffix=" s"
              placeholder="default"
              :input-style="{ width: '100%' }"
              aria-label="Per-target timeout in seconds"
              title="Seconds this target gets to answer a read. 0 uses the gateway default."
            />
            <PrimeButton
              icon="pi pi-arrow-up"
              text
              rounded
              severity="secondary"
              :disabled="i === 0"
              aria-label="Move target up"
              @click="moveTarget(i, i - 1)"
            />
            <PrimeButton
              icon="pi pi-arrow-down"
              text
              rounded
              severity="secondary"
              :disabled="i === form.push_urls.length - 1"
              aria-label="Move target down"
              @click="moveTarget(i, i + 1)"
            />
            <PrimeButton icon="pi pi-times" text rounded severity="danger" @click="removeTarget(i)" />
          </div>
          <small v-if="form.push_urls.length > 1" style="display: block; margin-top: 0.5rem">
            Every target receives every write. Reads try them in this order, so target 1 is the one
            normally queried and the rest are fallbacks used when it cannot answer. The timeout is
            how long each target gets to answer a read; leave it at 0 to use the gateway default.
          </small>
          <div style="margin-top: 0.75rem">
            <PrimeButton icon="pi pi-plus" label="Add target" size="small" severity="secondary" outlined @click="addTarget" />
          </div>
        </div>
      </template>

      <div class="form-field">
        <label for="i-auth">Upstream basic auth</label>
        <PrimeInputText id="i-auth" v-model="form.basic_auth" placeholder="user:password" class="mono" />
        <small>
          Credentials the gateway presents to the backend. Shown as
          <span class="mono">&lt;redacted&gt;</span> once saved; leave the mask in place to keep it.
        </small>
      </div>

      <div class="form-field">
        <label>Default tenant</label>
        <PrimeSelect
          v-model="form.tenant_id"
          :options="tenantOptions"
          option-label="label"
          option-value="value"
          placeholder="None — use the caller's grant"
          show-clear
        />
        <small>Only used when a request carries no resolved tenant of its own.</small>
      </div>

      <div class="form-field">
        <label>Upstream TLS verification</label>
        <PrimeSelect
          v-model="form.skip_tls_verify"
          :options="[
            { label: 'Verify certificates', value: false },
            { label: 'Skip certificate verification', value: true },
          ]"
          option-label="label"
          option-value="value"
        />
        <small>Only affects HTTPS upstream URLs for this instance.</small>
      </div>

      <template v-if="!isTempo">
        <div class="form-field">
          <label>Label policy</label>
          <PrimeSelect
            v-model="form.labelsEnabled"
            :options="[
              { label: 'None', value: false },
              { label: 'Filter and/or inject labels on push', value: true },
            ]"
            option-label="label"
            option-value="value"
          />
        </div>

        <template v-if="form.labelsEnabled">
          <div class="form-field">
            <label>Filter</label>
            <PrimeSelect
              v-model="form.filterEnabled"
              :options="[
                { label: 'No filtering', value: false },
                { label: 'Filter label names', value: true },
              ]"
              option-label="label"
              option-value="value"
            />
          </div>
          <template v-if="form.filterEnabled">
            <div class="form-field">
              <label>Filter mode</label>
              <PrimeSelect v-model="form.filter.mode" :options="filterModes" option-label="label" option-value="value" />
            </div>
            <div class="form-field">
              <label>Label names</label>
              <PrimeMultiSelect
                v-model="form.filter.names"
                :options="form.filter.names.map((n) => ({ label: n, value: n }))"
                option-label="label"
                option-value="value"
                display="chip"
                filter
                editable
                placeholder="Type a label name and press enter"
              />
              <small>Applied to labels on push only; queries are not filtered.</small>
            </div>
          </template>

          <div class="form-field">
            <label>Injected labels</label>
            <div v-for="(p, i) in form.injectPairs" :key="i" class="grant-row" style="margin-top: 0.5rem">
              <PrimeInputText v-model="p.key" placeholder="label" style="grid-column: span 2" />
              <PrimeInputText v-model="p.value" placeholder="value" />
              <PrimeButton icon="pi pi-times" text rounded severity="danger" @click="removeInject(i)" />
            </div>
            <div style="margin-top: 0.75rem">
              <PrimeButton icon="pi pi-plus" label="Add label" size="small" severity="secondary" outlined @click="addInject" />
            </div>
          </div>
        </template>
      </template>
    </div>

    <template #footer>
      <PrimeButton label="Cancel" severity="secondary" outlined @click="dialog = false" />
      <PrimeButton label="Save" :loading="saving" :disabled="!form.name" @click="save" />
    </template>
  </PrimeDialog>
</template>
