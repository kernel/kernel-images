// Keeps the video's scale while the soft keyboard is open and moves it up
// instead, the way Safari scrolls a focused field into view. Coordinates are
// client (CSS) pixels.

// how far above the keyboard the tapped point stays, so the field under it shows too
export const ANCHOR_GAP = 96
const TOP_MARGIN = 16

export interface Size {
  width: number
  height: number
}

export interface KeyboardView {
  // the player's top and height without any pan
  top: number
  height: number
  // the last tap, measured from the player's top; null if there was none
  anchor: number | null
  current: number
  // the part of the viewport the keyboard leaves visible
  visibleTop: number
  visibleBottom: number
}

// While the keyboard is open, a frame that only got shorter is the keyboard
// taking room, not a new layout to fit the stream into.
export function keyboardShrink(open: boolean, current: Size, next: Size) {
  return open && next.width === current.width && next.height < current.height
}

// How far to move the video up. The current pan is kept while the tapped point
// stays visible, and the video never moves further than its covered part.
export function keyboardPan(view: KeyboardView) {
  const max = Math.max(0, view.top + view.height - view.visibleBottom)
  let pan = view.current
  if (view.anchor !== null) {
    const y = view.top + view.anchor
    pan = Math.max(pan, y + ANCHOR_GAP - view.visibleBottom)
    pan = Math.min(pan, y - TOP_MARGIN - view.visibleTop)
  }
  return Math.round(Math.max(0, Math.min(max, pan)))
}
