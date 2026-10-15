// Tells whether a soft keyboard is actually up, from the visual viewport it
// covers (iOS) or shrinks (Android). A focused overlay does not mean that:
// iOS can leave it focused with the keyboard dismissed, or refuse to show the
// keyboard for a focus.

// Covering less than this is not a keyboard, such as the shortcut bar iOS
// shows with a hardware keyboard.
export const KEYBOARD_MIN_HEIGHT = 120

export class SoftKeyboard {
  up = false
  // The viewport shows the keyboard here. A top-level page's does; a live view
  // embedded in a frame may never see it, and then nothing is known.
  measurable: boolean
  private width = -1
  // the tallest viewport seen at this width, the height with nothing covering it
  private fullHeight = 0

  constructor(topLevel: boolean) {
    this.measurable = topLevel
  }

  // A viewport size, with the layout viewport's height, which the keyboard
  // does not shrink on iOS. Returns 'up' or 'down' when the keyboard came or
  // went at the same width.
  measure(width: number, height: number, layoutHeight: number): 'up' | 'down' | null {
    if (width !== this.width) {
      // a rotation keeps the keyboard as it was
      this.width = width
      this.fullHeight = Math.max(height, layoutHeight)
      return null
    }
    this.fullHeight = Math.max(this.fullHeight, height, layoutHeight)
    const up = this.fullHeight - height >= KEYBOARD_MIN_HEIGHT
    if (up) this.measurable = true
    if (up === this.up) return null
    this.up = up
    return up ? 'up' : 'down'
  }
}
