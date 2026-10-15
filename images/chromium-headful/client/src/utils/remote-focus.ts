// The remote browser's focused editable element and the editable fields on
// screen, streamed by the kernel-images API at /live-view/focus. Rects are in
// remote screen pixels. When the stream is unavailable (an older image, a live
// view not served through the browser proxy) the live view falls back to the
// cursor-shape guess.

import { Point } from './touch-gestures'

export interface FieldRect {
  x: number
  y: number
  w: number
  h: number
}

export interface RemoteField {
  // an input's type, "textarea" or "contenteditable"
  type: string
  inputMode?: string
  autocomplete?: string
  enterKeyHint?: string
  autocapitalize?: string
  autocorrect?: string
  spellcheck?: string
  // a one-time code field
  code?: boolean
  rect?: FieldRect
}

export interface FocusState {
  focus: RemoteField | null
  fields: RemoteField[]
  // the shown tab's page area; outside it is browser UI such as the address bar
  viewport: FieldRect | null
  // the page has keyboard focus, which the address bar and other browser UI take
  pageFocused: boolean
  screen: { w: number; h: number } | null
}

export interface StreamSize {
  w: number
  h: number
}

type EventSourceLike = Pick<EventSource, 'onmessage' | 'onerror' | 'addEventListener' | 'close'>

// the server pings every 15s
export const STREAM_STALE_MS = 40000
// a tap this close (remote px) to a field's edge still counts as on the field
const FIELD_SLOP = 4
// Rects sent before a scroll are where the fields were. A scroll that moved
// fields is reported well within this; after it, rects that did not change
// are still right.
const SCROLL_SETTLE_MS = 1000

// The stream's URL, and the one that blurs the remote's focused field, next
// to the live view's own: the browser proxy serves the live view at
// /browser/live and the kernel-images API at /browser/kernel, both with the
// session token.
export function focusUrls(loc: { origin: string; pathname: string; search: string }) {
  const params = new URLSearchParams(loc.search)
  const jwt = params.get('jwt')
  if (!jwt || params.get('focusSignal') === 'off' || loc.pathname.replace(/\/$/, '') !== '/browser/live') return null
  const base = `${loc.origin}/browser/kernel/live-view/focus`
  const token = `?jwt=${encodeURIComponent(jwt)}`
  return { stream: base + token, blur: `${base}/blur${token}` }
}

export class RemoteFocus {
  state: FocusState | null = null
  private source: EventSourceLike | null = null
  private heardAt = -Infinity
  private fieldsAt = -Infinity
  private fieldsJson = ''
  private scrolledAt = -Infinity

  constructor(
    private url: string,
    private open: (url: string) => EventSourceLike,
    private now: () => number,
    private onState: (state: FocusState) => void,
  ) {}

  get running() {
    return this.source !== null
  }

  start() {
    if (this.source) return
    const source = this.open(this.url)
    this.source = source
    source.onmessage = (e) => this.receive(e.data)
    source.addEventListener('ping', () => (this.heardAt = this.now()))
    // EventSource reconnects by itself. Until it does nothing is known, which
    // is not the same as a blur, so the keyboard is left as it is.
    source.onerror = () => this.forget()
  }

  stop() {
    if (!this.source) return
    this.source.close()
    this.source = null
    this.forget()
  }

  // The stream is current and sees a tab. Otherwise taps fall back to the
  // cursor shape.
  get live() {
    return this.state !== null && this.state.viewport !== null && this.now() - this.heardAt < STREAM_STALE_MS
  }

  // the client sent a scroll, which moves the fields
  scrolled() {
    this.scrolledAt = this.now()
  }

  inViewport(p: Point, stream: StreamSize) {
    const state = this.state
    if (!this.live || !state || !state.viewport) return false
    return contains(this.toStream(state.viewport, stream), p, 0)
  }

  // The field a tap at p lands on, by the last rects sent. The focused field
  // counts wherever it is, even when something covers its center.
  fieldAt(p: Point, stream: StreamSize): RemoteField | null {
    const state = this.state
    if (!this.live || !state) return null
    const settled = this.fieldsAt > this.scrolledAt || this.now() - this.scrolledAt > SCROLL_SETTLE_MS
    if (!settled) return null
    const fields = state.focus ? [state.focus, ...state.fields] : state.fields
    for (const field of fields) {
      if (field.rect && contains(this.toStream(field.rect, stream), p, FIELD_SLOP)) return field
    }
    return null
  }

  private receive(data: string) {
    let state: FocusState
    try {
      state = JSON.parse(data)
    } catch (e) {
      return
    }
    this.heardAt = this.now()
    const fieldsJson = JSON.stringify(state.fields)
    if (fieldsJson !== this.fieldsJson) {
      this.fieldsJson = fieldsJson
      this.fieldsAt = this.heardAt
    }
    this.state = state
    this.onState(state)
  }

  private forget() {
    this.state = null
    this.fieldsJson = ''
  }

  private toStream(rect: FieldRect, stream: StreamSize): FieldRect {
    const screen = this.state && this.state.screen
    const sx = screen && screen.w ? stream.w / screen.w : 1
    const sy = screen && screen.h ? stream.h / screen.h : 1
    return { x: rect.x * sx, y: rect.y * sy, w: rect.w * sx, h: rect.h * sy }
  }
}

function contains(rect: FieldRect, p: Point, slop: number) {
  return p.x >= rect.x - slop && p.x <= rect.x + rect.w + slop && p.y >= rect.y - slop && p.y <= rect.y + rect.h + slop
}
