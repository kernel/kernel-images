// Raises and closes the soft keyboard from the remote browser's focus, for
// taps on the page while the focus stream is live. iOS only raises the
// keyboard for a focus inside the tap, and the focus report arrives after the
// finger lifts, so:
//  - a tap on a field from the last rects sent raises the keyboard inside the
//    tap, set up for that field, and the focus report confirms it;
//  - on iOS any other tap focuses the overlay with inputmode="none", and a
//    focus report within the wait raises the keyboard from that focus;
//  - when the wait ends without a focused field, the keyboard closes.
// A keyboard raised for a field closes when the field loses focus, including
// by a submit that navigates; a chat box that keeps focus keeps it open. A
// keyboard raised for the browser's own UI, such as the address bar, closes
// when the page gets focus back, as it does when the address bar navigates.

import { Clock, TapKeyboardMode } from './tap-keyboard'
import { RemoteField } from './remote-focus'
import { keyboardFor } from './field-keyboard'

// how long after a tap a focus report can still raise the keyboard
export const FOCUS_WAIT_MS = 1000
// a blur followed by a focus this soon (a re-rendered field) keeps the keyboard
export const BLUR_GRACE_MS = 300

export interface FocusKeyboardOutputs {
  keyboardOpen(): boolean
  // Raise the keyboard set up for field (null: the default keyboard), from a
  // held focus if there is one. inGesture: running inside the tap.
  focus(field: RemoteField | null, inGesture: boolean): void
  // set up the open keyboard for another field
  update(field: RemoteField): void
  // focus without a keyboard; false if the focus did not take
  hold(): boolean
  // blur the overlay: closes the keyboard or ends a held focus
  release(): void
}

type Wait = { timer: number; heldFocus: boolean }

// What the open keyboard is for, which decides what closes it: a focused
// remote field, the browser's UI, or nothing known (the keyboard button with
// nothing focused).
type Owner = 'field' | 'browser' | 'none'

export class FocusKeyboard {
  // the remote's focused field, from the latest report
  private focused: RemoteField | null = null
  private wait: Wait | null = null
  private blurTimer = 0
  private owner: Owner = 'none'
  // the field the open keyboard is set up for
  private keyboardField: RemoteField | null = null
  private pageFocused = true
  // the page lost focus since the last tap on the browser's UI
  private pageLeft = false

  constructor(
    private mode: TapKeyboardMode,
    private focusNeedsGesture: boolean,
    private out: FocusKeyboardOutputs,
    private clock: Clock,
  ) {}

  get holding() {
    return this.wait !== null && this.wait.heldFocus
  }

  get field() {
    return this.focused
  }

  // a new touch ends the wait, so a scroll or pinch cannot raise the keyboard
  touchBegin() {
    this.cancel()
  }

  // Runs inside touchend. field: the field under the tap by the last rects
  // sent, which can be out of date.
  tap(field: RemoteField | null) {
    this.cancel()
    // the page's focus decides from here
    if (this.owner === 'browser') this.owner = 'none'
    if (this.mode === 'off') return
    if (this.mode === 'always') {
      this.raise(field || this.focused, true)
      return
    }

    if (field) {
      // after the focus, which may blur and refocus the overlay to set it up
      this.raise(field, true)
      this.owner = 'field'
      this.startWait(false)
      return
    }
    let heldFocus = false
    if (!this.out.keyboardOpen() && this.focusNeedsGesture) {
      // marked first: the focus event the hold causes must not count as the keyboard opening
      this.wait = { timer: 0, heldFocus: true }
      heldFocus = this.out.hold()
    }
    this.startWait(heldFocus)
  }

  // the remote's focused field and whether the page has focus at all, on every report
  focus(field: RemoteField | null, pageFocused: boolean) {
    this.focused = field
    this.pageFocused = pageFocused
    if (this.owner === 'browser') {
      if (!pageFocused) {
        this.pageLeft = true
      } else if (this.pageLeft) {
        // the address bar navigated, or focus went back to the page
        this.owner = 'none'
        if (this.out.keyboardOpen()) this.out.release()
        return
      }
    }
    if (!field) {
      this.blurred()
      return
    }
    this.clock.clearTimeout(this.blurTimer)
    this.blurTimer = 0
    const wait = this.wait
    if (wait) {
      this.endWait()
      if (this.out.keyboardOpen()) {
        this.switchTo(field)
      } else if (wait.heldFocus || !this.focusNeedsGesture) {
        this.raise(field, false)
      }
      if (this.out.keyboardOpen()) this.owner = 'field'
      return
    }
    // the page moved focus, such as to the next box of a code
    if (this.out.keyboardOpen() && this.owner !== 'browser') {
      this.switchTo(field)
      this.owner = 'field'
    }
  }

  // the keyboard was opened by the button, set up for the focused field
  opened() {
    this.cancel()
    this.owner = this.focused ? 'field' : 'none'
    this.keyboardField = this.focused
  }

  // A tap the cursor shape decides about: one on the browser's own UI, or any
  // tap while the focus stream is down. A page field's blur does not close
  // the keyboard it raises. After a tap on the browser's UI, the page getting
  // focus back does.
  cursorTap(browserUi: boolean) {
    this.cancel()
    this.owner = browserUi ? 'browser' : 'none'
    this.pageLeft = !this.pageFocused
  }

  // the overlay lost focus
  blur() {
    if (this.wait) this.wait.heldFocus = false
    this.owner = 'none'
    this.keyboardField = null
    this.clock.clearTimeout(this.blurTimer)
    this.blurTimer = 0
  }

  // ends any wait, releasing a held focus
  cancel() {
    const wait = this.wait
    if (!wait) return
    this.endWait()
    if (wait.heldFocus) this.out.release()
  }

  private raise(field: RemoteField | null, inGesture: boolean) {
    this.out.focus(field, inGesture)
    this.keyboardField = field
  }

  private switchTo(field: RemoteField) {
    if (this.keyboardField && sameKeyboard(this.keyboardField, field)) return
    this.out.update(field)
    this.keyboardField = field
  }

  private startWait(heldFocus: boolean) {
    this.wait = { timer: this.clock.setTimeout(this.waitEnded, FOCUS_WAIT_MS), heldFocus }
  }

  private endWait() {
    if (this.wait) this.clock.clearTimeout(this.wait.timer)
    this.wait = null
  }

  // No field took focus for the tap, so the keyboard closes. A field that
  // already had focus and was tapped again sends no new report and keeps it.
  private waitEnded = () => {
    const wait = this.wait
    if (!wait) return
    this.wait = null
    if (this.focused) {
      if (!this.out.keyboardOpen() && (wait.heldFocus || !this.focusNeedsGesture)) this.raise(this.focused, false)
      if (this.out.keyboardOpen()) this.owner = 'field'
      return
    }
    if (this.out.keyboardOpen() || wait.heldFocus) this.out.release()
  }

  // A blur during a wait is left to the end of the wait: focus moving between
  // frames reports the old frame's blur before the new frame's focus.
  private blurred() {
    if (this.wait || this.owner !== 'field' || this.blurTimer) return
    this.blurTimer = this.clock.setTimeout(() => {
      this.blurTimer = 0
      if (!this.focused && this.owner === 'field' && !this.wait && this.out.keyboardOpen()) this.out.release()
    }, BLUR_GRACE_MS)
  }
}

function sameKeyboard(a: RemoteField, b: RemoteField) {
  return JSON.stringify(keyboardFor(a)) === JSON.stringify(keyboardFor(b))
}
