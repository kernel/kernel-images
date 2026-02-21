// Telemetry Vue Plugin - Integrates telemetry with Vue app lifecycle

import { PluginObject } from 'vue'
import {
  TelemetryConfig,
  DEFAULT_TELEMETRY_CONFIG,
  initTelemetry,
  getTelemetry,
  installErrorCollector,
  installPerformanceCollector,
  installConnectionCollector,
  getConnectionCollector,
  TelemetryService,
} from '~/telemetry'
import { NekoClient } from '~/neko'

// Runtime configuration interface
// Only 2 flags:
//   - endpoint: If set, telemetry is enabled and sends to this URL
//   - capture: Comma-separated event types (defaults to 'all')
interface RuntimeTelemetryConfig {
  endpoint?: string
  capture?: string // comma-separated event types or 'all'
}

// Global window augmentation for runtime config
declare global {
  interface Window {
    __NEKO_TELEMETRY_CONFIG__?: RuntimeTelemetryConfig
    $telemetry: TelemetryService
  }
}

declare module 'vue/types/vue' {
  interface Vue {
    $telemetry: TelemetryService
  }
}

/**
 * Get telemetry configuration from multiple sources:
 * 1. Build-time environment variables (VUE_APP_TELEMETRY_ENDPOINT, VUE_APP_TELEMETRY_CAPTURE)
 * 2. Runtime window config (window.__NEKO_TELEMETRY_CONFIG__)
 * 3. Query parameters (?telemetry_endpoint=...&telemetry_capture=...)
 *
 * Priority: Query params > Window config > Env vars > Defaults
 *
 * Only 2 configuration options:
 *   - TELEMETRY_ENDPOINT: If set, telemetry is enabled and sends to this URL
 *   - TELEMETRY_CAPTURE: Comma-separated event types (defaults to 'all')
 */
function getTelemetryConfig(): Partial<TelemetryConfig> {
  const config: Partial<TelemetryConfig> = {}

  let endpoint: string | undefined
  let captureSpec: string | undefined

  // 1. Build-time environment variables (lowest priority)
  const envEndpoint = process.env.VUE_APP_TELEMETRY_ENDPOINT
  const envCapture = process.env.VUE_APP_TELEMETRY_CAPTURE

  if (envEndpoint) {
    endpoint = envEndpoint
  }
  if (envCapture) {
    captureSpec = envCapture
  }

  // 2. Runtime window configuration (medium priority)
  const windowConfig = window.__NEKO_TELEMETRY_CONFIG__
  if (windowConfig) {
    if (windowConfig.endpoint) {
      endpoint = windowConfig.endpoint
    }
    if (windowConfig.capture) {
      captureSpec = windowConfig.capture
    }
  }

  // 3. Query parameters (highest priority)
  const urlParams = new URLSearchParams(window.location.search)
  const paramEndpoint = urlParams.get('telemetry_endpoint')
  const paramCapture = urlParams.get('telemetry_capture')

  if (paramEndpoint) {
    endpoint = paramEndpoint
  }
  if (paramCapture) {
    captureSpec = paramCapture
  }

  // Set config: enabled is determined by whether endpoint is set
  if (endpoint) {
    config.enabled = true
    config.endpoint = endpoint
  } else {
    config.enabled = false
  }

  // Set capture spec (defaults to 'all')
  if (captureSpec) {
    config.capture = captureSpec
  }

  return config
}

const plugin: PluginObject<undefined> = {
  install(Vue) {
    // Get merged configuration
    const config = getTelemetryConfig()

    // Always log telemetry status on startup
    if (config.enabled) {
      console.log('[Telemetry] ENABLED - sending to:', config.endpoint)
      console.log('[Telemetry] Capture:', config.capture ?? 'all')
    } else {
      console.log('[Telemetry] DISABLED (no endpoint configured)')
    }

    // Debug: show config sources
    if (process.env.NODE_ENV === 'development') {
      console.log('[Telemetry] Config sources:', {
        env: { VUE_APP_TELEMETRY_ENDPOINT: process.env.VUE_APP_TELEMETRY_ENDPOINT },
        window: window.__NEKO_TELEMETRY_CONFIG__,
        queryParams: window.location.search,
      })
    }

    // Initialize telemetry service
    const telemetry = initTelemetry(config)

    // Make telemetry available globally
    window.$telemetry = telemetry
    Vue.prototype.$telemetry = telemetry

    // Only install collectors if telemetry is enabled
    if (telemetry.getConfig().enabled) {
      // Install error collector (captures JS errors, Vue errors, network errors)
      installErrorCollector()

      // Install performance collector (Core Web Vitals, FPS, memory)
      installPerformanceCollector()

      // Track app ready when Vue is mounted
      let appReadyTracked = false
      Vue.mixin({
        mounted() {
          // Only track once for the first mount
          if (!appReadyTracked) {
            appReadyTracked = true
            getTelemetry().track('app_ready', 'info', {
              mountTime: Date.now(),
            })
          }
        },
      })
    }
  },
}

/**
 * Install connection collector after neko client is initialized.
 * Call this in the app after $client is available.
 */
export function installTelemetryConnectionCollector(client: NekoClient): void {
  const telemetry = getTelemetry()
  if (!telemetry.getConfig().enabled) {
    return
  }

  // Cast to any to work around strict type checking on EventEmitter
  installConnectionCollector(client as unknown as Parameters<typeof installConnectionCollector>[0])

  // Track login/logout actions
  const originalLogin = client.login.bind(client)
  const originalLogout = client.logout.bind(client)

  client.login = function (password: string, displayname: string) {
    getConnectionCollector().trackLogin(displayname)
    return originalLogin(password, displayname)
  }

  client.logout = function () {
    getConnectionCollector().trackLogout()
    return originalLogout()
  }
}

/**
 * Get the current telemetry configuration that was used.
 */
export function getTelemetryConfigUsed(): TelemetryConfig {
  return getTelemetry().getConfig()
}

export default plugin
