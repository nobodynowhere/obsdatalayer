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
  default_target_timeout: '30s',
  auth_limit_enabled: true,
  auth_failure_threshold: 5,
  auth_failure_window: '1m0s',
  auth_block_duration: '1m0s',
  auth_max_block_duration: '15m0s',
  auth_max_concurrent_hashes: 0,
  auth_hash_wait: '250ms',
  auth_hash_concurrency_effective: 0,
})
const loading = ref(true)
const saving = ref(false)
const error = ref('')

const logLevels = ['debug', 'info', 'warn', 'error'].map((l) => ({ label: l, value: l }))
const message = (e) => e?.response?.data?.error || e.message || 'Unexpected error'

const mib = (bytes) => (bytes / (1024 * 1024)).toFixed(1)

const boolOptions = [
  { label: 'Enabled', value: true },
  { label: 'Disabled', value: false },
]

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
          <label for="s-target-timeout">Default target timeout</label>
          <PrimeInputText id="s-target-timeout" v-model="form.default_target_timeout" class="mono" />
          <small>
            How long a fan-out target gets to answer a read when it does not set its own timeout.
            Each target is bounded separately; the whole read ends when the caller disconnects.
          </small>
        </div>

        <div class="form-field">
          <label for="s-reload">Reload interval</label>
          <PrimeInputText id="s-reload" v-model="form.reload_interval" class="mono" />
          <small>How often the gateway re-reads configuration from the database.</small>
        </div>

      </div>
    </template>
  </PrimeCard>

  <PrimeCard class="mt-3">
    <template #title>Authentication throttling</template>
    <template #content>
      <PrimeMessage severity="secondary" :closable="false" class="mb-3">
        Checking a password is deliberately expensive, so a rejected credential costs the gateway
        real CPU. These two limits bound what an unauthenticated caller can spend. The per-source
        throttle stops one client guessing; the hashing cap is what still holds when the load is
        spread across many addresses, or when the gateway sits behind a load balancer and every
        request arrives from the same one.
      </PrimeMessage>

      <div v-if="loading" class="empty-state"><PrimeProgressSpinner style="width: 2.5rem" /></div>
      <div v-else class="form-grid" style="max-width: 34rem">
        <div class="form-field">
          <label>Per-source throttle</label>
          <PrimeSelect
            v-model="form.auth_limit_enabled"
            :options="boolOptions"
            option-label="label"
            option-value="value"
          />
          <small>Blocks a source that keeps failing. The hashing cap below is unaffected.</small>
        </div>

        <template v-if="form.auth_limit_enabled">
          <div class="form-field">
            <label for="s-auth-threshold">Failures before blocking</label>
            <PrimeInputNumber input-id="s-auth-threshold" v-model="form.auth_failure_threshold" :min="1" />
            <small>Failed attempts allowed from one source within the window below.</small>
          </div>

          <div class="form-field">
            <label for="s-auth-window">Failure window</label>
            <PrimeInputText id="s-auth-window" v-model="form.auth_failure_window" class="mono" />
            <small>A source that stops failing for this long starts again with a clean slate.</small>
          </div>

          <div class="form-field">
            <label for="s-auth-block">Block duration</label>
            <PrimeInputText id="s-auth-block" v-model="form.auth_block_duration" class="mono" />
            <small>The first block. Each further block doubles it.</small>
          </div>

          <div class="form-field">
            <label for="s-auth-maxblock">Maximum block duration</label>
            <PrimeInputText id="s-auth-maxblock" v-model="form.auth_max_block_duration" class="mono" />
            <small>Caps the doubling. Must not be shorter than the block duration.</small>
          </div>
        </template>

        <div class="form-field">
          <label for="s-auth-hashes">Concurrent password hashes</label>
          <PrimeInputNumber input-id="s-auth-hashes" v-model="form.auth_max_concurrent_hashes" :min="-1" />
          <small>
            0 sizes it automatically to half the available CPUs, leaving the rest to serve traffic;
            -1 removes the cap.
            <template v-if="form.auth_hash_concurrency_effective">
              Currently in force: {{ form.auth_hash_concurrency_effective }}.
            </template>
          </small>
        </div>

        <div class="form-field">
          <label for="s-auth-wait">Hash wait</label>
          <PrimeInputText id="s-auth-wait" v-model="form.auth_hash_wait" class="mono" />
          <small>
            How long a request waits for a hashing slot before the gateway sheds it with 503. A
            short wait absorbs a burst of logins without letting a queue build under attack.
          </small>
        </div>

        <div>
          <PrimeButton label="Save settings" :loading="saving" @click="save" />
        </div>
      </div>
    </template>
  </PrimeCard>
</template>
