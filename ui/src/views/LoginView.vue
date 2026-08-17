<script setup>
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores'

const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const username = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  busy.value = true
  const result = await auth.login(username.value, password.value)
  busy.value = false
  if (!result.ok) {
    error.value = result.message
    return
  }
  router.push(route.query.next || { name: 'dashboard' })
}
</script>

<template>
  <div class="login-shell">
    <form class="login-card" @submit.prevent="submit">
      <img src="/DellTech_Logo_mobile.svg" alt="Dell Technologies" class="login-card__logo" />
      <h1>Observability Gateway</h1>
      <p>Sign in with a gateway account that has admin access.</p>

      <PrimeMessage v-if="error" severity="error" :closable="false" class="mb-3">
        {{ error }}
      </PrimeMessage>

      <div class="form-field" style="min-width: 0; margin-bottom: 1rem">
        <label for="username">Username</label>
        <PrimeInputText
          id="username"
          v-model="username"
          autocomplete="username"
          autofocus
          required
        />
      </div>

      <div class="form-field" style="min-width: 0; margin-bottom: 1.5rem">
        <label for="password">Password</label>
        <PrimePassword
          input-id="password"
          v-model="password"
          :feedback="false"
          toggle-mask
          fluid
          autocomplete="current-password"
          required
        />
      </div>

      <PrimeButton
        type="submit"
        label="Sign in"
        :loading="busy"
        fluid
        :disabled="!username || !password"
      />
    </form>
  </div>
</template>
