<script setup>
import { ref, computed, watch, onMounted } from 'vue'
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
const statusDialog = ref(false)
const statusLoading = ref(false)
const statusResult = ref(null)
const statusError = ref('')
const statusRequest = ref(null)

const backends = [
  { label: 'loki', value: 'loki' },
  { label: 'mimir', value: 'mimir' },
  { label: 'tempo', value: 'tempo' },
]
// "Fan out" named the replication case and hid the surface-routing one, which
// is the case target groups exist for: a Tempo instance addressing 4318 and
// 3200 is not replicating anything. The label has to cover both, because it is
// the only route to the per-target Group control.
const targetModes = [
  { label: 'Single URL — one origin serves every surface', value: 'single' },
  { label: 'Several targets — replicas, or separate ingest and query surfaces', value: 'fanout' },
]
const fanOutModes = [
  { label: 'any — succeed if one target accepts', value: 'any' },
  { label: 'all — every target must accept', value: 'all' },
]
const groupLabels = {
  '': 'Legacy fallback',
  push: 'Generic ingest',
  query: 'Query API',
  otlp_http: 'OTLP HTTP',
  jaeger: 'Jaeger HTTP',
  zipkin: 'Zipkin',
}
// Which groups a backend accepts comes from the gateway
// (GET /api/operational-endpoints), not from a copy here. The rule lives in
// config.groupsByBackend, and a second copy in the SPA would drift the moment a
// backend gains or loses a receiver surface -- offering a group the gateway
// then rejects on save, or hiding one it would accept.
const groupsForBackend = (backend) => operationalCatalog.value.target_groups?.[backend] ?? []
const filterModes = [
  { label: 'allowlist', value: 'allowlist' },
  { label: 'denylist', value: 'denylist' },
]
// The endpoint catalog is served by the gateway (GET /api/operational-endpoints)
// rather than restated here, so a button can only ever offer an alias the
// gateway actually registers, under the grant it actually requires.
const operationalCatalog = ref({ mounts: {}, endpoints: {}, target_groups: {} })

// Aliases are snake_case identifiers; the button label is derived rather than
// carried, so the backend table stays free of presentation.
const checkLabel = (alias) => {
  const words = alias.replace(/_/g, ' ')
  return words.charAt(0).toUpperCase() + words.slice(1)
}

const maxStatusBodyChars = 100000

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
const statusTitle = computed(() => {
  if (!statusRequest.value) return 'Target status'
  return `${statusRequest.value.instance} — ${statusRequest.value.label}`
})
const statusTargets = computed(() => statusResult.value?.targets ?? [])

function severityFor(target) {
  if (target.error) return 'danger'
  const status = target.status ?? 0
  if (status >= 200 && status < 300) return 'success'
  if (status >= 500 || status === 0) return 'danger'
  return 'warn'
}

function statusLabel(target) {
  if (target.error) return target.error
  return `${target.status}${target.duration_ms != null ? ` · ${target.duration_ms} ms` : ''}`
}

// The gateway already caps each body; this second cap is only about what the
// browser is asked to lay out, since the dialog shows every target at once.
function bodyFor(target) {
  const body = String(target.body ?? '')
  const note = target.truncated ? '\n\n... truncated by the gateway' : ''
  if (body.length <= maxStatusBodyChars) return body + note
  return `${body.slice(0, maxStatusBodyChars)}\n\n... truncated in the UI after ${maxStatusBodyChars.toLocaleString()} characters`
}

// Tempo has no label policy, so the form hides those controls.
const isTempo = computed(() => form.value.backend === 'tempo')

async function load() {
  loading.value = true
  try {
    const [i, t, catalog] = await Promise.all([
      svc.list(),
      tenantSvc.list(),
      // A failure here costs the status buttons, not the page, so it does not
      // take the instance list down with it.
      svc.operationalCatalog().catch(() => ({ mounts: {}, endpoints: {}, target_groups: {} })),
    ])
    instances.value = i
    tenants.value = t
    operationalCatalog.value = catalog
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
    push_urls: (inst.push_urls ?? []).map((t) => ({ timeout_seconds: 0, ...t, group: t.group ?? '' })),
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
      group: t.group || undefined,
      basic_auth: t.basic_auth || undefined,
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
  if (strandedTargets.value.length) {
    const detail = strandedTargets.value
      .map((t) => `target ${t.index + 1} (${t.url || 'no URL'}) is set to ${groupLabels[t.group] ?? t.group}`)
      .join('; ')
    formError.value =
      `${form.value.backend} does not serve those target groups: ${detail}. ` +
      'Choose a group this backend serves before saving.'
    return
  }
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

function displayTargets(inst) {
  const targets = inst.push_urls?.length
    ? inst.push_urls
    : [{ url: inst.url, skip_tls_verify: inst.skip_tls_verify }]
  return targets.map((target, index) => ({ ...target, rank: index + 1 }))
}

function targetChecks(inst) {
  return operationalCatalog.value.endpoints?.[inst.backend] ?? []
}

// One request per instance, not per target: the gateway asks every target and
// answers with all of them, so the dialog can show which replica disagrees.
async function openTargetStatus(inst, check) {
  statusDialog.value = true
  statusLoading.value = true
  statusResult.value = null
  statusError.value = ''
  statusRequest.value = { instance: inst.name, backend: inst.backend, label: checkLabel(check.alias) }
  try {
    const mount = operationalCatalog.value.mounts?.[inst.backend]
    statusResult.value = await svc.targetStatus(mount, inst.name, check.alias)
  } catch (e) {
    statusError.value = message(e)
  } finally {
    statusLoading.value = false
  }
}

const newTarget = (url = '') => ({
  url,
  group: '',
  basic_auth: '',
  skip_tls_verify: false,
  timeout_seconds: 0,
})

const addTarget = () => form.value.push_urls.push(newTarget())

// Converting an existing single-URL instance to several targets is the main
// reason anyone touches the mode control -- it is how a Tempo instance grows a
// separate receiver and query surface. Starting that from an empty list meant
// the URL already configured was silently dropped on save, because the payload
// carries push_urls instead of url in this mode. Seed the first target from it.
watch(
  () => form.value.mode,
  (mode, previous) => {
    if (mode !== 'fanout' || previous !== 'single') return
    if (form.value.push_urls.length || !form.value.url) return
    form.value.push_urls.push(newTarget(form.value.url))
  },
)
const removeTarget = (i) => form.value.push_urls.splice(i, 1)

// Order is meaningful within a group, and only there. Every target in an ingest
// group receives every write, so order is not preference for them; reads walk a
// single group and take the first that answers, so within that one group the
// first target is the one queried. Moving a target past one in another group
// therefore changes nothing, which is why readGroup below exists: the tooltip
// has to say which of the two a given row is.
// Mirrors config.GetTargets(TargetGroupQuery): the query group when the
// instance declares one, then the generic push group, then legacy ungrouped
// targets. Null when the instance declares only receiver groups and so has
// nothing to answer a read with.
// The options offered depend on the backend chosen above, so switching an
// instance from tempo to mimir must not leave a jaeger target selected -- the
// backend would refuse the save with a message about a group it no longer
// serves. Clear those rather than let the operator discover it on submit.
const targetGroupOptions = computed(() =>
  ['', ...groupsForBackend(form.value.backend)].map((value) => ({
    label: groupLabels[value] ?? value,
    value,
  })),
)

// Changing the backend can strand a group the new backend does not serve --
// jaeger on an instance moved from tempo to mimir. These are reported rather
// than cleared: clearing looks like a tidy-up but silently changes where the
// target's traffic goes, from a named receiver to the legacy every-surface
// fallback. The operator picks the replacement.
const strandedTargets = computed(() => {
  const allowed = groupsForBackend(form.value.backend)
  if (!allowed.length) return []
  return form.value.push_urls
    .map((t, i) => ({ index: i, group: t.group, url: t.url }))
    .filter((t) => t.group && !allowed.includes(t.group))
})

const readGroup = computed(() => {
  const groups = form.value.push_urls.map((t) => t.group || '')
  for (const candidate of ['query', 'push', '']) {
    if (groups.includes(candidate)) return candidate
  }
  return null
})

const groupLabel = (group) => groupLabels[group || ''] ?? group

// Position among the targets sharing this one's group, which is the only
// position that decides anything.
const rankInGroup = (index) => {
  const group = form.value.push_urls[index]?.group || ''
  return form.value.push_urls.slice(0, index + 1).filter((t) => (t.group || '') === group).length
}

const rankTitle = (index) => {
  const group = form.value.push_urls[index]?.group || ''
  const label = groupLabel(group)
  const rank = rankInGroup(index)
  if (group !== readGroup.value) {
    return `${label} target ${rank} — every target in this group receives every write`
  }
  return rank === 1 ? `${label} — first tried for reads` : `${label} — read fallback ${rank - 1}`
}

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
            <div
              v-for="target in displayTargets(data)"
              :key="`${data.name}-${target.rank}`"
              class="instance-target"
            >
              <div class="instance-target__url mono" :title="target.url">
                <span class="target-rank">{{ target.rank }}</span>
                <span>{{ target.url }}</span>
              </div>
              <div v-if="target.skip_tls_verify" class="stat-card__hint">TLS verification skipped</div>
              <div v-if="target.group" class="stat-card__hint">group: {{ target.group }}</div>
            </div>
            <div v-if="targetChecks(data).length" class="instance-target__actions">
              <PrimeButton
                v-for="check in targetChecks(data)"
                :key="check.alias"
                :label="checkLabel(check.alias)"
                :title="`Asks every target; needs a ${data.backend}:${check.action} grant on the data plane`"
                size="small"
                severity="secondary"
                outlined
                @click="openTargetStatus(data, check)"
              />
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
    v-model:visible="statusDialog"
    modal
    :header="statusTitle"
    :style="{ width: '58rem' }"
  >
    <div v-if="statusLoading" class="empty-state">
      <PrimeProgressSpinner style="width: 2.5rem" />
    </div>
    <PrimeMessage v-else-if="statusError" severity="error" :closable="false">
      {{ statusError }}
    </PrimeMessage>
    <template v-else-if="statusResult">
      <div v-for="target in statusTargets" :key="target.rank" class="target-status">
        <div class="target-status__meta">
          <div>
            <div class="stat-card__label">Target {{ target.rank }}</div>
            <div class="mono">{{ target.url || '—' }}</div>
          </div>
          <div>
            <div class="stat-card__label">Result</div>
            <PrimeTag :severity="severityFor(target)" :value="statusLabel(target)" />
          </div>
          <div>
            <div class="stat-card__label">Content type</div>
            <div class="mono">{{ target.content_type || 'not set' }}</div>
          </div>
        </div>
        <pre v-if="!target.error" class="code-block target-status__body">{{ bodyFor(target) }}</pre>
      </div>
    </template>
  </PrimeDialog>

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
          :options="targetModes"
          option-label="label"
          option-value="value"
        />
        <small>
          Choose several targets to replicate writes across backends, to give reads a fallback, or
          to address a backend whose ingest and query APIs listen on different ports — Tempo serves
          its OTLP, Jaeger and Zipkin receivers separately from its HTTP API, so reaching both needs
          one target per surface.
        </small>
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
          <small>
            How the responses from one group's targets are aggregated. It decides nothing when a
            group holds a single target, which is the usual shape when the groups address different
            surfaces rather than replicas.
          </small>
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
            <span class="target-rank" :title="rankTitle(i)">{{ i + 1 }}</span>
            <PrimeInputText v-model="t.url" placeholder="http://target.local" class="mono" />
            <PrimeSelect
              v-model="t.group"
              :options="targetGroupOptions"
              option-label="label"
              option-value="value"
              placeholder="Group"
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
          <PrimeMessage v-if="strandedTargets.length" severity="error" :closable="false" style="margin-top: 0.5rem">
            {{ form.backend }} does not serve
            {{ strandedTargets.map((t) => groupLabels[t.group] ?? t.group).join(', ') }}.
            Reassign
            {{ strandedTargets.map((t) => `target ${t.index + 1}`).join(', ') }}
            to a group this backend serves — the routing a receiver group selects has no equivalent
            here, so it is not something the form can pick for you.
          </PrimeMessage>
          <small v-if="form.push_urls.length > 1" style="display: block; margin-top: 0.5rem">
            Every target in an ingest group receives every write. Reads use one group only — the
            Query API group when you define one, otherwise generic ingest and then legacy fallback
            targets — and try that group's targets in order, so its first target is the one normally
            queried and the rest cover for it. Order therefore matters within a group and nowhere
            else: moving a target past one in a different group changes nothing. The timeout is how
            long each target gets to answer a read; leave it at 0 to use the gateway default.
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
