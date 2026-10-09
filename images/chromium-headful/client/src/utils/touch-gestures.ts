// Turns raw touch events on the live view overlay into semantic gestures:
// tap, long-press (release = right click, move = drag), one-finger scroll with
// momentum, and two-finger pinch/pan. Coordinates are client (CSS) pixels.

export interface Point {
  x: number
  y: number
}

export interface GestureHandlers {
  // first finger down; called before any other handler for the sequence
  onTouchBegin(p: Point): void
  // called synchronously from touchend, so it runs inside the user gesture
  onTap(p: Point): void
  onLongPress(p: Point): void
  onLongPressRelease(p: Point): void
  onDragStart(p: Point): void
  onDragMove(p: Point): void
  onDragEnd(p: Point): void
  // finger delta since the previous call; positive means the finger moved right/down
  onScroll(dx: number, dy: number, anchor: Point): void
  onPinchStart(mid: Point, distance: number): void
  onPinchMove(mid: Point, distance: number): void
  onPinchEnd(): void
}

const TAP_SLOP = 10
const LONG_PRESS_MS = 500
const VELOCITY_WINDOW_MS = 100
// a finger that rested this long before lifting does not fling
const FLING_REST_MS = 60
// per-millisecond velocity decay, close to iOS UIScrollView's "normal" rate
const FLING_DECAY = 0.9975
const FLING_MIN_START = 0.15
const FLING_MIN_STOP = 0.02

type Mode = 'idle' | 'pending' | 'scroll' | 'held' | 'drag' | 'pinch'

interface Sample extends Point {
  t: number
}

function point(t: Touch): Point {
  return { x: t.clientX, y: t.clientY }
}

function distance(a: Point, b: Point) {
  return Math.hypot(a.x - b.x, a.y - b.y)
}

function midpoint(a: Point, b: Point): Point {
  return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 }
}

export class TouchGestures {
  private mode: Mode = 'idle'
  private start: Point = { x: 0, y: 0 }
  private last: Point = { x: 0, y: 0 }
  private samples: Sample[] = []
  private longPressTimer?: ReturnType<typeof setTimeout>
  private flingFrame?: number
  private interruptedFling = false

  constructor(private handlers: GestureHandlers) {}

  get flinging() {
    return this.flingFrame !== undefined
  }

  touchStart(e: TouchEvent) {
    if (e.touches.length === 1 && this.mode === 'idle') {
      this.interruptedFling = this.stopFling()
      const p = point(e.touches[0])
      this.start = p
      this.last = p
      this.samples = [{ ...p, t: e.timeStamp }]
      this.mode = 'pending'
      this.handlers.onTouchBegin(p)
      this.longPressTimer = setTimeout(() => {
        this.longPressTimer = undefined
        if (this.mode === 'pending') {
          this.mode = 'held'
          this.handlers.onLongPress(this.start)
        }
      }, LONG_PRESS_MS)
      return
    }

    if (e.touches.length >= 2 && this.mode !== 'pinch') {
      this.clearLongPress()
      this.stopFling()
      if (this.mode === 'drag') this.handlers.onDragEnd(this.last)
      this.mode = 'pinch'
      const a = point(e.touches[0])
      const b = point(e.touches[1])
      this.handlers.onPinchStart(midpoint(a, b), distance(a, b))
    }
  }

  touchMove(e: TouchEvent) {
    if (this.mode === 'pinch') {
      if (e.touches.length >= 2) {
        const a = point(e.touches[0])
        const b = point(e.touches[1])
        this.handlers.onPinchMove(midpoint(a, b), distance(a, b))
      }
      return
    }

    if (e.touches.length !== 1) return
    const p = point(e.touches[0])

    switch (this.mode) {
      case 'pending':
        if (distance(p, this.start) <= TAP_SLOP) return
        this.clearLongPress()
        this.mode = 'scroll'
        this.handlers.onScroll(p.x - this.last.x, p.y - this.last.y, this.start)
        break
      case 'scroll':
        this.handlers.onScroll(p.x - this.last.x, p.y - this.last.y, this.start)
        break
      case 'held':
        if (distance(p, this.start) <= TAP_SLOP) return
        this.mode = 'drag'
        this.handlers.onDragStart(this.start)
        this.handlers.onDragMove(p)
        break
      case 'drag':
        this.handlers.onDragMove(p)
        break
    }

    this.last = p
    this.samples.push({ ...p, t: e.timeStamp })
    while (this.samples.length > 2 && e.timeStamp - this.samples[0].t > VELOCITY_WINDOW_MS) {
      this.samples.shift()
    }
  }

  touchEnd(e: TouchEvent) {
    // wait for every finger to lift; a leftover finger after a pinch must not scroll
    if (e.touches.length > 0) return
    this.clearLongPress()

    const mode = this.mode
    this.mode = 'idle'

    switch (mode) {
      case 'pending':
        if (!this.interruptedFling) this.handlers.onTap(this.last)
        break
      case 'held':
        this.handlers.onLongPressRelease(this.last)
        break
      case 'drag':
        this.handlers.onDragEnd(this.last)
        break
      case 'scroll':
        this.startFling(e.timeStamp)
        break
      case 'pinch':
        this.handlers.onPinchEnd()
        break
    }
  }

  touchCancel() {
    this.clearLongPress()
    if (this.mode === 'drag') this.handlers.onDragEnd(this.last)
    if (this.mode === 'pinch') this.handlers.onPinchEnd()
    this.mode = 'idle'
  }

  // returns true if a fling was running
  stopFling() {
    if (this.flingFrame === undefined) return false
    cancelAnimationFrame(this.flingFrame)
    this.flingFrame = undefined
    return true
  }

  destroy() {
    this.clearLongPress()
    this.stopFling()
  }

  private clearLongPress() {
    if (this.longPressTimer !== undefined) {
      clearTimeout(this.longPressTimer)
      this.longPressTimer = undefined
    }
  }

  private startFling(endTime: number) {
    const first = this.samples[0]
    const last = this.samples[this.samples.length - 1]
    if (!first || !last || endTime - last.t > FLING_REST_MS) return
    const dt = last.t - first.t
    if (dt <= 0) return

    let vx = (last.x - first.x) / dt
    let vy = (last.y - first.y) / dt
    if (Math.hypot(vx, vy) < FLING_MIN_START) return

    const anchor = this.start
    let prev = performance.now()
    const step = (now: number) => {
      const elapsed = Math.min(now - prev, 50)
      prev = now
      const decay = Math.pow(FLING_DECAY, elapsed)
      vx *= decay
      vy *= decay
      this.handlers.onScroll(vx * elapsed, vy * elapsed, anchor)
      if (Math.hypot(vx, vy) < FLING_MIN_STOP) {
        this.flingFrame = undefined
        return
      }
      this.flingFrame = requestAnimationFrame(step)
    }
    this.flingFrame = requestAnimationFrame(step)
  }
}
