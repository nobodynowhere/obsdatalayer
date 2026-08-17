<script setup>
import { computed } from 'vue'

const props = defineProps({
  modelValue: { type: Array, required: true },
  tenants: { type: Array, default: () => [] },
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

const tenantOptions = computed(() =>
  props.tenants.map((t) => ({ label: `${t.name} — ${t.id.slice(0, 8)}…`, value: t.id })),
)

function update(index, patch) {
  const next = props.modelValue.map((g, i) => (i === index ? { ...g, ...patch } : g))
  emit('update:modelValue', next)
}

function setBackend(index, backend) {
  // The admin plane takes a fixed action and carries no tenants.
  if (backend === 'admin') {
    update(index, { backend, action: 'access', tenant_ids: [] })
    return
  }
  const current = props.modelValue[index]
  const action = current.action === 'access' ? 'read' : current.action
  update(index, { backend, action })
}

function add() {
  emit('update:modelValue', [...props.modelValue, { backend: 'loki', action: 'read', tenant_ids: [] }])
}

function remove(index) {
  emit('update:modelValue', props.modelValue.filter((_, i) => i !== index))
}
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
        :options="dataActions"
        option-label="label"
        option-value="value"
        @update:model-value="update(i, { action: $event })"
      />
      <PrimeInputText v-else model-value="access" disabled />

      <PrimeMultiSelect
        v-if="grant.backend !== 'admin'"
        :model-value="grant.tenant_ids"
        :options="tenantOptions"
        option-label="label"
        option-value="value"
        display="chip"
        filter
        placeholder="Select tenants"
        @update:model-value="update(i, { tenant_ids: $event })"
      />
      <span v-else class="stat-card__hint" style="align-self: center">no tenants</span>

      <PrimeButton icon="pi pi-times" text rounded severity="danger" @click="remove(i)" />
    </div>

    <div style="margin-top: 0.75rem">
      <PrimeButton icon="pi pi-plus" label="Add grant" size="small" severity="secondary" outlined @click="add" />
    </div>
  </div>
</template>
