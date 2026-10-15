// Tells whether a soft keyboard is actually up, from the visual viewport it
// covers (iOS) or shrinks (Android). A focused overlay does not mean that:
// iOS can leave it focused with the keyboard dismissed, or refuse to show the
// keyboard for a focus.

// Covering less than this (screen pixels) is not a keyboard, such as the
// shortcut bar iOS shows with a hardware keyboard.
export const KEYBOARD_MIN_HEIGHT = 120

export interface VisualViewport {
  width: number
  height: number
  scale: number
}

export class SoftKeyboard {
  up = false
  // A keyboard has been seen in the viewport. Until then nothing is known: a
  // live view embedded in a frame, or one typed into with a hardware keyboard,
  // may never see one.
  measurable = false
  private width = -1
  // the tallest viewport seen at this width, the height with nothing covering it
  private fullHeight = 0

  // A viewport, with the layout viewport's height, which the keyboard does
  // not shrink. Sizes are taken in screen pixels, which a zoom does not change:
  // iOS can zoom the page when a field first takes focus, which changes the
  // viewport's size in CSS pixels as the keyboard comes up. Returns 'up' or
  // 'down' when the keyboard came or went.
  measure(viewport: VisualViewport, layoutHeight: number): 'up' | 'down' | null {
    const width = viewport.width * viewport.scale
    const height = viewport.height * viewport.scale
    // rounding in the reported sizes is not a rotation
    if (Math.abs(width - this.width) > 2) {
      // after a rotation, which can come with the keyboard going, the layout
      // viewport is the best guess of the height with nothing covering it
      this.width = width
      this.fullHeight = Math.max(height, layoutHeight * viewport.scale)
    }
    this.fullHeight = Math.max(this.fullHeight, height)
    const up = this.fullHeight - height >= KEYBOARD_MIN_HEIGHT
    if (up) this.measurable = true
    if (up === this.up) return null
    this.up = up
    return up ? 'up' : 'down'
  }
}
