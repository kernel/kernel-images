import './assets/styles/main.scss'

import Vue from 'vue'

import Notifications from 'vue-notification'
import ToolTip from 'v-tooltip'
import Logger from './plugins/log'
import Client from './plugins/neko'
import Axios from './plugins/axios'
import Swal from './plugins/swal'
import Anime from './plugins/anime'
import Telemetry, { installTelemetryConnectionCollector } from './plugins/telemetry'

import { i18n } from './plugins/i18n'
import store from './store'
import app from './app.vue'

Vue.config.productionTip = false

// Install telemetry first to capture any errors during plugin initialization
Vue.use(Telemetry)

Vue.use(Logger)
Vue.use(Notifications)
Vue.use(ToolTip)
Vue.use(Axios)
Vue.use(Swal)
Vue.use(Anime)
Vue.use(Client)

new Vue({
  i18n,
  store,
  render: (h) => h(app),
  created() {
    this.$client.init(this)
    this.$accessor.initialise()

    // Install telemetry connection collector after client is initialized
    installTelemetryConnectionCollector(this.$client)
  },
}).$mount('#neko')
