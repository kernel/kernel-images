// Camera passthrough controller.
//
// Lets a live-view viewer share their real camera into the browser so a page
// inside it (e.g. an identity-verification flow) can use it. The browser's
// camera manager (server lib/camera) reports, via /state, whether a page
// currently wants the camera; this controller starts and stops the viewer's
// camera to match (camera on demand), and offers a manual toggle.
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

export class CameraController {
  private client: NekoClient
  private stream: MediaStream | null = null
  private sender: RTCRtpSender | null = null
  private manual = false
  private starting = false
  private idleSince = 0
  private timer: number | null = null

  constructor(client: NekoClient) {
    this.client = client
  }

  // mount begins polling the on-demand signal and re-requesting the camera at a
  // matching aspect ratio when the device orientation changes.
  mount() {
    this.poll()
    window.addEventListener('orientationchange', this.onOrientationChange)
    window.matchMedia('(orientation: portrait)').addEventListener('change', this.onOrientationChange)
  }

  unmount() {
    if (this.timer !== null) window.clearTimeout(this.timer)
    window.removeEventListener('orientationchange', this.onOrientationChange)
    this.stop()
  }

  // toggle is the manual control. Manual-on keeps the camera on regardless of
  // whether a page is currently requesting it.
  async toggle(): Promise<void> {
    if (this.stream) {
      this.manual = false
      this.stop()
    } else {
      this.manual = true
      await this.start()
    }
  }

  get active(): boolean {
    return this.stream !== null
  }

  private constraints(): MediaStreamConstraints {
    // Match the shared camera's aspect to the device orientation so the browser
    // receives upright video. The server letterboxes onto its fixed native mode;
    // per-orientation native caps are a follow-up (see PR notes).
    const portrait = window.matchMedia('(orientation: portrait)').matches
    const long = 1280
    const short = 720
    return {
      video: {
        width: { ideal: portrait ? short : long },
        height: { ideal: portrait ? long : short },
        frameRate: { ideal: 30 },
      },
      audio: false,
    }
  }

  private async start(): Promise<void> {
    if (this.stream || this.starting || !this.client.peerConnected) return
    this.starting = true
    try {
      this.stream = await navigator.mediaDevices.getUserMedia(this.constraints())
      const track = this.stream.getVideoTracks()[0]
      this.sender = this.client.shareLocalCamera(track, this.stream) ?? null
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
    if (this.sender) {
      this.client.unshareLocalCamera(this.sender)
      this.sender = null
    }
    if (this.stream) {
      this.stream.getTracks().forEach((t) => t.stop())
      this.stream = null
    }
    this.emitChange()
  }

  private onOrientationChange = async (): Promise<void> => {
    // Re-acquire at the new aspect ratio so the shared video stays upright.
    if (!this.stream) return
    this.stop()
    await this.start()
  }

  private async poll(): Promise<void> {
    try {
      const state = await this.fetchState()
      if (state) {
        if (state.wanted) {
          this.idleSince = 0
          if (!this.stream) await this.start()
        } else if (this.stream && !this.manual) {
          this.idleSince = this.idleSince || Date.now()
          if (Date.now() - this.idleSince > STOP_GRACE_MS) {
            this.stop()
            this.idleSince = 0
          }
        }
      }
    } catch {
      // ignore transient fetch errors; the manager may be starting up
    }
    this.timer = window.setTimeout(() => this.poll(), STATE_POLL_MS)
  }

  private async fetchState(): Promise<CameraState | null> {
    // The state file is served alongside the live-view page, under the same
    // authenticated path, so a relative URL carries the page's credentials.
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
