import { config } from '@fortawesome/fontawesome-svg-core'
import '@fortawesome/fontawesome-svg-core/styles.css'
import { FontAwesomeIcon } from '@fortawesome/vue-fontawesome'
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
app.component('FontAwesomeIcon', FontAwesomeIcon)
app.mount('#app')
