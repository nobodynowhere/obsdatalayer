<script setup>
import { ref, onMounted } from 'vue'
import { useToast } from 'primevue/usetoast'
import { SettingsService } from '@/services'

const svc = new SettingsService()
const toast = useToast()

const form = ref({
  max_body_bytes: 33554432,
  query_timeout: '30s',
  push_timeout: '60s',
  log_level: 'info',
  reload_interval: '30s',
})
const loading = ref(true)
const saving = ref(false)
const error = ref('')

const logLevels = ['debug', 'info', 'warn', 'error'].map((l) => ({ label: l, value: l }))
const message = (e) => e?.response?.data?.error || e.message || 'Unexpected error'

const mib = (bytes) => (bytes / (1024 * 1024)).toFixed(1)

async function load() {
  loading.value = true
  error.value = ''
  try {
    form.value = await svc.get()
  } catch (e) {
    error.value = message(e)
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  error.value = ''
  try {
    form.value = await svc.update(form.value)
    toast.add({ severity: 'success', summary: 'Settings saved', life: 3000 })
  } catch (e) {
    error.value = message(e)
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page-title">
    <div>
      <h1>Settings</h1>
      <p>Gateway-wide configuration. Every value here applies without a restart.</p>
    </div>
    <PrimeButton icon="pi pi-refresh" label="Refresh" severity="secondary" outlined @click="load" />
  </div>

  <PrimeMessage severity="secondary" :closable="false" class="mb-3">
    Listener addresses are not shown here: they come from the bootstrap file and change only on
    restart. The bootstrap file holds nothing else.
  </PrimeMessage>

  <PrimeMessage v-if="error" severity="error" :closable="false" class="mb-3">{{ error }}</PrimeMessage>

  <PrimeCard>
    <template #content>
      <div v-if="loading" class="empty-state"><PrimeProgressSpinner style="width: 2.5rem" /></div>
      <div v-else class="form-grid" style="max-width: 34rem">
        <div class="form-field">
          <label for="s-body">Maximum request body</label>
          <PrimeInputNumber input-id="s-body" v-model="form.max_body_bytes" :use-grouping="true" :min="0" suffix=" bytes" />
          <small>{{ mib(form.max_body_bytes) }} MiB. Applies to Loki, Mimir and Tempo pushes.</small>
        </div>

        <div class="form-field">
          <label for="s-query">Query timeout</label>
          <PrimeInputText id="s-query" v-model="form.query_timeout" class="mono" />
          <small>Go duration, for example 30s or 2m.</small>
        </div>

        <div class="form-field">
          <label for="s-push">Push timeout</label>
          <PrimeInputText id="s-push" v-model="form.push_timeout" class="mono" />
        </div>

        <div class="form-field">
          <label>Log level</label>
          <PrimeSelect v-model="form.log_level" :options="logLevels" option-label="label" option-value="value" />
          <small>Takes effect on save; no restart needed.</small>
        </div>

        <div class="form-field">
          <label for="s-reload">Reload interval</label>
          <PrimeInputText id="s-reload" v-model="form.reload_interval" class="mono" />
          <small>How often the gateway re-reads configuration from the database.</small>
        </div>

        <div>
          <PrimeButton label="Save settings" :loading="saving" @click="save" />
        </div>
      </div>
    </template>
  </PrimeCard>
</template>
