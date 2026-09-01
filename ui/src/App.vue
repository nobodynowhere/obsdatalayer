<script setup>
import { ref, computed, onMounted, onBeforeUnmount, watchEffect } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useAuthStore } from '@/stores'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const { username, loggedIn } = storeToRefs(auth)

const darkMode = ref(localStorage.getItem('obsgateway-dark') === 'true')
const bare = computed(() => route.meta.layout === 'bare')

const nav = [
  { to: '/', label: 'Overview', icon: 'dds__icon--home' },
  { to: '/instances', label: 'Instances', icon: 'dds__icon--device-server' },
  { to: '/tenants', label: 'Tenants', icon: 'dds__icon--cloud' },
  { to: '/roles', label: 'Roles', icon: 'dds__icon--shield' },
  { to: '/users', label: 'Users', icon: 'dds__icon--user-group' },
  { to: '/settings', label: 'Settings', icon: 'dds__icon--gear' },
  { to: '/config', label: 'Configuration', icon: 'dds__icon--view-list' },
]

function toggleDark() {
  darkMode.value = !darkMode.value
}

// Keyboard shortcuts follow the reference UI: Ctrl/Cmd + Alt + <key>.
// Ctrl/Cmd + Alt + 1 toggles the theme.
const isTypingTarget = (el) => {
  if (!(el instanceof HTMLElement)) return false
  const tag = el.tagName
  const editable = el.getAttribute('contenteditable')
  return (
    tag === 'INPUT' ||
    tag === 'TEXTAREA' ||
    editable === '' ||
    editable === 'true' ||
    el.closest('[role="textbox"]') !== null
  )
}

const onKeyDown = (e) => {
  // Never steal a keystroke that is being typed into a field.
  if (isTypingTarget(e.target)) return

  const k = e.key.toLowerCase()
  const primary = e.ctrlKey || e.metaKey
  const alt = e.altKey

  if (primary && alt) {
    if (k === '1') {
      e.preventDefault()
      toggleDark()
      return
    }
  }
}

watchEffect(() => {
  document.documentElement.classList.toggle('my-app-dark', darkMode.value)
  localStorage.setItem('obsgateway-dark', String(darkMode.value))
})

onMounted(() => {
  window.addEventListener('keydown', onKeyDown)

  // DDS's SideNav enhancer is optional here: the nav is a flat list, so it
  // renders correctly as plain markup if the module is unavailable.
  import('@dds/components/esm/index.js')
    .then(({ SideNav }) => {
      const el = document.getElementById('gateway-sidenav')
      if (el && SideNav) SideNav(el, { fixed: false, expand: true })
    })
    .catch(() => {})
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeyDown)
})
</script>

<template>
  <PrimeToast position="bottom-right" />
  <PrimeConfirmDialog />

  <RouterView v-if="bare" />

  <div v-else class="dds__template--productivity">
    <header class="app-header">
      <div class="app-header__brand">
        <img src="/header-logo.svg" alt="Observability Gateway" class="app-header__logo" />
        <span>Observability Gateway</span>
      </div>
      <div class="app-header__actions">
        <PrimeButton
          :icon="darkMode ? 'pi pi-sun' : 'pi pi-moon'"
          severity="secondary"
          text
          rounded
          :aria-label="darkMode ? 'Switch to light theme' : 'Switch to dark theme'"
          v-tooltip.bottom="(darkMode ? 'Light theme' : 'Dark theme') + ' (Ctrl+Alt+1)'"
          @click="toggleDark"
        />
        <PrimeButton
          v-if="loggedIn"
          icon="pi pi-sign-out"
          :label="username || 'Sign out'"
          severity="secondary"
          text
          @click="auth.logout()"
        />
      </div>
    </header>

    <nav
      id="gateway-sidenav"
      class="dds__side-nav__wrapper"
      data-dds="side-nav"
      aria-label="Primary"
    >
      <section class="dds__side-nav">
        <ul class="dds__side-nav__menu">
          <li v-for="item in nav" :key="item.to" class="dds__side-nav__item">
            <router-link :to="item.to" class="nav-link">
              <span class="dds__icon dds__side-nav__icon" :class="item.icon" aria-hidden="true" />
              <span>{{ item.label }}</span>
            </router-link>
          </li>
        </ul>
      </section>
    </nav>

    <main class="app-main">
      <RouterView />
    </main>
  </div>
</template>
