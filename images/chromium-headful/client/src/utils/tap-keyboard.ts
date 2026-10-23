// Decides whether a tap on the live view raises the soft keyboard. The signal is
// the remote cursor shape: Chromium shows an I-beam over text fields (and over
// selectable page text, which is the false positive), and the pointer moves to
// the touch point on touchstart. Over a real network the cursor update usually
// arrives after the finger lifts, and iOS ignores focus() outside a gesture, so
// there the overlay is focused inside the tap with inputmode="none", which shows
// no keyboard, and removing inputmode later raises it. If the guess misses a
// field, the keyboard button opens the keyboard.

import { Point } from './touch-gestures'
import { CursorKind } from './cursor-shape'

export type TapKeyboardMode = 'auto' | 'always' | 'off'

// how long after a tap a text cursor can still raise the keyboard
export const CURSOR_WAIT_MS = 800
// how long after Enter the cursor has to leave the I-beam for the keyboard to close
export const SUBMIT_WAIT_MS = 1000
// remote px; the smallest radius for the spot where a text cursor was seen
const TEXT_SPOT_MIN = 16
// a tap this close (CSS px on screen) to where a text cursor was seen raises the keyboard
const TEXT_SPOT_RADIUS = 12

export interface TapKeyboardOutputs {
  keyboardOpen(): boolean
  // raise the keyboard, from a held focus if there is one
  focus(): void
  // focus without a keyboard; false if the focus did not take
  hold(): boolean
  // blur the overlay: closes the keyboard or ends a held focus
  release(): void
}

export interface Clock {
  now(): number
  setTimeout(fn: () => void, ms: number): number
  clearTimeout(id: number): void
}

interface Tap {
  // remote point, for the text spot
  here: Point
  // remote px per CSS px when tapped
  scale: number
}

type State =
  | { name: 'idle' }
  | { name: 'waiting'; tap: Tap; heldFocus: boolean; timer: number }
  | { name: 'submitted'; timer: number }

export class TapKeyboard {
  private state: State = { name: 'idle' }
  private cursorKind: CursorKind = 'unknown'
  private cursorChangedAt = -Infinity
  private touchBeganAt = 0
  // A text cursor before this time may be for the previous tap's position, whose
  // update can still be in flight when the pointer moves on, and the latest
  // text cursor arrived in that window.
  private staleUntil = -Infinity
  // only says something about the touch it arrived during
  private cursorStale = false
  // remote position where a text cursor was last seen after a tap
  private textSpot: Point | null = null

  constructor(
    private mode: TapKeyboardMode,
    private focusNeedsGesture: boolean,
    private out: TapKeyboardOutputs,
    private clock: Clock,
  ) {}

  // the overlay is focused with inputmode="none" while a tap waits for the cursor
  get holding() {
    return this.state.name === 'waiting' && this.state.heldFocus
  }

  // A new touch ends the wait for the previous tap's cursor, so it cannot
  // raise the keyboard during a scroll, pinch or drag.
  touchBegin() {
    this.touchBeganAt = this.clock.now()
    if (this.state.name === 'waiting') this.staleUntil = this.touchBeganAt + CURSOR_WAIT_MS
    this.cancel()
  }

  // Runs inside touchend, the only place iOS lets focus() raise the keyboard.
  tap(here: Point, scale: number) {
    this.cancel()
    if (this.mode === 'off') return
    if (this.mode === 'always') {
      this.out.focus()
      return
    }

    const tap = { here, scale }
    if (this.cursorKind === 'text' && this.cursorChangedAt >= this.touchBeganAt && !this.cursorStale) {
      this.textSpot = here
      this.offer(true)
      return
    }

    let heldFocus = false
    if (this.nearTextSpot(tap)) {
      // the tap may still have moved the pointer off the field, which the end
      // of the window catches
      this.offer(true)
    } else if (!this.out.keyboardOpen() && this.focusNeedsGesture) {
      // marked first: the focus event the hold causes must not count as the keyboard opening
      this.state = { name: 'waiting', tap, heldFocus: true, timer: 0 }
      heldFocus = this.out.hold()
    }

    // Pages often flash another cursor before the I-beam, so any text cursor
    // within the window counts; only the end of the window means "not text".
    const timer = this.clock.setTimeout(this.endWait, CURSOR_WAIT_MS)
    this.state = { name: 'waiting', tap, heldFocus, timer }
  }

  cursor(kind: CursorKind) {
    this.cursorKind = kind
    this.cursorChangedAt = this.clock.now()
    this.cursorStale = kind === 'text' && this.cursorChangedAt < this.staleUntil
    if (kind !== 'text') return

    const state = this.state
    // a possibly stale I-beam is left to the end of the window, when a later
    // cursor may have replaced it
    if (state.name === 'waiting' && !this.cursorStale) {
      this.clock.clearTimeout(state.timer)
      this.state = { name: 'idle' }
      this.textSpot = state.tap.here
      this.offer(state.heldFocus)
    }
  }

  // Enter was sent while the keyboard is open. If the I-beam under the last
  // tap goes away, the form was submitted or the page navigated, so the
  // keyboard closes; a chat box or textarea keeps its I-beam and the keyboard.
  submit() {
    if (!this.out.keyboardOpen() || this.cursorKind !== 'text') return
    this.cancel()
    this.state = { name: 'submitted', timer: this.clock.setTimeout(this.endSubmit, SUBMIT_WAIT_MS) }
  }

  // the overlay lost focus, so a held focus is gone
  blur() {
    if (this.state.name === 'waiting') this.state.heldFocus = false
  }

  // ends any wait, releasing a held focus
  cancel() {
    const state = this.state
    this.state = { name: 'idle' }
    if (state.name === 'idle') return
    this.clock.clearTimeout(state.timer)
    if (state.name === 'waiting' && state.heldFocus) this.out.release()
  }

  // No new text cursor showed up for the tap. An I-beam that was already
  // showing stays put when the tap lands on text again, so only a non-text
  // cursor means the tap was not on a text field.
  private endWait = () => {
    const state = this.state
    if (state.name !== 'waiting') return
    this.state = { name: 'idle' }
    if (this.cursorKind === 'text' && this.cursorStale && this.cursorChangedAt >= this.touchBeganAt) {
      // no later cursor settled it, so leave it to the keyboard button
      if (state.heldFocus) this.out.release()
      return
    }
    if (this.cursorKind === 'text') {
      this.textSpot = state.tap.here
      this.offer(state.heldFocus)
      return
    }
    if (this.nearTextSpot(state.tap)) this.textSpot = null
    if (this.out.keyboardOpen() || state.heldFocus) this.out.release()
  }

  private endSubmit = () => {
    if (this.state.name !== 'submitted') return
    this.state = { name: 'idle' }
    if (this.cursorKind !== 'text' && this.out.keyboardOpen()) this.out.release()
  }

  // inGesture: running inside the tap, or from a focus held since then. iOS
  // ignores a focus outside both.
  private offer(inGesture: boolean) {
    if (this.out.keyboardOpen()) return
    if (inGesture || !this.focusNeedsGesture) this.out.focus()
  }

  private nearTextSpot(tap: Tap) {
    if (!this.textSpot) return false
    const radius = Math.max(TEXT_SPOT_MIN, TEXT_SPOT_RADIUS * tap.scale)
    return Math.hypot(tap.here.x - this.textSpot.x, tap.here.y - this.textSpot.y) <= radius
  }
}
