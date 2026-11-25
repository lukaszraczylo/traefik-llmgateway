import { useThemeStore } from '@perfect-design/ui'
import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from '@/App.vue'
import FaIcon from '@/components/FaIcon.vue'
import '@/assets/main.css'
import { messageOf } from '@/lib/api'
import { useToastsStore } from '@/stores/toasts'

const app = createApp(App)
const pinia = createPinia()
app.use(pinia)
app.config.errorHandler = (err) => {
  console.error(err)
  useToastsStore(pinia).push({ kind: 'error', message: `Unexpected error: ${messageOf(err)}` })
}
// Workbench is the app-wide style; init() restores the saved mode and the persisted choice survives reloads.
const theme = useThemeStore(pinia)
theme.init()
theme.setStyle('workbench')
app.component('FontAwesomeIcon', FaIcon)
app.mount('#app')
