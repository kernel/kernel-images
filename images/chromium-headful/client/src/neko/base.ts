import EventEmitter from 'eventemitter3'
import { OPCODE } from './data'
import { EVENT, WebSocketEvents } from './events'

import {
  WebSocketMessages,
  WebSocketPayloads,
  SignalProvidePayload,
  SignalCandidatePayload,
  SignalOfferPayload,
  SignalAnswerMessage,
} from './messages'

// A retry that reproduces the same failure is not recovery. Without a bound, a
// client that cannot construct a peer reconnects on the 15s watchdog forever,
// leaving the viewer black with nothing reported to whoever is watching.
const MAX_CONNECT_ATTEMPTS = 3

// Spacing between attempts, so a transient socket or relay problem has a moment
// to clear before the next one. Exported for tests that drive the timer.
export const RETRY_DELAY_MS = 1000

// A connect walks transport -> signaling -> media. A single 15s clock over all
// three meant a socket that never opened burned the whole budget and reported
// only "timeout", so each stage now fails on its own bound with its own reason.
export type ConnectStage = 'transport' | 'signaling' | 'media'

export const CONNECT_STAGE_TIMEOUT_MS: Record<ConnectStage, number> = {
  transport: 5000,
  signaling: 5000,
  media: 5000,
}

export interface BaseEvents {
  info: (...message: any[]) => void
  warn: (...message: any[]) => void
  debug: (...message: any[]) => void
  error: (error: Error) => void
}

export abstract class BaseClient extends EventEmitter<BaseEvents> {
  protected _ws?: WebSocket
  protected _ws_heartbeat?: number
  protected _peer?: RTCPeerConnection
  protected _channel?: RTCDataChannel
  protected _timeout?: number
  protected _stage?: ConnectStage
  protected _retry?: number
  protected _connectAttempts = 0
  protected _everConnected = false
  protected _gaveUp = false
  // Tagged at the source of the failure. Undefined means terminal: only the
  // transport and signaling stages are transient enough to retry.
  protected _failure?: ConnectStage
  protected _lastAttempt?: { url: string; password: string; displayname: string }
  protected _displayname?: string
  protected _state: RTCIceConnectionState = 'disconnected'
  protected _id = ''
  protected _candidates: RTCIceCandidate[] = []

  get id() {
    return this._id
  }

  get supported() {
    return typeof RTCPeerConnection !== 'undefined' && typeof RTCPeerConnection.prototype.addTransceiver !== 'undefined'
  }

  get socketOpen() {
    return typeof this._ws !== 'undefined' && this._ws.readyState === WebSocket.OPEN
  }

  get peerConnected() {
    return typeof this._peer !== 'undefined' && ['connected', 'checking', 'completed'].includes(this._state)
  }

  get connected() {
    return this.peerConnected && this.socketOpen
  }

  public connect(url: string, password: string, displayname: string) {
    if (this._gaveUp) {
      this.emit('debug', `not reconnecting, already gave up`)
      return
    }

    if (this.socketOpen) {
      this.emit('warn', `attempting to create websocket while connection open`)
      return
    }

    if (!this.supported) {
      this.giveUp(new Error('browser does not support webrtc (RTCPeerConnection missing)'))
      return
    }

    if (this._connectAttempts >= MAX_CONNECT_ATTEMPTS) {
      this.giveUp(new Error(`live view did not start after ${MAX_CONNECT_ATTEMPTS} attempts`))
      return
    }
    this._connectAttempts++
    this._failure = undefined
    this._lastAttempt = { url, password, displayname }

    this._displayname = displayname
    this[EVENT.CONNECTING]()

    try {
      this._ws = new WebSocket(
        `${url}?password=${encodeURIComponent(password)}&username=${encodeURIComponent(displayname)}`,
      )
      this.emit('debug', `connecting to ${this._ws.url}`)
      this._ws.onmessage = this.onMessage.bind(this)
      this._ws.onerror = this.onError.bind(this)
      this._ws.onopen = () => this.armStage('signaling')
      this._ws.onclose = (event) => {
        this.emit('debug', `websocket closed: code=${event.code}, reason=${event.reason}`)
        this._failure = 'transport'
        this.onDisconnected(new Error('websocket closed'))
      }
      this.armStage('transport')
    } catch (err: any) {
      this.onDisconnected(err)
    }
  }

  protected disconnect() {
    if (this._timeout) {
      clearTimeout(this._timeout)
      this._timeout = undefined
    }
    this._stage = undefined

    if (this._retry) {
      clearTimeout(this._retry)
      this._retry = undefined
    }

    if (this._ws_heartbeat) {
      clearInterval(this._ws_heartbeat)
      this._ws_heartbeat = undefined
    }

    if (this._ws) {
      // reset all events
      this._ws.onmessage = () => {}
      this._ws.onerror = () => {}
      this._ws.onclose = () => {}

      try {
        this._ws.close()
      } catch (err) {}

      this._ws = undefined
    }

    if (this._channel) {
      // reset all events
      this._channel.onmessage = () => {}
      this._channel.onerror = () => {}
      this._channel.onclose = () => {}

      try {
        this._channel.close()
      } catch (err) {}

      this._channel = undefined
    }

    if (this._peer) {
      // reset all events
      this._peer.onconnectionstatechange = () => {}
      this._peer.onsignalingstatechange = () => {}
      this._peer.oniceconnectionstatechange = () => {}
      this._peer.ontrack = () => {}

      try {
        this._peer.close()
      } catch (err) {}

      this._peer = undefined
    }

    this._state = 'disconnected'
    this._displayname = undefined
    this._id = ''
  }

  public sendData(event: 'wheel', data: { x: number; y: number; controlKey?: boolean }): void
  public sendData(event: 'mousemove', data: { x: number; y: number }): void
  public sendData(event: 'mousedown' | 'mouseup' | 'keydown' | 'keyup', data: { key: number }): void
  public sendData(event: string, data: any) {
    if (!this.connected) {
      this.emit('warn', `attempting to send data while disconnected`)
      return
    }

    let buffer: ArrayBuffer
    let payload: DataView
    switch (event) {
      case 'mousemove':
        buffer = new ArrayBuffer(7)
        payload = new DataView(buffer)
        payload.setUint8(0, OPCODE.MOVE)
        payload.setUint16(1, 4, true)
        payload.setUint16(3, data.x, true)
        payload.setUint16(5, data.y, true)
        break
      case 'wheel':
        buffer = new ArrayBuffer(8)
        payload = new DataView(buffer)
        payload.setUint8(0, OPCODE.SCROLL)
        payload.setUint16(1, 5, true)
        payload.setInt16(3, data.x, true)
        payload.setInt16(5, data.y, true)
        payload.setUint8(7, data.controlKey ? 1 : 0)
        break
      case 'keydown':
      case 'mousedown':
        buffer = new ArrayBuffer(11)
        payload = new DataView(buffer)
        payload.setUint8(0, OPCODE.KEY_DOWN)
        payload.setUint16(1, 8, true)
        payload.setBigUint64(3, BigInt(data.key), true)
        break
      case 'keyup':
      case 'mouseup':
        buffer = new ArrayBuffer(11)
        payload = new DataView(buffer)
        payload.setUint8(0, OPCODE.KEY_UP)
        payload.setUint16(1, 8, true)
        payload.setBigUint64(3, BigInt(data.key), true)
        break
      default:
        this.emit('warn', `unknown data event: ${event}`)
    }

    // @ts-ignore
    if (typeof buffer !== 'undefined') {
      this._channel!.send(buffer)
    }
  }

  public sendMessage(event: WebSocketEvents, payload?: WebSocketPayloads) {
    if (!this.connected) {
      this.emit('warn', `attempting to send message while disconnected`)
      return
    }
    this.emit('debug', `sending event '${event}' ${payload ? `with payload: ` : ''}`, payload)
    this._ws!.send(JSON.stringify({ event, ...payload }))
  }

  public async createPeer(lite: boolean, servers: RTCIceServer[]) {
    this.emit('debug', `creating peer`)
    if (!this.socketOpen) {
      this.emit(
        'warn',
        `attempting to create peer with no websocket: `,
        this._ws ? `state: ${this._ws.readyState}` : 'no socket',
      )
      return
    }

    if (this.peerConnected) {
      this.emit('warn', `attempting to create peer while connected`)
      return
    }

    if (lite !== true) {
      this._peer = new RTCPeerConnection({
        iceServers: servers,
      })
    } else {
      this._peer = new RTCPeerConnection()
    }

    this._peer.onconnectionstatechange = () => {
      this.emit('debug', `peer connection state changed`, this._peer ? this._peer.connectionState : undefined)
    }

    this._peer.onsignalingstatechange = () => {
      this.emit('debug', `peer signaling state changed`, this._peer ? this._peer.signalingState : undefined)
    }

    this._peer.oniceconnectionstatechange = () => {
      this._state = this._peer!.iceConnectionState

      this.emit('debug', `peer ice connection state changed: ${this._peer!.iceConnectionState}`)

      switch (this._state) {
        case 'checking':
          if (this._timeout) {
            clearTimeout(this._timeout)
            this._timeout = undefined
          }
          break
        case 'connected':
          this.onConnected()
          break
        case 'disconnected':
          this[EVENT.RECONNECTING]()
          break
        // https://developer.mozilla.org/en-US/docs/Web/API/WebRTC_API/Signaling_and_video_calling#ice_connection_state
        // We don't watch the disconnected signaling state here as it can indicate temporary issues and may
        // go back to a connected state after some time. Watching it would close the video call on any temporary
        // network issue.
        case 'failed':
          this.onDisconnected(new Error('peer failed'))
          break
        case 'closed':
          this.onDisconnected(new Error('peer closed'))
          break
      }
    }

    this._peer.ontrack = this.onTrack.bind(this)

    this._peer.onicecandidate = (event: RTCPeerConnectionIceEvent) => {
      if (!event.candidate) {
        this.emit('debug', `sent all local ICE candidates`)
        return
      }

      const init = event.candidate.toJSON()
      this.emit('debug', `sending local ICE candidate`, init)

      this._ws!.send(
        JSON.stringify({
          event: EVENT.SIGNAL.CANDIDATE,
          data: JSON.stringify(init),
        }),
      )
    }

    this._peer.onnegotiationneeded = async () => {
      this.emit('warn', `negotiation is needed`)

      const d = await this._peer!.createOffer()
      await this._peer!.setLocalDescription(d)

      this._ws!.send(
        JSON.stringify({
          event: EVENT.SIGNAL.OFFER,
          sdp: d.sdp,
        }),
      )
    }

    this._channel = this._peer.createDataChannel('data')
    this._channel.onerror = this.onError.bind(this)
    this._channel.onmessage = this.onData.bind(this)
    this._channel.onclose = this.onDisconnected.bind(this, new Error('peer data channel closed'))
  }

  public async setRemoteOffer(sdp: string) {
    if (!this._peer) {
      this.emit('warn', `attempting to set remote offer while disconnected`)
      return
    }

    await this._peer.setRemoteDescription({ type: 'offer', sdp })

    for (const candidate of this._candidates) {
      await this._peer.addIceCandidate(candidate)
    }
    this._candidates = []

    try {
      const d = await this._peer.createAnswer()

      // add stereo=1 to answer sdp to enable stereo audio for chromium
      d.sdp = d.sdp?.replace(/(stereo=1;)?useinbandfec=1/, 'useinbandfec=1;stereo=1')

      this._peer!.setLocalDescription(d)

      this._ws!.send(
        JSON.stringify({
          event: EVENT.SIGNAL.ANSWER,
          sdp: d.sdp,
          displayname: this._displayname,
        }),
      )
    } catch (err: any) {
      this.emit('error', err)
    }
  }

  public async setRemoteAnswer(sdp: string) {
    if (!this._peer) {
      this.emit('warn', `attempting to set remote answer while disconnected`)
      return
    }

    await this._peer.setRemoteDescription({ type: 'answer', sdp })
  }

  // onMessage is assigned straight to ws.onmessage, so a rejection here has
  // nowhere to go: without this guard a throw from createPeer or
  // setRemoteOffer is discarded and the client cannot tell "peer construction
  // failed" from "still connecting".
  private async onMessage(e: MessageEvent) {
    try {
      await this.handleMessage(e)
    } catch (err: unknown) {
      this.onDisconnected(err instanceof Error ? err : new Error(String(err)))
    }
  }

  private async handleMessage(e: MessageEvent) {
    const { event, ...payload } = JSON.parse(e.data) as WebSocketMessages

    this.emit('debug', `received websocket event ${event} ${payload ? `with payload: ` : ''}`, payload)

    if (event === EVENT.SIGNAL.PROVIDE) {
      const { sdp, lite, ice, id } = payload as SignalProvidePayload
      this._id = id
      await this.createPeer(lite, ice)
      await this.setRemoteOffer(sdp)
      this.armStage('media')
      return
    }

    if (event === EVENT.SIGNAL.OFFER) {
      const { sdp } = payload as SignalOfferPayload
      await this.setRemoteOffer(sdp)
      this.armStage('media')
      return
    }

    if (event === EVENT.SIGNAL.ANSWER) {
      const { sdp } = payload as SignalAnswerMessage
      await this.setRemoteAnswer(sdp)
      return
    }

    if (event === EVENT.SIGNAL.CANDIDATE) {
      const { data } = payload as SignalCandidatePayload
      const candidate: RTCIceCandidate = JSON.parse(data)
      if (this._peer) {
        this._peer.addIceCandidate(candidate)
      } else {
        this._candidates.push(candidate)
      }
      return
    }

    // @ts-ignore
    if (typeof this[event] === 'function') {
      // @ts-ignore
      this[event](payload)
    } else {
      this[EVENT.MESSAGE](event, payload)
    }
  }

  private onData(e: MessageEvent) {
    this[EVENT.DATA](e.data)
  }

  private onTrack(event: RTCTrackEvent) {
    this.emit('debug', `received ${event.track.kind} track from peer: ${event.track.id}`, event)
    const stream = event.streams[0]
    if (!stream) {
      this.emit('warn', `no stream provided for track ${event.track.id}(${event.track.label})`)
      return
    }
    this[EVENT.TRACK](event)
  }

  private onError(event: Event) {
    this.emit('error', (event as ErrorEvent).error)
  }

  private postParentMessage(message: Record<string, unknown>) {
    if (window.parent === window) {
      return
    }

    let targetOrigin = '*'
    try {
      if (document.referrer) {
        targetOrigin = new URL(document.referrer).origin
      }
    } catch (e) {}

    window.parent.postMessage(message, targetOrigin)
  }

  private onConnected() {
    if (this._timeout) {
      clearTimeout(this._timeout)
      this._timeout = undefined
    }

    if (!this.connected) {
      this.emit('warn', `onConnected called while being disconnected`)
      return
    }

    this._everConnected = true
    this._connectAttempts = 0

    this.emit('debug', `connected`)
    this[EVENT.CONNECTED]()
  }

  private armStage(stage: ConnectStage) {
    if (this._timeout) {
      clearTimeout(this._timeout)
    }

    this._stage = stage
    this._timeout = window.setTimeout(this.onTimeout.bind(this), CONNECT_STAGE_TIMEOUT_MS[stage])
  }

  private onTimeout() {
    const stage = this._stage ?? 'transport'
    this.emit('debug', `connection timeout at ${stage} stage`)
    this._failure = stage

    if (this._timeout) {
      clearTimeout(this._timeout)
      this._timeout = undefined
    }

    // A retried attempt is internal, so the parent only hears about the last
    // one. Reporting every attempt would also make an embedder that remounts on
    // the timeout event retry on top of us, and a remount resets the bound.
    if (this.scheduleRetry()) {
      return
    }

    this.postParentMessage({
      type: 'KERNEL_CONNECTION_TIMEOUT',
      reason: `${stage} timeout`,
      iceConnectionState: this._peer?.iceConnectionState ?? this._state,
      connectionState: this._peer?.connectionState,
      signalingState: this._peer?.signalingState,
      socketOpen: this.socketOpen,
    })
    this.onDisconnected(new Error(`${stage} timeout`))
  }

  private giveUp(reason: Error) {
    this._gaveUp = true
    this.postParentMessage({
      type: 'KERNEL_CONNECTION_FAILED',
      reason: reason.message,
      attempts: this._connectAttempts,
      iceConnectionState: this._peer?.iceConnectionState ?? this._state,
      connectionState: this._peer?.connectionState,
      signalingState: this._peer?.signalingState,
      socketOpen: this.socketOpen,
    })
    this.onDisconnected(reason)
  }

  protected onDisconnected(reason?: Error) {
    // A disconnect before any peer was established is a failed connect, not a
    // dropped session. disconnect() clears the watchdog below, so without this
    // the parent frame would get neither KERNEL_CONNECTION_FAILED nor the
    // legacy KERNEL_CONNECTION_TIMEOUT.
    if (!this._gaveUp && !this._everConnected) {
      if (this.scheduleRetry()) {
        return
      }
      this.giveUp(reason ?? new Error('connection failed'))
      return
    }

    this.disconnect()
    this.emit('debug', `disconnected:`, reason)
    this[EVENT.DISCONNECTED](reason)
  }

  // Only failures tagged at the source are transient. Anything unclassified — a
  // peer-construction throw, an ICE failure, a server-side disconnect — is
  // deterministic enough that a retry repeats it, and reports to the parent
  // instead of looping.
  private scheduleRetry() {
    const retryable = this._failure === 'transport' || this._failure === 'signaling'
    if (!retryable || this._connectAttempts >= MAX_CONNECT_ATTEMPTS) {
      return false
    }

    this.disconnect()
    this._retry = window.setTimeout(() => {
      this._retry = undefined

      if (!this._lastAttempt) {
        return
      }
      const { url, password, displayname } = this._lastAttempt
      this.connect(url, password, displayname)
    }, RETRY_DELAY_MS)

    return true
  }

  protected [EVENT.MESSAGE](event: string, payload: any) {
    this.emit('warn', `unhandled websocket event '${event}':`, payload)
  }

  protected abstract [EVENT.RECONNECTING](): void
  protected abstract [EVENT.CONNECTING](): void
  protected abstract [EVENT.CONNECTED](): void
  protected abstract [EVENT.DISCONNECTED](reason?: Error): void
  protected abstract [EVENT.TRACK](event: RTCTrackEvent): void
  protected abstract [EVENT.DATA](data: any): void
}
