// Connection Collector - WebRTC/WebSocket connection telemetry for neko client

import { getTelemetry } from '../service'
import { ConnectionMetrics, TelemetryEventType } from '../types'

interface NekoClientLike {
  on: (event: string, handler: (...args: unknown[]) => void) => NekoClientLike
  off?: (event: string, handler: (...args: unknown[]) => void) => NekoClientLike
  socketOpen?: boolean
  peerConnected?: boolean
  connected?: boolean
  id?: string
  _peer?: RTCPeerConnection
  _ws?: WebSocket
}

interface ServerInfo {
  url: string
  version?: string
  ice?: RTCIceServer[]
  lite?: boolean
}

class ConnectionCollector {
  private isInstalled = false
  private client: NekoClientLike | null = null
  private serverInfo: ServerInfo | null = null
  private connectionStartTime: number | null = null
  private reconnectAttempts = 0
  private lastState: string = 'disconnected'
  private statsDumpInterval: number | null = null

  // Event handlers (bound) - using loose typing for EventEmitter compatibility
  private onConnecting = () => this.handleConnecting()
  private onConnected = () => this.handleConnected()
  private onDisconnected = (...args: unknown[]) => this.handleDisconnected(args[0] as Error | undefined)
  private onReconnecting = () => this.handleReconnecting()
  private onError = (...args: unknown[]) => this.handleError(args[0] as Error)
  private onDebug = (...args: unknown[]) => this.handleDebug(...args)

  install(client: NekoClientLike): void {
    if (this.isInstalled) {
      return
    }

    this.isInstalled = true
    this.client = client

    // Attach to client events
    client.on('connecting', this.onConnecting)
    client.on('connected', this.onConnected)
    client.on('disconnected', this.onDisconnected)
    client.on('reconnecting', this.onReconnecting)
    client.on('error', this.onError)
    client.on('debug', this.onDebug)

    // Start periodic stats collection
    this.startStatsCollection()
  }

  setServerInfo(info: Partial<ServerInfo>): void {
    this.serverInfo = { ...this.serverInfo, ...info } as ServerInfo
  }

  private handleConnecting(): void {
    this.connectionStartTime = Date.now()
    this.lastState = 'connecting'

    const telemetry = getTelemetry()
    telemetry.trackConnection(
      {
        state: 'connecting',
        reconnectAttempts: this.reconnectAttempts,
      },
      'connection_webrtc_connecting',
    )

    // Also track server info if available
    if (this.serverInfo) {
      telemetry.track('connection_webrtc_connecting', 'info', {
        serverUrl: this.serverInfo.url,
        iceLite: this.serverInfo.lite,
        iceServersCount: this.serverInfo.ice?.length || 0,
      })
    }
  }

  private handleConnected(): void {
    const connectionDuration = this.connectionStartTime ? Date.now() - this.connectionStartTime : undefined
    this.lastState = 'connected'

    const telemetry = getTelemetry()
    const metrics: ConnectionMetrics = {
      state: 'connected',
      previousState: 'connecting',
      duration: connectionDuration,
      reconnectAttempts: this.reconnectAttempts,
    }

    // Get ICE state if available
    if (this.client?._peer) {
      metrics.iceConnectionState = this.client._peer.iceConnectionState
      metrics.iceGatheringState = this.client._peer.iceGatheringState
      metrics.signalingState = this.client._peer.signalingState
    }

    telemetry.trackConnection(metrics, 'connection_webrtc_connected')

    // Track successful connection with server info
    telemetry.track('connection_webrtc_connected', 'info', {
      connectionTimeMs: connectionDuration,
      reconnectAttempts: this.reconnectAttempts,
      clientId: this.client?.id,
      serverUrl: this.serverInfo?.url,
    })

    // Reset reconnect attempts on successful connection
    this.reconnectAttempts = 0
  }

  private handleDisconnected(reason?: Error): void {
    const previousState = this.lastState
    this.lastState = 'disconnected'

    const telemetry = getTelemetry()
    const metrics: ConnectionMetrics = {
      state: 'disconnected',
      previousState,
      reconnectAttempts: this.reconnectAttempts,
    }

    // Determine the event type based on whether it was a failure
    const eventType: TelemetryEventType = reason ? 'connection_webrtc_failed' : 'connection_webrtc_disconnected'

    telemetry.trackConnection(metrics, eventType)

    // Track detailed disconnect info
    telemetry.track(
      eventType,
      reason ? 'error' : 'info',
      {
        reason: reason?.message || 'User initiated or server closed',
        previousState,
        reconnectAttempts: this.reconnectAttempts,
        serverUrl: this.serverInfo?.url,
      },
      reason
        ? {
            error: {
              message: reason.message,
              errorType: reason.name || 'ConnectionError',
              stack: reason.stack,
            },
          }
        : undefined,
    )
  }

  private handleReconnecting(): void {
    this.reconnectAttempts++
    this.lastState = 'reconnecting'

    const telemetry = getTelemetry()
    telemetry.trackConnection(
      {
        state: 'reconnecting',
        previousState: 'disconnected',
        reconnectAttempts: this.reconnectAttempts,
      },
      'connection_reconnecting',
    )
  }

  private handleError(error: Error): void {
    const telemetry = getTelemetry()

    // Determine if this is a WebSocket or WebRTC error
    const isWebSocketError = error.message?.toLowerCase().includes('websocket')
    const eventType: TelemetryEventType = isWebSocketError ? 'error_websocket' : 'error_webrtc'

    telemetry.track(eventType, 'error', {
      currentState: this.lastState,
      reconnectAttempts: this.reconnectAttempts,
    }, {
      error: {
        message: error.message,
        errorType: error.name || 'ConnectionError',
        stack: error.stack,
      },
    })
  }

  private handleDebug(...args: unknown[]): void {
    // Parse debug messages for specific events
    const message = args.join(' ')

    // Track ICE state changes
    if (message.includes('peer ice connection state changed')) {
      const state = message.match(/changed: (\w+)/)?.[1]
      if (state) {
        const telemetry = getTelemetry()
        telemetry.trackConnection(
          {
            state: state,
            iceConnectionState: state,
          },
          'connection_webrtc_ice_state',
        )
      }
    }

    // Track connection timeout
    if (message.includes('connection timeout')) {
      const telemetry = getTelemetry()
      telemetry.trackConnection(
        {
          state: 'timeout',
          reconnectAttempts: this.reconnectAttempts,
        },
        'connection_timeout',
      )
    }

    // Track WebSocket close
    if (message.includes('websocket closed')) {
      const telemetry = getTelemetry()
      telemetry.trackConnection(
        {
          state: 'closed',
        },
        'connection_websocket_close',
      )
    }
  }

  private startStatsCollection(): void {
    // Collect WebRTC stats every 30 seconds when connected
    this.statsDumpInterval = window.setInterval(async () => {
      if (!this.client?._peer || this.lastState !== 'connected') {
        return
      }

      try {
        const stats = await this.client._peer.getStats()
        const metrics = this.parseRTCStats(stats)

        if (Object.keys(metrics).length > 0) {
          const telemetry = getTelemetry()
          telemetry.track('connection_webrtc_connected', 'debug', {
            ...metrics,
            timestamp: Date.now(),
          })
        }
      } catch (e) {
        // Stats collection failed, ignore
      }
    }, 30000)
  }

  private parseRTCStats(stats: RTCStatsReport): Partial<ConnectionMetrics> {
    const metrics: Partial<ConnectionMetrics> = {}

    stats.forEach((report) => {
      if (report.type === 'candidate-pair' && report.state === 'succeeded') {
        metrics.bytesReceived = report.bytesReceived
        metrics.bytesSent = report.bytesSent
        metrics.roundTripTime = report.currentRoundTripTime ? report.currentRoundTripTime * 1000 : undefined

        // Get local candidate type
        const localCandidate = stats.get(report.localCandidateId)
        if (localCandidate) {
          metrics.localCandidateType = localCandidate.candidateType
        }

        // Get remote candidate type
        const remoteCandidate = stats.get(report.remoteCandidateId)
        if (remoteCandidate) {
          metrics.remoteCandidateType = remoteCandidate.candidateType
        }
      }

      if (report.type === 'inbound-rtp' && report.kind === 'video') {
        metrics.packetsLost = report.packetsLost
        metrics.jitter = report.jitter ? report.jitter * 1000 : undefined
      }
    })

    return metrics
  }

  // Track user-initiated actions
  trackLogin(displayname: string): void {
    const telemetry = getTelemetry()
    telemetry.trackAction('action_login', {
      displayname,
      timestamp: Date.now(),
    })
  }

  trackLogout(): void {
    const telemetry = getTelemetry()
    telemetry.trackAction('action_logout', {
      sessionDuration: this.connectionStartTime ? Date.now() - this.connectionStartTime : undefined,
      timestamp: Date.now(),
    })
  }

  trackControlRequest(): void {
    const telemetry = getTelemetry()
    telemetry.trackAction('action_control_request', {
      timestamp: Date.now(),
    })
  }

  trackControlRelease(): void {
    const telemetry = getTelemetry()
    telemetry.trackAction('action_control_release', {
      timestamp: Date.now(),
    })
  }

  trackFullscreen(enabled: boolean): void {
    const telemetry = getTelemetry()
    telemetry.trackAction('action_fullscreen', {
      enabled,
      timestamp: Date.now(),
    })
  }

  trackPictureInPicture(enabled: boolean): void {
    const telemetry = getTelemetry()
    telemetry.trackAction('action_pip', {
      enabled,
      timestamp: Date.now(),
    })
  }

  trackResolutionChange(width: number, height: number, rate: number): void {
    const telemetry = getTelemetry()
    telemetry.trackAction('action_resolution_change', {
      width,
      height,
      rate,
      timestamp: Date.now(),
    })
  }

  trackVolumeChange(volume: number, muted: boolean): void {
    const telemetry = getTelemetry()
    telemetry.trackAction('action_volume_change', {
      volume,
      muted,
      timestamp: Date.now(),
    })
  }

  // Track stream events
  trackStreamTrackAdded(kind: string, id: string): void {
    const telemetry = getTelemetry()
    telemetry.track('stream_track_added', 'info', {
      kind,
      id,
      timestamp: Date.now(),
    })
  }

  trackStreamPlayStarted(): void {
    const telemetry = getTelemetry()
    telemetry.track('stream_play_started', 'info', {
      connectionDuration: this.connectionStartTime ? Date.now() - this.connectionStartTime : undefined,
      timestamp: Date.now(),
    })
  }

  trackStreamPlayFailed(error: Error): void {
    const telemetry = getTelemetry()
    telemetry.track(
      'stream_play_failed',
      'error',
      {
        timestamp: Date.now(),
      },
      {
        error: {
          message: error.message,
          errorType: error.name || 'PlaybackError',
          stack: error.stack,
        },
      },
    )
  }

  getServerInfo(): ServerInfo | null {
    return this.serverInfo
  }

  uninstall(): void {
    if (!this.isInstalled || !this.client) {
      return
    }

    // Remove event listeners
    if (this.client.off) {
      this.client.off('connecting', this.onConnecting)
      this.client.off('connected', this.onConnected)
      this.client.off('disconnected', this.onDisconnected)
      this.client.off('reconnecting', this.onReconnecting)
      this.client.off('error', this.onError)
      this.client.off('debug', this.onDebug)
    }

    // Clear stats collection
    if (this.statsDumpInterval !== null) {
      clearInterval(this.statsDumpInterval)
      this.statsDumpInterval = null
    }

    this.client = null
    this.isInstalled = false
  }
}

// Singleton instance
let connectionCollectorInstance: ConnectionCollector | null = null

export function getConnectionCollector(): ConnectionCollector {
  if (!connectionCollectorInstance) {
    connectionCollectorInstance = new ConnectionCollector()
  }
  return connectionCollectorInstance
}

export function installConnectionCollector(client: NekoClientLike): void {
  getConnectionCollector().install(client)
}

export function uninstallConnectionCollector(): void {
  if (connectionCollectorInstance) {
    connectionCollectorInstance.uninstall()
  }
}
