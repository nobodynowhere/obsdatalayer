import { ref, computed } from 'vue'
import { defineStore } from 'pinia'
import axios from 'axios'
import router from '@/router'

// The admin API authenticates with HTTP Basic on every request, so the encoded
// credential is what we hold onto. It lives in sessionStorage: it is cleared
// when the tab closes, and never touches localStorage or a cookie.
export const useAuthStore = defineStore(
  'auth',
  () => {
    const credential = ref(null) // base64("user:pass")
    const username = ref(null)
    const isAdmin = ref(false)

    const loggedIn = computed(() => credential.value !== null)

    function reset() {
      credential.value = null
      username.value = null
      isAdmin.value = false
    }

    async function login(user, password) {
      const encoded = btoa(`${user}:${password}`)
      try {
        // whoami is the cheapest authenticated endpoint; a 200 proves both the
        // credential and the admin grant.
        const res = await axios.get('/whoami', {
          headers: { Authorization: `Basic ${encoded}` },
        })
        credential.value = encoded
        username.value = res.data?.name ?? res.data?.username ?? user
        isAdmin.value = res.data?.admin ?? true
        return { ok: true }
      } catch (err) {
        reset()
        const status = err?.response?.status
        if (status === 401) return { ok: false, message: 'Incorrect username or password.' }
        if (status === 403) return { ok: false, message: 'This account does not have admin access.' }
        return { ok: false, message: 'Could not reach the gateway admin API.' }
      }
    }

    function logout() {
      reset()
      router.push({ name: 'login' })
    }

    return { credential, username, isAdmin, loggedIn, login, logout, reset }
  },
  {
    persist: {
      storage: sessionStorage,
      key: 'obsgateway-auth',
      pick: ['credential', 'username', 'isAdmin'],
    },
  },
)

// Attach the credential to every request and treat a 401 as a lost session.
// Called once after Pinia is installed.
export function installAuthInterceptors() {
  axios.interceptors.request.use((config) => {
    const auth = useAuthStore()
    if (!config.headers['Content-Type']) {
      config.headers['Content-Type'] = 'application/json'
    }
    if (auth.credential) {
      config.headers.Authorization = `Basic ${auth.credential}`
    }
    return config
  })

  axios.interceptors.response.use(
    (response) => response,
    (error) => {
      if (error?.response?.status === 401) {
        const auth = useAuthStore()
        // Don't bounce the login screen's own probe back to itself.
        if (!String(error.config?.url || '').includes('/whoami') || auth.loggedIn) {
          auth.reset()
          if (router.currentRoute.value.name !== 'login') {
            router.push({ name: 'login' })
          }
        }
      }
      return Promise.reject(error)
    },
  )
}
