// Tells whether a soft keyboard is actually up, from how much of the layout
// viewport the visual viewport leaves visible. A focused overlay does not mean
// that: iOS can leave it focused with the keyboard dismissed, or refuse to show
// the keyboard for a focus. The keyboard covers the layout viewport without
// shrinking it (iOS, and Android's default), while the browser's own toolbars
// coming and going resize both, so they are not mistaken for a keyboard.

// Covering less than this is not a keyboard, such as the shortcut bar iOS
// shows with a hardware keyboard.
export const KEYBOARD_MIN_HEIGHT = 120

// phones always have a soft keyboard; tablets may have a hardware one instead
export function isPhone(userAgent: string) {
  return /iPhone|iPod|Android.+Mobile/.test(userAgent)
}

export interface VisualViewport {
  height: number
  scale: number
}

export class SoftKeyboard {
  up = false
  // A keyboard can be seen in the viewport: on a phone, or once one has been.
  // Until then nothing is known, as with a tablet's hardware keyboard or a
  // live view embedded in a frame.
  measurable: boolean

  constructor(phone: boolean) {
    this.measurable = phone
  }

  // Returns 'up' or 'down' when the keyboard came or went. A zoomed page
  // shows less of the layout viewport without a keyboard, so its sizes say
  // nothing about one.
  measure(viewport: VisualViewport, layoutHeight: number): 'up' | 'down' | null {
    if (Math.abs(viewport.scale - 1) > 0.01) return null
    const up = layoutHeight - viewport.height >= KEYBOARD_MIN_HEIGHT
    if (up) this.measurable = true
    if (up === this.up) return null
    this.up = up
    return up ? 'up' : 'down'
  }
}
