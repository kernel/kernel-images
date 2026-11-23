// Brings a dropped live view session back. A phone that locks or backgrounds
// the page loses the websocket, the WebRTC peer, or both, and nothing else
// reconnects. Attempts back off while they keep failing, wait while the page is
// hidden (a suspended page can't connect anyway), and start at once when the
// page comes back. Before each attempt the live view page is fetched again: the
// proxy answers 4xx once the browser is gone or the link no longer grants
// access, and that ends the session instead of retrying forever.

export const RECONNECT_BACKOFF_MS = [0, 1000, 2000, 4000, 8000]
export const RECONNECT_MAX_DELAY_MS = 10000
// a session that drops sooner than this after connecting keeps backing off,
// so one that connects and drops right away can't retry in a tight loop
export const RECONNECT_STABLE_MS = 10000

// ok: the page is still served. ended: the proxy refused it for good.
// unknown: a network error or 5xx, which says nothing about the session.
export type ProbeResult = 'ok' | 'ended' | 'unknown'

export interface ReconnectDeps {
  now(): number
  setTimeout(fn: () => void, ms: number): number
  clearTimeout(id: number): void
  hidden(): boolean
  probe(): Promise<ProbeResult>
  connect(): void
  // an attempt is pending; attempt counts from 1 for each drop
  onWaiting(attempt: number): void
  onEnded(): void
}

export function reconnectDelay(failures: number) {
  return failures < RECONNECT_BACKOFF_MS.length ? RECONNECT_BACKOFF_MS[failures] : RECONNECT_MAX_DELAY_MS
}

// Status codes from the live view proxy that mean the session is over.
export function probeResult(status: number): ProbeResult {
  if (status >= 200 && status < 400) return 'ok'
  if ([401, 403, 404, 410].includes(status)) return 'ended'
  return 'unknown'
}

export class Reconnector {
  private phase: 'idle' | 'waiting' | 'connecting' | 'ended' = 'idle'
  private failures = 0
  private connectedAt = -Infinity
  private timer?: number

  constructor(private deps: ReconnectDeps) {}

  get active() {
    return this.phase === 'waiting' || this.phase === 'connecting'
  }

  get ended() {
    return this.phase === 'ended'
  }

  // the session, or the attempt to restore it, dropped
  dropped() {
    if (this.phase === 'ended') return
    if (this.phase === 'connecting') this.failures++
    else if (this.phase === 'idle') {
      this.failures = this.deps.now() - this.connectedAt < RECONNECT_STABLE_MS ? this.failures + 1 : 0
    }
    this.phase = 'waiting'
    this.deps.onWaiting(this.failures + 1)
    this.schedule(reconnectDelay(this.failures))
  }

  succeeded() {
    this.clear()
    this.connectedAt = this.deps.now()
    this.phase = 'idle'
  }

  // the page became visible, was restored, or the network came back
  wake() {
    if (this.phase !== 'waiting') return
    this.clear()
    this.attempt()
  }

  stop() {
    this.clear()
    this.phase = 'ended'
  }

  private schedule(ms: number) {
    this.clear()
    this.timer = this.deps.setTimeout(() => {
      this.timer = undefined
      this.attempt()
    }, ms)
  }

  private clear() {
    if (this.timer === undefined) return
    this.deps.clearTimeout(this.timer)
    this.timer = undefined
  }

  private async attempt() {
    if (this.phase !== 'waiting' || this.deps.hidden()) return
    this.phase = 'connecting'
    const result = await this.deps.probe()
    if (this.phase !== 'connecting') return
    if (result === 'ended') {
      this.phase = 'ended'
      this.deps.onEnded()
      return
    }
    this.deps.connect()
  }
}
