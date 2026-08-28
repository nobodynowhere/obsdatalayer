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
  read_header_timeout: '5s',
  idle_timeout: '2m0s',
  upstream_max_idle_conns: 10000,
  upstream_max_idle_conns_per_host: 10000,
  upstream_max_conns_per_host: 0,
  upstream_idle_conn_timeout: '1m30s',
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
  metrics_unauthenticated: false,
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

// Phrased as what the endpoint requires rather than as the field name: an
// "Enabled/Disabled" pair against metrics_unauthenticated reads as a double
// negative, and getting it backwards here exposes the backend URLs.
const metricsAuthOptions = [
  { label: 'Require authentication', value: false },
  { label: 'Allow unauthenticated scrapes', value: true },
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
      <p>Gateway-wide configuration. Most values here apply without a restart.</p>
    </div>
    <PrimeButton icon="pi pi-refresh" label="Refresh" severity="secondary" outlined @click="load" />
  </div>

  <PrimeMessage severity="secondary" :closable="false" class="mb-3">
    Listener addresses are not shown here: they come from the bootstrap file and change only on
    restart. Listener timeout changes below are also applied on restart.
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
          <label for="s-read-header-timeout">Read header timeout</label>
          <PrimeInputText id="s-read-header-timeout" v-model="form.read_header_timeout" class="mono" />
          <small>Startup setting. Bounds how long a new client gets to send request headers.</small>
        </div>

        <div class="form-field">
          <label for="s-idle-timeout">Idle client timeout</label>
          <PrimeInputText id="s-idle-timeout" v-model="form.idle_timeout" class="mono" />
          <small>Startup setting. Closes quiet keep-alive client connections after this long.</small>
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

        <div class="form-field">
          <label for="s-upstream-idle">Upstream idle connections</label>
          <PrimeInputNumber
            input-id="s-upstream-idle"
            v-model="form.upstream_max_idle_conns"
            :use-grouping="true"
            :min="0"
          />
          <small>Total idle upstream connections kept for reuse. 0 restores the default.</small>
        </div>

        <div class="form-field">
          <label for="s-upstream-idle-host">Upstream idle connections per host</label>
          <PrimeInputNumber
            input-id="s-upstream-idle-host"
            v-model="form.upstream_max_idle_conns_per_host"
            :use-grouping="true"
            :min="0"
          />
          <small>Idle upstream connections kept per backend host. 0 restores the default.</small>
        </div>

        <div class="form-field">
          <label for="s-upstream-active-host">Upstream active connections per host</label>
          <PrimeInputNumber
            input-id="s-upstream-active-host"
            v-model="form.upstream_max_conns_per_host"
            :use-grouping="true"
            :min="0"
          />
          <small>0 means unlimited active upstream connections per host.</small>
        </div>

        <div class="form-field">
          <label for="s-upstream-idle-timeout">Upstream idle timeout</label>
          <PrimeInputText id="s-upstream-idle-timeout" v-model="form.upstream_idle_conn_timeout" class="mono" />
          <small>How long reusable upstream connections can sit idle before being closed.</small>
        </div>

        <div class="form-field">
          <label>Metrics endpoint</label>
          <PrimeSelect
            v-model="form.metrics_unauthenticated"
            :options="metricsAuthOptions"
            option-label="label"
            option-value="value"
          />
          <small>
            Whether <span class="mono">GET /metrics</span> on the admin port requires an admin
            credential. Turn it off only for a Prometheus that cannot hold one.
          </small>
          <PrimeMessage
            v-if="form.metrics_unauthenticated"
            severity="warn"
            :closable="false"
            class="mt-2"
          >
            Anyone who can reach the admin port can now read the metrics, which name every
            configured instance and every upstream target URL. Make sure that port is on loopback
            or firewalled. Nothing else is exposed &mdash; the rest of the admin API, health check
            included, still requires an admin grant.
          </PrimeMessage>
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
