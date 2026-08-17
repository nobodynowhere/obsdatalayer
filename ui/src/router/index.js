import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores'

const router = createRouter({
  // Matches ui.Prefix in internal/ui/ui.go; the Go handler falls back to
  // index.html for unknown paths so deep links and refreshes work.
  history: createWebHistory('/ui/'),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/LoginView.vue'),
      meta: { public: true, layout: 'bare' },
    },
    {
      path: '/',
      name: 'dashboard',
      component: () => import('@/views/DashboardView.vue'),
      meta: { title: 'Overview' },
    },
    {
      path: '/instances',
      name: 'instances',
      component: () => import('@/views/InstancesView.vue'),
      meta: { title: 'Instances' },
    },
    {
      path: '/tenants',
      name: 'tenants',
      component: () => import('@/views/TenantsView.vue'),
      meta: { title: 'Tenants' },
    },
    {
      path: '/users',
      name: 'users',
      component: () => import('@/views/UsersView.vue'),
      meta: { title: 'Users' },
    },
    {
      path: '/roles',
      name: 'roles',
      component: () => import('@/views/RolesView.vue'),
      meta: { title: 'Roles' },
    },
    {
      path: '/settings',
      name: 'settings',
      component: () => import('@/views/SettingsView.vue'),
      meta: { title: 'Settings' },
    },
    {
      path: '/config',
      name: 'config',
      component: () => import('@/views/ConfigView.vue'),
      meta: { title: 'Configuration' },
    },
    { path: '/:pathMatch(.*)*', redirect: { name: 'dashboard' } },
  ],
})

router.beforeEach((to) => {
  const auth = useAuthStore()
  if (!to.meta.public && !auth.loggedIn) {
    return { name: 'login', query: to.fullPath === '/' ? undefined : { next: to.fullPath } }
  }
  if (to.name === 'login' && auth.loggedIn) {
    return { name: 'dashboard' }
  }
  return true
})

export default router
