import { PluginObject } from 'vue'
import { NekoClient } from '~/neko'
import { getTelemetry, getConnectionCollector } from '~/telemetry'

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

const plugin: PluginObject<undefined> = {
  install(Vue) {
    window.$client = new NekoClient()
      .on('error', (error: Error) => {
        window.$log.error(error)
        // Track errors via telemetry
        getTelemetry().track('error_webrtc', 'error', undefined, {
          error: {
            message: error.message,
            errorType: error.name || 'WebRTCError',
            stack: error.stack,
          },
        })
      })
      .on('warn', (...args: unknown[]) => {
        window.$log.warn(...args)
      })
      .on('info', (...args: unknown[]) => {
        window.$log.info(...args)
        // Track connection-related info events
        const message = args.join(' ')
        if (message.includes('connected') || message.includes('disconnected')) {
          getTelemetry().track('connection_webrtc_connected', 'debug', {
            message,
            timestamp: Date.now(),
          })
        }
      })
      .on('debug', (...args: unknown[]) => {
        window.$log.debug(...args)
        // Parse debug messages for connection state changes
        const message = args.join(' ')

        // Track WebSocket connection URL
        if (message.includes('connecting to')) {
          const url = message.match(/connecting to (\S+)/)?.[1]
          if (url) {
            getConnectionCollector().setServerInfo({ url })
          }
        }
      })

    Vue.prototype.$client = window.$client
  },
}

export default plugin
