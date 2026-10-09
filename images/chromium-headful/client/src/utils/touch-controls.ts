// Layout math for the touch controls. The control prefers a band outside the
// video (shrinking the video a little to make one if needed) and only
// overlays the stream when there is no room.

export const CONTROL_SIZE = 44
// a 44px target plus a 4px margin on each side
export const BAND_SIZE = 52
// how much the video may shrink to make room for a band
export const MAX_VIDEO_SHRINK = 0.1
export const EDGE_MARGIN = 8

export type ControlSide = 'left' | 'right'
export type ControlMode = 'bottom' | ControlSide | 'overlay'

export interface ControlLayout {
  mode: ControlMode
  // reserved band thickness in px, including the safe-area inset; 0 for overlay
  band: number
}

export interface ControlPosition {
  side: ControlSide
  // 0..1 along the bottom band
  x: number
  // 0..1 along a side strip or screen edge
  y: number
}

export interface Insets {
  bottom: number
  left: number
  right: number
}

export const DEFAULT_POSITION: ControlPosition = { side: 'left', x: 0, y: 0.5 }

// videoWidth/videoHeight: the size the video gets in the whole area, before any band is reserved
export function controlLayout(
  areaWidth: number,
  areaHeight: number,
  videoWidth: number,
  videoHeight: number,
  insets: Insets,
  side: ControlSide,
): ControlLayout {
  const bottom = BAND_SIZE + insets.bottom
  const strip = BAND_SIZE + insets[side]

  if (areaHeight - videoHeight >= bottom) return { mode: 'bottom', band: bottom }
  if (areaWidth - videoWidth >= strip) return { mode: side, band: strip }

  // scale the video would need to fit next to each band
  const fit = (width: number, height: number) => Math.min(1, width / videoWidth, height / videoHeight)
  const bottomScale = fit(areaWidth, areaHeight - bottom)
  const stripScale = fit(areaWidth - strip, areaHeight)
  const best = Math.max(bottomScale, stripScale)
  if (best < 1 - MAX_VIDEO_SHRINK) return { mode: 'overlay', band: 0 }
  return bottomScale >= stripScale ? { mode: 'bottom', band: bottom } : { mode: side, band: strip }
}

function fraction(value: number, length: number) {
  const range = length - CONTROL_SIZE - 2 * EDGE_MARGIN
  return range > 0 ? Math.min(1, Math.max(0, (value - CONTROL_SIZE / 2 - EDGE_MARGIN) / range)) : 0.5
}

// Where a control dropped with its center at (x, y) ends up. Overlay and side
// strips snap to the nearest side; the bottom band only moves along its length.
export function dropControl(
  mode: ControlMode,
  position: ControlPosition,
  x: number,
  y: number,
  width: number,
  height: number,
): ControlPosition {
  if (mode === 'bottom') return { ...position, x: fraction(x, width) }
  return { ...position, side: x < width / 2 ? 'left' : 'right', y: fraction(y, height) }
}

export function encodePosition(position: ControlPosition) {
  return `${position.side}:${Math.round(position.x * 1000)}:${Math.round(position.y * 1000)}`
}

export function decodePosition(value: string): ControlPosition {
  const [side, x, y] = value.split(':')
  const unit = (v: string, def: number) => {
    const n = parseInt(v, 10) / 1000
    return isNaN(n) ? def : Math.min(1, Math.max(0, n))
  }
  return {
    side: side === 'right' ? 'right' : 'left',
    x: unit(x, DEFAULT_POSITION.x),
    y: unit(y, DEFAULT_POSITION.y),
  }
}
