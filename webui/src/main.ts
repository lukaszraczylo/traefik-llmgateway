import { FontAwesomeIcon } from '@fortawesome/vue-fontawesome'
import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from '@/App.vue'
import '@/assets/main.css'

const app = createApp(App)
app.use(createPinia())
app.component('FontAwesomeIcon', FontAwesomeIcon)
app.mount('#app')
