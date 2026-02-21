// Error Collector - Global error tracking for comprehensive crash reporting

import Vue from 'vue'
import { getTelemetry } from '../service'
import { ErrorContext } from '../types'

interface NetworkErrorInfo {
  url: string
  method?: string
  status?: number
  statusText?: string
  type: 'fetch' | 'xhr' | 'resource'
}

class ErrorCollector {
  private isInstalled = false
  private originalOnError: OnErrorEventHandler | null = null
  private originalOnUnhandledRejection: ((event: PromiseRejectionEvent) => void) | null = null
  private originalFetch: typeof fetch | null = null
  private originalXHROpen: typeof XMLHttpRequest.prototype.open | null = null
  private originalXHRSend: typeof XMLHttpRequest.prototype.send | null = null

  install(): void {
    if (this.isInstalled) {
      return
    }

    this.isInstalled = true
    this.installGlobalErrorHandler()
    this.installUnhandledRejectionHandler()
    this.installVueErrorHandler()
    this.installNetworkErrorHandlers()
    this.installResourceErrorHandler()
  }

  private installGlobalErrorHandler(): void {
    this.originalOnError = window.onerror

    window.onerror = (
      message: string | Event,
      source?: string,
      lineno?: number,
      colno?: number,
      error?: Error,
    ): boolean => {
      const telemetry = getTelemetry()

      const errorContext: ErrorContext = {
        message: typeof message === 'string' ? message : 'Unknown error',
        filename: source,
        lineno,
        colno,
        errorType: error?.name || 'Error',
        stack: error?.stack,
      }

      telemetry.track('error_js', 'error', undefined, { error: errorContext })

      // Call original handler if it exists
      if (this.originalOnError) {
        return this.originalOnError.call(window, message, source, lineno, colno, error)
      }

      return false
    }
  }

  private installUnhandledRejectionHandler(): void {
    this.originalOnUnhandledRejection = window.onunhandledrejection as
      | ((event: PromiseRejectionEvent) => void)
      | null

    window.addEventListener('unhandledrejection', (event: PromiseRejectionEvent) => {
      const telemetry = getTelemetry()

      const reason = event.reason
      const errorContext: ErrorContext = {
        message: reason?.message || String(reason),
        errorType: reason?.name || 'UnhandledPromiseRejection',
        stack: reason?.stack,
      }

      telemetry.track('error_unhandled_rejection', 'error', undefined, { error: errorContext })

      // Call original handler if it exists
      if (this.originalOnUnhandledRejection) {
        this.originalOnUnhandledRejection(event)
      }
    })
  }

  private installVueErrorHandler(): void {
    const originalHandler = Vue.config.errorHandler

    Vue.config.errorHandler = (err: Error, vm: Vue | null, info: string) => {
      const telemetry = getTelemetry()

      // Get component name and hierarchy
      let componentName = 'Unknown'
      let componentStack = ''

      if (vm) {
        const options = vm.$options as { name?: string; _componentTag?: string }
        componentName = options.name || options._componentTag || 'AnonymousComponent'

        // Build component stack
        const stack: string[] = [componentName]
        let parent = vm.$parent
        while (parent) {
          const parentOptions = parent.$options as { name?: string; _componentTag?: string }
          const name = parentOptions.name || parentOptions._componentTag || 'AnonymousComponent'
          stack.push(name)
          parent = parent.$parent
        }
        componentStack = stack.reverse().join(' > ')
      }

      const errorContext: ErrorContext = {
        message: err.message,
        errorType: err.name || 'VueError',
        stack: err.stack,
        componentName,
        componentStack,
      }

      telemetry.track(
        'error_vue',
        'error',
        {
          vueInfo: info,
          componentName,
          componentStack,
        },
        { error: errorContext },
      )

      // Call original handler if it exists
      if (originalHandler) {
        originalHandler.call(Vue, err, vm as Vue, info)
      } else {
        // Default behavior: log to console
        console.error('[Vue Error]', err, info)
      }
    }

    // Also capture Vue warnings in development
    if (process.env.NODE_ENV !== 'production') {
      const originalWarnHandler = Vue.config.warnHandler

      Vue.config.warnHandler = (msg: string, vm: Vue | null, trace: string) => {
        const telemetry = getTelemetry()

        let componentName = 'Unknown'
        if (vm) {
          const options = vm.$options as { name?: string; _componentTag?: string }
          componentName = options.name || options._componentTag || 'AnonymousComponent'
        }

        telemetry.track('error_vue', 'warning', {
          message: msg,
          componentName,
          trace,
          isWarning: true,
        })

        // Call original handler if it exists
        if (originalWarnHandler && vm) {
          originalWarnHandler.call(Vue, msg, vm, trace)
        }
      }
    }
  }

  private installNetworkErrorHandlers(): void {
    // Wrap fetch
    this.originalFetch = window.fetch
    const originalFetch = this.originalFetch

    window.fetch = async function (input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      const method = init?.method || 'GET'
      const startTime = performance.now()

      try {
        const response = await originalFetch.call(window, input, init)

        // Track failed responses (4xx, 5xx)
        if (!response.ok) {
          const telemetry = getTelemetry()
          const networkError: NetworkErrorInfo = {
            url,
            method,
            status: response.status,
            statusText: response.statusText,
            type: 'fetch',
          }

          telemetry.track('error_network', 'warning', {
            ...networkError,
            duration: performance.now() - startTime,
          })
        }

        return response
      } catch (error) {
        const telemetry = getTelemetry()
        const networkError: NetworkErrorInfo = {
          url,
          method,
          type: 'fetch',
        }

        telemetry.track(
          'error_network',
          'error',
          {
            ...networkError,
            duration: performance.now() - startTime,
          },
          {
            error: {
              message: (error as Error).message,
              errorType: (error as Error).name || 'NetworkError',
              stack: (error as Error).stack,
            },
          },
        )

        throw error
      }
    }

    // Wrap XMLHttpRequest
    this.originalXHROpen = XMLHttpRequest.prototype.open
    this.originalXHRSend = XMLHttpRequest.prototype.send
    const originalOpen = this.originalXHROpen
    const originalSend = this.originalXHRSend

    XMLHttpRequest.prototype.open = function (
      method: string,
      url: string | URL,
      async?: boolean,
      username?: string | null,
      password?: string | null,
    ): void {
      (this as XMLHttpRequest & { _telemetryUrl: string; _telemetryMethod: string })._telemetryUrl =
        url.toString()
      ;(this as XMLHttpRequest & { _telemetryUrl: string; _telemetryMethod: string })._telemetryMethod =
        method
      return originalOpen.call(this, method, url, async ?? true, username, password)
    }

    XMLHttpRequest.prototype.send = function (body?: Document | XMLHttpRequestBodyInit | null): void {
      const xhr = this as XMLHttpRequest & { _telemetryUrl: string; _telemetryMethod: string }
      const startTime = performance.now()

      this.addEventListener('error', () => {
        const telemetry = getTelemetry()
        telemetry.track('error_network', 'error', {
          url: xhr._telemetryUrl,
          method: xhr._telemetryMethod,
          type: 'xhr',
          status: xhr.status,
          duration: performance.now() - startTime,
        })
      })

      this.addEventListener('load', () => {
        if (xhr.status >= 400) {
          const telemetry = getTelemetry()
          telemetry.track('error_network', 'warning', {
            url: xhr._telemetryUrl,
            method: xhr._telemetryMethod,
            type: 'xhr',
            status: xhr.status,
            statusText: xhr.statusText,
            duration: performance.now() - startTime,
          })
        }
      })

      return originalSend.call(this, body)
    }
  }

  private installResourceErrorHandler(): void {
    // Listen for resource loading errors
    window.addEventListener(
      'error',
      (event: Event) => {
        const target = event.target as HTMLElement | null

        // Check if it's a resource loading error (not a JS error)
        if (target && (target.tagName === 'IMG' || target.tagName === 'SCRIPT' || target.tagName === 'LINK')) {
          const telemetry = getTelemetry()
          const src =
            (target as HTMLImageElement).src ||
            (target as HTMLScriptElement).src ||
            (target as HTMLLinkElement).href

          telemetry.track('error_network', 'warning', {
            url: src,
            type: 'resource',
            resourceType: target.tagName.toLowerCase(),
          })
        }
      },
      true,
    ) // Use capture phase to catch before event bubbles
  }

  uninstall(): void {
    if (!this.isInstalled) {
      return
    }

    // Restore original handlers
    if (this.originalOnError !== null) {
      window.onerror = this.originalOnError
    }

    if (this.originalFetch) {
      window.fetch = this.originalFetch
    }

    if (this.originalXHROpen && this.originalXHRSend) {
      XMLHttpRequest.prototype.open = this.originalXHROpen
      XMLHttpRequest.prototype.send = this.originalXHRSend
    }

    // Note: We can't easily restore Vue.config.errorHandler without keeping a reference
    // Vue.config.errorHandler = undefined

    this.isInstalled = false
  }
}

// Singleton instance
let errorCollectorInstance: ErrorCollector | null = null

export function getErrorCollector(): ErrorCollector {
  if (!errorCollectorInstance) {
    errorCollectorInstance = new ErrorCollector()
  }
  return errorCollectorInstance
}

export function installErrorCollector(): void {
  getErrorCollector().install()
}

export function uninstallErrorCollector(): void {
  if (errorCollectorInstance) {
    errorCollectorInstance.uninstall()
  }
}
