import { config } from '@fortawesome/fontawesome-svg-core'
import { FontAwesomeIcon } from '@fortawesome/vue-fontawesome'
import { useThemeStore } from '@perfect-design/ui'
import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from '@/App.vue'
import '@/assets/main.css'
import { messageOf } from '@/lib/api'
import { useToastsStore } from '@/stores/toasts'

// The admin CSP is style-src 'self': FontAwesome's runtime <style> injection is blocked, so its CSS ships in the bundle instead.
config.autoAddCss = false

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
app.component('FontAwesomeIcon', FontAwesomeIcon)
app.mount('#app')
