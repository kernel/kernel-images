// Camera passthrough controller.
//
// Lets a live-view viewer share their real camera into the browser so a page
// inside it (e.g. an identity-verification flow where the person is present)
// can use it. The browser's camera manager (server lib/camera) reports, via a
// state file, whether a page currently wants the camera; this controller starts
// and stops the viewer's camera to match (camera on demand), on top of an
// explicit one-time consent.
//
// Consent model: the viewer must Share once per session (a manual toggle). Only
// after that does a page's request auto-start the camera — a page cannot turn
// on a real person's webcam on its own (e.g. by calling enumerateDevices()).
//
// Scope: this relays a real person's real camera. It is not a way to feed
// synthetic or recorded video to a liveness check.

import { NekoClient } from '~/neko'

interface CameraState {
  wanted: boolean
  streaming: boolean
  width: number
  height: number
}

const STATE_POLL_MS = 700
const STOP_GRACE_MS = 3000
// The browser's camera mode. neko scales whatever it receives straight to this
// size without keeping the aspect ratio, so the controller always sends exactly
// this shape (see toLandscape).
const WIDTH = 1280
const HEIGHT = 720

// toLandscape returns a WIDTH x HEIGHT track that carries source upright and
// undistorted: a portrait phone camera is pillarboxed rather than squashed, and
// rotating the phone just changes the source's shape, with no re-acquire.
// The image is not mirrored; like any webcam, the page receives the scene as
// others see it.
export function toLandscape(source: MediaStreamTrack): { track: MediaStreamTrack; stop: () => void } {
  const video = document.createElement('video')
  video.muted = true
  video.playsInline = true
  // Kept in the DOM (invisibly) so mobile browsers keep decoding it.
  video.style.cssText = 'position:fixed;width:1px;height:1px;opacity:0;pointer-events:none'
  video.srcObject = new MediaStream([source])
  document.body.appendChild(video)

  const canvas = document.createElement('canvas')
  canvas.width = WIDTH
  canvas.height = HEIGHT
  const ctx = canvas.getContext('2d')!
  const out = canvas.captureStream(30)
  let live = true

  const schedule = () => {
    if (!live) return
    if ('requestVideoFrameCallback' in video) video.requestVideoFrameCallback(draw)
    else requestAnimationFrame(draw)
  }
  const draw = () => {
    const w = video.videoWidth
    const h = video.videoHeight
    if (w && h) {
      const scale = Math.min(WIDTH / w, HEIGHT / h)
      ctx.fillStyle = '#000'
      ctx.fillRect(0, 0, WIDTH, HEIGHT)
      ctx.drawImage(video, (WIDTH - w * scale) / 2, (HEIGHT - h * scale) / 2, w * scale, h * scale)
    }
    schedule()
  }
  video.play().then(schedule, schedule)

  return {
    track: out.getVideoTracks()[0],
    stop: () => {
      live = false
      out.getTracks().forEach((t) => t.stop())
      video.srcObject = null
      video.remove()
    },
  }
}

export class CameraController {
  private client: NekoClient
  private stream: MediaStream | null = null
  private shaped: { track: MediaStreamTrack; stop: () => void } | null = null
  private sender: RTCRtpSender | null = null
  private starting = false
  // gen advances on every stop so a start whose getUserMedia is still pending
  // when stop()/unmount() runs tears down the stream it acquired.
  private gen = 0
  private stopped = false
  // armed becomes true after the first manual share this session; auto-follow is
  // gated on it so a page request alone never turns on the camera.
  private armed = false
  // keepOn is set while the viewer has manually turned the camera on; it
  // suppresses auto-stop. userOff is set when they manually turn it off and
  // blocks auto-start until they share again.
  private keepOn = false
  private userOff = false
  private idleSince = 0
  private timer: number | null = null

  constructor(client: NekoClient) {
    this.client = client
  }

  // mount begins polling the on-demand signal.
  mount() {
    this.stopped = false
    this.poll()
  }

  unmount() {
    this.stopped = true
    if (this.timer !== null) {
      window.clearTimeout(this.timer)
      this.timer = null
    }
    this.stop()
  }

  // toggle is the manual control. Turning on arms auto-follow and keeps the
  // camera on regardless of page requests; turning off disarms it, so the next
  // page request will not auto-start until the viewer shares again.
  async toggle(): Promise<void> {
    if (this.stream || this.starting) {
      this.keepOn = false
      this.armed = false
      this.userOff = true
      this.stop()
    } else {
      this.armed = true
      this.keepOn = true
      this.userOff = false
      await this.start()
    }
  }

  get active(): boolean {
    return this.stream !== null
  }

  private async start(): Promise<void> {
    if (this.stream || this.starting || this.stopped || !this.client.peerConnected) return
    this.starting = true
    const gen = this.gen
    try {
      const stream = await navigator.mediaDevices.getUserMedia({
        video: { width: { ideal: WIDTH }, height: { ideal: HEIGHT }, frameRate: { ideal: 30 } },
        audio: false,
      })
      if (gen !== this.gen || this.stopped) {
        // stop()/unmount() ran while getUserMedia was pending; discard it.
        stream.getTracks().forEach((t) => t.stop())
        return
      }
      this.stream = stream
      this.shaped = toLandscape(stream.getVideoTracks()[0])
      this.sender = this.client.shareLocalCamera(this.shaped.track, new MediaStream([this.shaped.track])) ?? null
      this.emitChange()
    } catch (err) {
      this.stream = null
      this.sender = null
      this.emitError(err)
    } finally {
      this.starting = false
    }
  }

  private stop(): void {
    this.gen++
    if (this.sender) {
      this.client.unshareLocalCamera(this.sender)
      this.sender = null
    }
    if (this.shaped) {
      this.shaped.stop()
      this.shaped = null
    }
    if (this.stream) {
      this.stream.getTracks().forEach((t) => t.stop())
      this.stream = null
    }
    this.emitChange()
  }

  private async poll(): Promise<void> {
    try {
      const state = await this.fetchState()
      if (state) {
        if (state.wanted) {
          this.idleSince = 0
          if (this.armed && !this.userOff && !this.stream) await this.start()
        } else {
          // The page released the camera: clear the manual-off latch so a
          // future request can auto-follow, and auto-stop unless the viewer is
          // holding it on.
          this.userOff = false
          if (this.stream && !this.keepOn) {
            this.idleSince = this.idleSince || Date.now()
            if (Date.now() - this.idleSince > STOP_GRACE_MS) {
              this.stop()
              this.idleSince = 0
            }
          }
        }
      }
    } catch {
      // ignore transient fetch errors; the manager may be starting up
    }
    if (!this.stopped) {
      this.timer = window.setTimeout(() => this.poll(), STATE_POLL_MS)
    }
  }

  private async fetchState(): Promise<CameraState | null> {
    // The state file is served by the live-view server alongside the page, so a
    // relative URL reaches it with the page's own path and query (the live-view
    // origin is what authenticates the session).
    const base = window.location.pathname.replace(/\/?$/, '/')
    const sep = window.location.search ? window.location.search + '&' : '?'
    const res = await fetch(`${base}camera-state.json${sep}t=${Date.now()}`, { cache: 'no-store' })
    if (!res.ok) return null
    return (await res.json()) as CameraState
  }

  private emitChange(): void {
    window.dispatchEvent(new CustomEvent('kernel:camera', { detail: { active: this.active } }))
  }

  private emitError(err: unknown): void {
    window.dispatchEvent(new CustomEvent('kernel:camera-error', { detail: { error: err } }))
  }
}
