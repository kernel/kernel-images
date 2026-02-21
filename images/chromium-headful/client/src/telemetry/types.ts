// Telemetry Types - Production-grade telemetry for Neko client

export type TelemetryEventType =
  // Session lifecycle
  | 'session_start'
  | 'session_end'
  | 'session_heartbeat'
  // App lifecycle
  | 'app_init'
  | 'app_ready'
  | 'app_visible'
  | 'app_hidden'
  | 'app_beforeunload'
  // Errors
  | 'error_js'
  | 'error_unhandled_rejection'
  | 'error_vue'
  | 'error_network'
  | 'error_websocket'
  | 'error_webrtc'
  // Performance
  | 'perf_page_load'
  | 'perf_first_contentful_paint'
  | 'perf_largest_contentful_paint'
  | 'perf_first_input_delay'
  | 'perf_cumulative_layout_shift'
  | 'perf_long_task'
  | 'perf_memory'
  | 'perf_fps'
  // Connection
  | 'connection_websocket_open'
  | 'connection_websocket_close'
  | 'connection_websocket_error'
  | 'connection_webrtc_connecting'
  | 'connection_webrtc_connected'
  | 'connection_webrtc_disconnected'
  | 'connection_webrtc_failed'
  | 'connection_webrtc_ice_state'
  | 'connection_reconnecting'
  | 'connection_timeout'
  // User actions
  | 'action_login'
  | 'action_logout'
  | 'action_control_request'
  | 'action_control_release'
  | 'action_fullscreen'
  | 'action_pip'
  | 'action_resolution_change'
  | 'action_volume_change'
  // Video/Stream
  | 'stream_track_added'
  | 'stream_play_started'
  | 'stream_play_failed'
  | 'stream_quality_change'

export type TelemetrySeverity = 'debug' | 'info' | 'warning' | 'error' | 'critical'

export interface DeviceInfo {
  userAgent: string
  platform: string
  language: string
  languages: string[]
  cookiesEnabled: boolean
  doNotTrack: string | null
  hardwareConcurrency: number
  maxTouchPoints: number
  deviceMemory?: number
  connection?: {
    effectiveType: string
    downlink: number
    rtt: number
    saveData: boolean
  }
}

export interface ScreenInfo {
  width: number
  height: number
  availWidth: number
  availHeight: number
  colorDepth: number
  pixelRatio: number
  orientation?: string
}

export interface ViewportInfo {
  width: number
  height: number
}

export interface BrowserInfo {
  name: string
  version: string
  engine: string
  webrtcSupported: boolean
  webglSupported: boolean
  cookiesEnabled: boolean
}

export interface SessionData {
  sessionId: string
  startTime: number
  pageUrl: string
  referrer: string
  device: DeviceInfo
  screen: ScreenInfo
  viewport: ViewportInfo
  browser: BrowserInfo
  timezone: string
  timezoneOffset: number
}

export interface ErrorContext {
  message: string
  stack?: string
  filename?: string
  lineno?: number
  colno?: number
  componentName?: string
  componentStack?: string
  errorType: string
  isTrusted?: boolean
}

export interface PerformanceMetrics {
  // Navigation timing
  domContentLoaded?: number
  domComplete?: number
  loadEventEnd?: number
  // Core Web Vitals
  fcp?: number
  lcp?: number
  fid?: number
  cls?: number
  ttfb?: number
  // Resource timing
  resourceCount?: number
  transferSize?: number
  // Memory
  usedJSHeapSize?: number
  totalJSHeapSize?: number
  jsHeapSizeLimit?: number
  // FPS
  fps?: number
  droppedFrames?: number
}

export interface ConnectionMetrics {
  state: string
  previousState?: string
  duration?: number
  reconnectAttempts?: number
  latency?: number
  iceConnectionState?: string
  iceGatheringState?: string
  signalingState?: string
  localCandidateType?: string
  remoteCandidateType?: string
  bytesReceived?: number
  bytesSent?: number
  packetsLost?: number
  jitter?: number
  roundTripTime?: number
}

export interface TelemetryEvent {
  // Event identification
  eventId: string
  eventType: TelemetryEventType
  severity: TelemetrySeverity
  timestamp: number

  // Session context
  sessionId: string
  sequenceNumber: number

  // Event-specific data
  data?: Record<string, unknown>

  // Error context (for error events)
  error?: ErrorContext

  // Performance metrics (for perf events)
  performance?: PerformanceMetrics

  // Connection metrics (for connection events)
  connection?: ConnectionMetrics

  // Additional context
  tags?: Record<string, string>
}

export interface TelemetryBatch {
  batchId: string
  events: TelemetryEvent[]
  session: SessionData
  sentAt: number
  retryCount: number
}

/**
 * Capture configuration - comma-separated list of event types to capture
 *
 * Special values:
 *   - 'all' (default): Capture all events
 *   - 'none': Capture no events
 *
 * Event type patterns (supports wildcards with *):
 *   - Exact: 'error_js,perf_fps,stream_play_started'
 *   - Prefix wildcard: 'error_*,perf_*' (all error and perf events)
 *   - Mixed: 'all,-perf_*' (all except performance events)
 *
 * Available event types:
 *   Session: session_start, session_end, session_heartbeat
 *   App: app_init, app_ready, app_visible, app_hidden, app_beforeunload
 *   Errors: error_js, error_unhandled_rejection, error_vue, error_network, error_websocket, error_webrtc
 *   Performance: perf_page_load, perf_first_contentful_paint, perf_largest_contentful_paint,
 *                perf_first_input_delay, perf_cumulative_layout_shift, perf_long_task, perf_memory, perf_fps
 *   Connection: connection_websocket_open, connection_websocket_close, connection_websocket_error,
 *               connection_webrtc_connecting, connection_webrtc_connected, connection_webrtc_disconnected,
 *               connection_webrtc_failed, connection_webrtc_ice_state, connection_reconnecting, connection_timeout
 *   Actions: action_login, action_logout, action_control_request, action_control_release,
 *            action_fullscreen, action_pip, action_resolution_change, action_volume_change
 *   Stream: stream_track_added, stream_play_started, stream_play_failed, stream_quality_change
 */
export type TelemetryCaptureSpec = string // 'all' | 'none' | comma-separated event types/patterns

export interface TelemetryConfig {
  enabled: boolean
  endpoint: string
  batchSize: number
  flushInterval: number
  maxRetries: number
  retryDelay: number
  sessionHeartbeatInterval: number
  performanceSampleInterval: number
  debug: boolean
  // Capture specification - comma-separated list of event types or 'all'
  capture: TelemetryCaptureSpec
}

export const DEFAULT_TELEMETRY_CONFIG: TelemetryConfig = {
  enabled: false, // Disabled by default - enabled automatically when endpoint is provided
  endpoint: '', // No default endpoint - must be explicitly set via TELEMETRY_ENDPOINT
  batchSize: 10,
  flushInterval: 30000, // 30 seconds
  maxRetries: 3,
  retryDelay: 1000,
  sessionHeartbeatInterval: 60000, // 1 minute
  performanceSampleInterval: 10000, // 10 seconds
  debug: false,
  capture: 'all', // Default: capture all events
}
