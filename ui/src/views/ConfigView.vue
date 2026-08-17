<script setup>
import { ref, onMounted } from 'vue'
import { useToast } from 'primevue/usetoast'
import { ConfigService } from '@/services'

const svc = new ConfigService()
const toast = useToast()

const yaml = ref('')
const loading = ref(true)
const reloading = ref(false)
const error = ref('')

const message = (e) => e?.response?.data?.error || e.message || 'Unexpected error'

async function load() {
  loading.value = true
  error.value = ''
  try {
    const data = await svc.get()
    yaml.value = typeof data === 'string' ? data : JSON.stringify(data, null, 2)
  } catch (e) {
    error.value = message(e)
  } finally {
    loading.value = false
  }
}

async function reload() {
  reloading.value = true
  try {
    const res = await svc.reload()
    toast.add({
      severity: 'success',
      summary: 'Configuration reloaded',
      detail: `${res.instances} instance(s) active`,
      life: 4000,
    })
    await load()
  } catch (e) {
    toast.add({ severity: 'error', summary: 'Reload failed', detail: message(e), life: 8000 })
  } finally {
    reloading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page-title">
    <div>
      <h1>Configuration</h1>
      <p>
        The live configuration read from the database. Upstream
        <span class="mono">basic_auth</span> values are redacted.
      </p>
    </div>
    <div style="display: flex; gap: 0.5rem">
      <PrimeButton icon="pi pi-refresh" label="Refresh" severity="secondary" outlined @click="load" />
      <PrimeButton icon="pi pi-sync" label="Reload from database" :loading="reloading" @click="reload" />
    </div>
  </div>

  <PrimeMessage severity="secondary" :closable="false" class="mb-3">
    Listener addresses are not shown here: they come from the bootstrap file and change only on
    restart. Everything below hot-reloads.
  </PrimeMessage>

  <PrimeMessage v-if="error" severity="error" :closable="false">{{ error }}</PrimeMessage>

  <PrimeCard v-else>
    <template #content>
      <div v-if="loading" class="empty-state"><PrimeProgressSpinner style="width: 2.5rem" /></div>
      <pre v-else class="code-block">{{ yaml }}</pre>
    </template>
  </PrimeCard>
</template>
