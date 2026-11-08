import { FontAwesomeIcon } from '@fortawesome/vue-fontawesome'
import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from '@/App.vue'
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
app.component('FontAwesomeIcon', FontAwesomeIcon)
app.mount('#app')
