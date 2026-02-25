import { PluginObject } from 'vue'
import { NekoClient } from '~/neko'

declare global {
  const $client: NekoClient

  interface Window {
    $client: NekoClient
  }
}

declare module 'vue/types/vue' {
  interface Vue {
    $client: NekoClient
  }
}

function isDebugEnabled(): boolean {
  const params = new URLSearchParams(location.search)
  const debug = params.get('debug')
  return debug === '1' || debug === 'true'
}

const plugin: PluginObject<undefined> = {
  install(Vue) {
    const client = new NekoClient()
      .on('error', window.$log.error)
      .on('warn', window.$log.warn)
      .on('info', window.$log.info)
    if (isDebugEnabled()) {
      client.on('debug', window.$log.debug)
    }
    window.$client = client

    Vue.prototype.$client = window.$client
  },
}

export default plugin
