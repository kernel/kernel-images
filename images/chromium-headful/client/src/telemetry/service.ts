// Telemetry Service - Core telemetry collection and transmission

import {
  TelemetryEvent,
  TelemetryBatch,
  TelemetryConfig,
  TelemetryEventType,
  TelemetrySeverity,
  SessionData,
  DeviceInfo,
  ScreenInfo,
  ViewportInfo,
  BrowserInfo,
  ErrorContext,
  PerformanceMetrics,
  ConnectionMetrics,
  DEFAULT_TELEMETRY_CONFIG,
} from './types'

function generateId(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 11)}`
}

function detectBrowser(): BrowserInfo {
  const ua = navigator.userAgent
  let name = 'Unknown'
  let version = 'Unknown'
  let engine = 'Unknown'

  if (ua.includes('Firefox/')) {
    name = 'Firefox'
    version = ua.match(/Firefox\/(\d+(\.\d+)?)/)?.[1] || 'Unknown'
    engine = 'Gecko'
  } else if (ua.includes('Edg/')) {
    name = 'Edge'
    version = ua.match(/Edg\/(\d+(\.\d+)?)/)?.[1] || 'Unknown'
    engine = 'Blink'
  } else if (ua.includes('Chrome/')) {
    name = 'Chrome'
    version = ua.match(/Chrome\/(\d+(\.\d+)?)/)?.[1] || 'Unknown'
    engine = 'Blink'
  } else if (ua.includes('Safari/') && !ua.includes('Chrome')) {
    name = 'Safari'
    version = ua.match(/Version\/(\d+(\.\d+)?)/)?.[1] || 'Unknown'
    engine = 'WebKit'
  } else if (ua.includes('Opera/') || ua.includes('OPR/')) {
    name = 'Opera'
    version = ua.match(/(?:Opera|OPR)\/(\d+(\.\d+)?)/)?.[1] || 'Unknown'
    engine = 'Blink'
  }

  let webglSupported = false
  try {
    const canvas = document.createElement('canvas')
    webglSupported = !!(canvas.getContext('webgl') || canvas.getContext('experimental-webgl'))
  } catch (e) {
    // WebGL not supported
  }

  return {
    name,
    version,
    engine,
    webrtcSupported:
      typeof RTCPeerConnection !== 'undefined' && typeof RTCPeerConnection.prototype.addTransceiver !== 'undefined',
    webglSupported,
    cookiesEnabled: navigator.cookieEnabled,
  }
}

function getDeviceInfo(): DeviceInfo {
  const nav = navigator as Navigator & {
    deviceMemory?: number
    connection?: {
      effectiveType: string
      downlink: number
      rtt: number
      saveData: boolean
    }
  }

  const info: DeviceInfo = {
    userAgent: nav.userAgent,
    platform: nav.platform,
    language: nav.language,
    languages: Array.from(nav.languages || [nav.language]),
    cookiesEnabled: nav.cookieEnabled,
    doNotTrack: nav.doNotTrack,
    hardwareConcurrency: nav.hardwareConcurrency || 0,
    maxTouchPoints: nav.maxTouchPoints || 0,
  }

  if (nav.deviceMemory) {
    info.deviceMemory = nav.deviceMemory
  }

  if (nav.connection) {
    info.connection = {
      effectiveType: nav.connection.effectiveType,
      downlink: nav.connection.downlink,
      rtt: nav.connection.rtt,
      saveData: nav.connection.saveData,
    }
  }

  return info
}

function getScreenInfo(): ScreenInfo {
  return {
    width: screen.width,
    height: screen.height,
    availWidth: screen.availWidth,
    availHeight: screen.availHeight,
    colorDepth: screen.colorDepth,
    pixelRatio: window.devicePixelRatio || 1,
    orientation: screen.orientation?.type,
  }
}

function getViewportInfo(): ViewportInfo {
  return {
    width: window.innerWidth,
    height: window.innerHeight,
  }
}

class TelemetryService {
  private config: TelemetryConfig
  private session: SessionData | null = null
  private eventQueue: TelemetryEvent[] = []
  private pendingBatches: Map<string, TelemetryBatch> = new Map()
  private sequenceNumber = 0
  private flushTimer: number | null = null
  private heartbeatTimer: number | null = null
  private performanceTimer: number | null = null
  private isInitialized = false
  private isFlushing = false

  constructor(config: Partial<TelemetryConfig> = {}) {
    this.config = { ...DEFAULT_TELEMETRY_CONFIG, ...config }
  }

  init(): void {
    if (this.isInitialized) {
      this.log('Telemetry already initialized')
      return
    }

    if (!this.config.enabled) {
      this.log('Telemetry disabled')
      return
    }

    this.isInitialized = true
    this.session = this.createSession()

    // Parse capture specification
    this.parseCaptureSpec()

    this.log('Telemetry initialized', this.session)
    this.log('Capture config:', {
      spec: this.config.capture,
      captureAll: this.captureAll,
      includes: this.captureIncludes,
      excludes: this.captureExcludes,
    })

    // Track session start
    this.track('session_start', 'info', {
      session: this.session,
    })

    // Start flush timer
    this.startFlushTimer()

    // Start heartbeat
    this.startHeartbeat()

    // Setup visibility change handler
    this.setupVisibilityHandler()

    // Setup beforeunload handler
    this.setupBeforeUnloadHandler()

    // Track app init
    this.track('app_init', 'info')
  }

  private createSession(): SessionData {
    return {
      sessionId: generateId(),
      startTime: Date.now(),
      pageUrl: window.location.href,
      referrer: document.referrer,
      device: getDeviceInfo(),
      screen: getScreenInfo(),
      viewport: getViewportInfo(),
      browser: detectBrowser(),
      timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      timezoneOffset: new Date().getTimezoneOffset(),
    }
  }

  private log(...args: unknown[]): void {
    if (this.config.debug) {
      console.log('[Telemetry]', ...args)
    }
  }

  private startFlushTimer(): void {
    if (this.flushTimer) {
      clearInterval(this.flushTimer)
    }
    this.flushTimer = window.setInterval(() => {
      this.flush()
    }, this.config.flushInterval)
  }

  private startHeartbeat(): void {
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer)
    }
    this.heartbeatTimer = window.setInterval(() => {
      this.track('session_heartbeat', 'debug', {
        uptimeMs: Date.now() - (this.session?.startTime || Date.now()),
        eventCount: this.sequenceNumber,
        queueSize: this.eventQueue.length,
      })
    }, this.config.sessionHeartbeatInterval)
  }

  private setupVisibilityHandler(): void {
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') {
        this.track('app_visible', 'debug')
      } else {
        this.track('app_hidden', 'debug')
        // Flush when page becomes hidden
        this.flush()
      }
    })
  }

  private setupBeforeUnloadHandler(): void {
    window.addEventListener('beforeunload', () => {
      this.track('app_beforeunload', 'info')
      this.track('session_end', 'info', {
        duration: Date.now() - (this.session?.startTime || Date.now()),
        totalEvents: this.sequenceNumber,
      })
      // Use sendBeacon for reliable delivery on page unload
      this.flushSync()
    })
  }

  track(
    eventType: TelemetryEventType,
    severity: TelemetrySeverity = 'info',
    data?: Record<string, unknown>,
    options?: {
      error?: ErrorContext
      performance?: PerformanceMetrics
      connection?: ConnectionMetrics
      tags?: Record<string, string>
    },
  ): void {
    if (!this.config.enabled || !this.isInitialized) {
      return
    }

    // Check category filters
    if (!this.shouldCollect(eventType)) {
      return
    }

    const event: TelemetryEvent = {
      eventId: generateId(),
      eventType,
      severity,
      timestamp: Date.now(),
      sessionId: this.session?.sessionId || 'unknown',
      sequenceNumber: ++this.sequenceNumber,
      data,
      ...options,
    }

    this.log('Track event:', event)
    this.eventQueue.push(event)

    // Flush if batch size reached
    if (this.eventQueue.length >= this.config.batchSize) {
      this.flush()
    }
  }

  // Parsed capture patterns (cached for performance)
  private captureIncludes: string[] = []
  private captureExcludes: string[] = []
  private captureAll = true

  private parseCaptureSpec(): void {
    const spec = this.config.capture.trim().toLowerCase()

    if (spec === 'all' || spec === '') {
      this.captureAll = true
      this.captureIncludes = []
      this.captureExcludes = []
      return
    }

    if (spec === 'none') {
      this.captureAll = false
      this.captureIncludes = []
      this.captureExcludes = []
      return
    }

    this.captureAll = false
    this.captureIncludes = []
    this.captureExcludes = []

    const parts = spec.split(',').map((p) => p.trim()).filter((p) => p)

    for (const part of parts) {
      if (part === 'all') {
        this.captureAll = true
      } else if (part.startsWith('-')) {
        // Exclusion pattern
        this.captureExcludes.push(part.slice(1))
      } else {
        // Inclusion pattern
        this.captureIncludes.push(part)
      }
    }
  }

  private matchesPattern(eventType: string, pattern: string): boolean {
    if (pattern === eventType) {
      return true
    }

    // Handle wildcard patterns (e.g., 'error_*', 'perf_*')
    if (pattern.endsWith('*')) {
      const prefix = pattern.slice(0, -1)
      return eventType.startsWith(prefix)
    }

    return false
  }

  private shouldCollect(eventType: TelemetryEventType): boolean {
    const eventLower = eventType.toLowerCase()

    // Check exclusions first (they take priority)
    for (const pattern of this.captureExcludes) {
      if (this.matchesPattern(eventLower, pattern)) {
        return false
      }
    }

    // If capturing all, allow (unless excluded above)
    if (this.captureAll) {
      return true
    }

    // Check inclusions
    for (const pattern of this.captureIncludes) {
      if (this.matchesPattern(eventLower, pattern)) {
        return true
      }
    }

    // Not in inclusion list
    return false
  }

  trackError(error: Error | ErrorEvent | PromiseRejectionEvent, context?: Partial<ErrorContext>): void {
    let errorContext: ErrorContext

    if (error instanceof ErrorEvent) {
      errorContext = {
        message: error.message,
        filename: error.filename,
        lineno: error.lineno,
        colno: error.colno,
        errorType: error.error?.name || 'Error',
        stack: error.error?.stack,
        isTrusted: error.isTrusted,
        ...context,
      }
    } else if ('reason' in error) {
      // PromiseRejectionEvent
      const reason = error.reason
      errorContext = {
        message: reason?.message || String(reason),
        errorType: reason?.name || 'UnhandledPromiseRejection',
        stack: reason?.stack,
        ...context,
      }
    } else {
      errorContext = {
        message: error.message,
        errorType: error.name || 'Error',
        stack: error.stack,
        ...context,
      }
    }

    const eventType: TelemetryEventType =
      (context as { eventType?: TelemetryEventType })?.eventType || 'error_js'

    this.track(eventType, 'error', undefined, { error: errorContext })
  }

  trackPerformance(metrics: PerformanceMetrics, eventType: TelemetryEventType = 'perf_page_load'): void {
    this.track(eventType, 'info', undefined, { performance: metrics })
  }

  trackConnection(metrics: ConnectionMetrics, eventType: TelemetryEventType): void {
    this.track(eventType, metrics.state === 'failed' ? 'error' : 'info', undefined, { connection: metrics })
  }

  trackAction(
    action: TelemetryEventType,
    data?: Record<string, unknown>,
    tags?: Record<string, string>,
  ): void {
    this.track(action, 'info', data, { tags })
  }

  async flush(): Promise<void> {
    if (!this.config.enabled || this.isFlushing || this.eventQueue.length === 0) {
      return
    }

    this.isFlushing = true

    try {
      const events = [...this.eventQueue]
      this.eventQueue = []

      const batch: TelemetryBatch = {
        batchId: generateId(),
        events,
        session: this.session!,
        sentAt: Date.now(),
        retryCount: 0,
      }

      await this.sendBatch(batch)
    } catch (error) {
      this.log('Flush error:', error)
    } finally {
      this.isFlushing = false
    }
  }

  private flushSync(): void {
    if (!this.config.enabled || this.eventQueue.length === 0 || !this.session) {
      return
    }

    const events = [...this.eventQueue]
    this.eventQueue = []

    const batch: TelemetryBatch = {
      batchId: generateId(),
      events,
      session: this.session,
      sentAt: Date.now(),
      retryCount: 0,
    }

    const payload = JSON.stringify(batch)

    // Use sendBeacon for reliable delivery
    if (navigator.sendBeacon) {
      const blob = new Blob([payload], { type: 'application/json' })
      navigator.sendBeacon(this.config.endpoint, blob)
      this.log('Sent via sendBeacon')
    } else {
      // Fallback to sync XHR
      const xhr = new XMLHttpRequest()
      xhr.open('POST', this.config.endpoint, false) // sync
      xhr.setRequestHeader('Content-Type', 'application/json')
      try {
        xhr.send(payload)
      } catch (e) {
        this.log('Sync send failed:', e)
      }
    }
  }

  private async sendBatch(batch: TelemetryBatch): Promise<void> {
    this.pendingBatches.set(batch.batchId, batch)
    this.log('Sending batch:', batch.batchId, 'with', batch.events.length, 'events')

    try {
      const response = await fetch(this.config.endpoint, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify(batch),
        keepalive: true,
      })

      if (!response.ok) {
        throw new Error(`HTTP ${response.status}: ${response.statusText}`)
      }

      this.log('Batch sent successfully:', batch.batchId)
      this.pendingBatches.delete(batch.batchId)
    } catch (error) {
      this.log('Batch send failed:', batch.batchId, error)

      // Retry logic
      if (batch.retryCount < this.config.maxRetries) {
        batch.retryCount++
        const delay = this.config.retryDelay * Math.pow(2, batch.retryCount - 1)
        this.log(`Retrying batch ${batch.batchId} in ${delay}ms (attempt ${batch.retryCount})`)

        setTimeout(() => {
          this.sendBatch(batch)
        }, delay)
      } else {
        this.log(`Batch ${batch.batchId} failed after ${this.config.maxRetries} retries`)
        this.pendingBatches.delete(batch.batchId)
      }
    }
  }

  getSessionId(): string {
    return this.session?.sessionId || 'unknown'
  }

  getConfig(): TelemetryConfig {
    return { ...this.config }
  }

  updateConfig(config: Partial<TelemetryConfig>): void {
    this.config = { ...this.config, ...config }

    // Re-parse capture spec if changed
    if (config.capture !== undefined) {
      this.parseCaptureSpec()
    }

    // Restart timers if intervals changed
    if (config.flushInterval) {
      this.startFlushTimer()
    }
    if (config.sessionHeartbeatInterval) {
      this.startHeartbeat()
    }
  }

  destroy(): void {
    if (this.flushTimer) {
      clearInterval(this.flushTimer)
      this.flushTimer = null
    }
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer)
      this.heartbeatTimer = null
    }
    if (this.performanceTimer) {
      clearInterval(this.performanceTimer)
      this.performanceTimer = null
    }

    // Final flush
    this.flushSync()

    this.isInitialized = false
    this.log('Telemetry destroyed')
  }
}

// Singleton instance
let telemetryInstance: TelemetryService | null = null

export function getTelemetry(): TelemetryService {
  if (!telemetryInstance) {
    telemetryInstance = new TelemetryService()
  }
  return telemetryInstance
}

export function initTelemetry(config?: Partial<TelemetryConfig>): TelemetryService {
  if (telemetryInstance) {
    telemetryInstance.updateConfig(config || {})
  } else {
    telemetryInstance = new TelemetryService(config)
  }
  telemetryInstance.init()
  return telemetryInstance
}

export { TelemetryService }
