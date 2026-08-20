<script setup>
import { computed, watch } from 'vue'

const props = defineProps({
  modelValue: { type: Array, required: true },
  tenants: { type: Array, default: () => [] },
  enforceSingleWriteTenant: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue'])

const backends = [
  { label: 'All backends (*)', value: '*' },
  { label: 'loki', value: 'loki' },
  { label: 'mimir', value: 'mimir' },
  { label: 'tempo', value: 'tempo' },
  { label: 'Admin plane', value: 'admin' },
]

const dataActions = [
  { label: 'read + write (*)', value: '*' },
  { label: 'read', value: 'read' },
  { label: 'write', value: 'write' },
]

const controlActionLabels = {
  'rules:read': { label: 'rules read', value: 'rules:read' },
  'rules:write': { label: 'rules write', value: 'rules:write' },
  'alerts:read': { label: 'alerts read', value: 'alerts:read' },
  'alerts:write': { label: 'alerts write', value: 'alerts:write' },
}

// Which control actions each backend actually exposes. Mirrors
// controlActionBackends in internal/auth: Mimir runs a ruler and an
// alertmanager, Loki runs a ruler only and forwards alerts to an external
// Alertmanager, so it has no alert configuration to write. Tempo has neither.
const controlActionsByBackend = {
  mimir: ['rules:read', 'rules:write', 'alerts:read', 'alerts:write'],
  loki: ['rules:read', 'rules:write', 'alerts:read'],
}

const tenantOptions = computed(() =>
  props.tenants.map((t) => ({ label: `${t.name} — ${t.id.slice(0, 8)}…`, value: t.id })),
)

function normalizeGrant(grant) {
  if (grant.backend === 'admin') {
    return { ...grant, action: 'access', tenant_ids: [], read_label_selector: '' }
  }
  const next = { ...grant }
  if (next.action === 'access') {
    next.action = 'read'
  }
  if (isControlAction(next.action) && !controlActionAllowed(next.backend, next.action)) {
    next.action = 'read'
  }
  if (usesSingleTenant(next)) {
    next.tenant_ids = (next.tenant_ids ?? []).slice(0, 1)
  }
  if (!isMimirRead(next)) {
    next.read_label_selector = ''
  }
  return next
}

function normalizeGrants(grants, preferredWriteTenant = '') {
  const next = grants.map(normalizeGrant)
  if (!props.enforceSingleWriteTenant) {
    return next
  }
  const writeTenant =
    preferredWriteTenant ||
    next.find((grant) => isWriteCapable(grant) && grant.tenant_ids?.[0])?.tenant_ids?.[0] ||
    ''
  if (!writeTenant) {
    return next
  }
  return next.map((grant) => (isWriteCapable(grant) ? { ...grant, tenant_ids: [writeTenant] } : grant))
}

function grantsChanged(a, b) {
  return JSON.stringify(a) !== JSON.stringify(b)
}

function update(index, patch) {
  const patchedGrant = normalizeGrant({ ...props.modelValue[index], ...patch })
  const preferredWriteTenant = isWriteCapable(patchedGrant) ? patchedGrant.tenant_ids?.[0] ?? '' : ''
  const next = normalizeGrants(
    props.modelValue.map((g, i) => (i === index ? patchedGrant : g)),
    preferredWriteTenant,
  )
  emit('update:modelValue', next)
}

function setBackend(index, backend) {
  update(index, { backend })
}

function setReadLabelSelector(index, value) {
  update(index, { read_label_selector: String(value ?? '').trim() })
}

function isMimirRead(grant) {
  return grant.backend === 'mimir' && grant.action === 'read'
}

function isWriteCapable(grant) {
  return grant.backend !== 'admin' && ['write', '*', 'rules:write', 'alerts:write'].includes(grant.action)
}

function usesSingleTenant(grant) {
  return ['write', '*', 'rules:write', 'alerts:write'].includes(grant.action)
}

function isControlAction(action) {
  return Object.prototype.hasOwnProperty.call(controlActionLabels, action)
}

function controlActionAllowed(backend, action) {
  return (controlActionsByBackend[backend] ?? []).includes(action)
}

function actionOptions(grant) {
  const control = controlActionsByBackend[grant.backend] ?? []
  return control.length ? [...dataActions, ...control.map((a) => controlActionLabels[a])] : dataActions
}

function add() {
  emit(
    'update:modelValue',
    normalizeGrants([...props.modelValue, { backend: 'loki', action: 'read', tenant_ids: [], read_label_selector: '' }]),
  )
}

function remove(index) {
  emit('update:modelValue', normalizeGrants(props.modelValue.filter((_, i) => i !== index)))
}

watch(
  () => props.modelValue,
  (grants) => {
    const next = normalizeGrants(grants)
    if (grantsChanged(next, grants)) {
      emit('update:modelValue', next)
    }
  },
  { immediate: true, deep: true },
)
</script>

<template>
  <div class="form-field">
    <label>Grants</label>
    <small>
      A grant allows an action on a backend for a set of tenants. A wildcard backend covers every
      data backend but never the admin plane, which must be granted explicitly.
    </small>

    <div v-if="!modelValue.length" class="empty-state" style="padding: 1rem">
      No grants. This subject can reach nothing.
    </div>

    <div v-for="(grant, i) in modelValue" :key="i" class="grant-row" style="margin-top: 0.5rem">
      <PrimeSelect
        :model-value="grant.backend"
        :options="backends"
        option-label="label"
        option-value="value"
        @update:model-value="setBackend(i, $event)"
      />
      <PrimeSelect
        v-if="grant.backend !== 'admin'"
        :model-value="grant.action"
        :options="actionOptions(grant)"
        option-label="label"
        option-value="value"
        @update:model-value="update(i, { action: $event })"
      />
      <PrimeInputText v-else model-value="access" disabled />

      <PrimeMultiSelect
        v-if="grant.backend !== 'admin' && !usesSingleTenant(grant)"
        :model-value="grant.tenant_ids"
        :options="tenantOptions"
        option-label="label"
        option-value="value"
        display="chip"
        filter
        placeholder="Select tenants"
        @update:model-value="update(i, { tenant_ids: $event })"
      />
      <PrimeSelect
        v-else-if="grant.backend !== 'admin'"
        :model-value="grant.tenant_ids?.[0] ?? null"
        :options="tenantOptions"
        option-label="label"
        option-value="value"
        placeholder="Select tenant"
        filter
        @update:model-value="update(i, { tenant_ids: $event ? [$event] : [] })"
      />
      <span v-else class="stat-card__hint" style="align-self: center">no tenants</span>

      <PrimeButton icon="pi pi-times" text rounded severity="danger" @click="remove(i)" />

      <div v-if="isMimirRead(grant)" class="grant-row__policy">
        <label>Read label policy</label>
        <PrimeInputText
          :model-value="grant.read_label_selector ?? ''"
          placeholder='{cluster="prod"}'
          @update:model-value="setReadLabelSelector(i, $event)"
        />
      </div>
    </div>

    <div style="margin-top: 0.75rem">
      <PrimeButton icon="pi pi-plus" label="Add grant" size="small" severity="secondary" outlined @click="add" />
    </div>
  </div>
</template>
