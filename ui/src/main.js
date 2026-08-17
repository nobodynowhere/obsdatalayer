// Vue core
import { createApp } from 'vue'
import { createPinia } from 'pinia'
import piniaPluginPersistedState from 'pinia-plugin-persistedstate'

// Styles: DDS chrome, PrimeVue icons, then app overrides.
import './assets/main.scss'
import 'primeicons/primeicons.css'
import './assets/app.css'

// PrimeVue
import PrimeVue from 'primevue/config'
import ConfirmationService from 'primevue/confirmationservice'
import ToastService from 'primevue/toastservice'
import Tooltip from 'primevue/tooltip'
import { DellPreset } from './theme/preset'

// PrimeVue components used across views. Registered globally with a Prime*
// prefix so DDS class names and Vue component names never collide.
import Badge from 'primevue/badge'
import Button from 'primevue/button'
import Card from 'primevue/card'
import Column from 'primevue/column'
import ConfirmDialog from 'primevue/confirmdialog'
import DataTable from 'primevue/datatable'
import Dialog from 'primevue/dialog'
import IconField from 'primevue/iconfield'
import InputIcon from 'primevue/inputicon'
import InputNumber from 'primevue/inputnumber'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import MultiSelect from 'primevue/multiselect'
import Password from 'primevue/password'
import ProgressSpinner from 'primevue/progressspinner'
import Select from 'primevue/select'
import Tag from 'primevue/tag'
import Toast from 'primevue/toast'
import Toolbar from 'primevue/toolbar'

import App from './App.vue'
import router from './router'
import { installAuthInterceptors } from './stores'

const app = createApp(App)

const pinia = createPinia()
pinia.use(piniaPluginPersistedState)
app.use(pinia)

// Interceptors need Pinia active, so this runs after app.use(pinia).
installAuthInterceptors()

app.use(router)
app.use(PrimeVue, {
  theme: {
    preset: DellPreset,
    options: {
      darkModeSelector: '.my-app-dark',
      // Keep PrimeVue's generated CSS below DDS's, so DDS layout wins.
      cssLayer: { name: 'primevue', order: 'dds, primevue' },
    },
  },
})
app.use(ConfirmationService)
app.use(ToastService)
app.directive('tooltip', Tooltip)

app.component('PrimeBadge', Badge)
app.component('PrimeButton', Button)
app.component('PrimeCard', Card)
app.component('PrimeColumn', Column)
app.component('PrimeConfirmDialog', ConfirmDialog)
app.component('PrimeDataTable', DataTable)
app.component('PrimeDialog', Dialog)
app.component('PrimeIconField', IconField)
app.component('PrimeInputIcon', InputIcon)
app.component('PrimeInputNumber', InputNumber)
app.component('PrimeInputText', InputText)
app.component('PrimeMessage', Message)
app.component('PrimeMultiSelect', MultiSelect)
app.component('PrimePassword', Password)
app.component('PrimeProgressSpinner', ProgressSpinner)
app.component('PrimeSelect', Select)
app.component('PrimeTag', Tag)
app.component('PrimeToast', Toast)
app.component('PrimeToolbar', Toolbar)

app.mount('#app')
