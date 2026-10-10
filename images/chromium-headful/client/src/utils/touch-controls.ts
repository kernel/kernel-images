// Layout math for the touch controls. The control prefers a band outside the
// video (shrinking the video a little to make one if needed) and only
// overlays the stream when there is no room.

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

export interface Size {
  width: number
  height: number
}

export interface SafeArea extends Insets {
  top: number
}

export interface ControlFrame {
  area: Size
  control: Size
  safe: SafeArea
  // height of the soft keyboard covering the bottom of the area
  keyboardInset: number
}

// gap between the control and the edge it is docked to
const DOCK_GAP = { band: 4, overlay: 6 }

interface Track {
  start: number
  length: number
}

// the range the control's leading edge can move along
function track(mode: ControlMode, frame: ControlFrame): Track {
  const { area, control, safe, keyboardInset } = frame
  if (mode === 'bottom') {
    const start = safe.left + EDGE_MARGIN
    return { start, length: Math.max(0, area.width - safe.right - EDGE_MARGIN - control.width - start) }
  }
  const start = safe.top + EDGE_MARGIN
  const end = area.height - keyboardInset - safe.bottom - EDGE_MARGIN - control.height
  return { start, length: Math.max(0, end - start) }
}

// top-left corner of the control in area coordinates
export function placeControl(mode: ControlMode, position: ControlPosition, frame: ControlFrame) {
  const { area, control, safe, keyboardInset } = frame
  const t = track(mode, frame)
  if (mode === 'bottom') {
    return {
      x: t.start + position.x * t.length,
      y: area.height - keyboardInset - safe.bottom - DOCK_GAP.band - control.height,
    }
  }
  const side = mode === 'overlay' ? position.side : mode
  const gap = mode === 'overlay' ? DOCK_GAP.overlay : DOCK_GAP.band
  return {
    x: side === 'left' ? safe.left + gap : area.width - safe.right - gap - control.width,
    y: t.start + position.y * t.length,
  }
}

function fraction(offset: number, t: Track) {
  return t.length > 0 ? Math.min(1, Math.max(0, (offset - t.start) / t.length)) : 0.5
}

// Where a control dropped with its top-left corner at (x, y) ends up. Overlay
// and side strips snap to the nearest side; the bottom band only moves along
// its length.
export function dropControl(
  mode: ControlMode,
  position: ControlPosition,
  x: number,
  y: number,
  frame: ControlFrame,
): ControlPosition {
  const t = track(mode, frame)
  if (mode === 'bottom') return { ...position, x: fraction(x, t) }
  const center = x + frame.control.width / 2
  return { ...position, side: center < frame.area.width / 2 ? 'left' : 'right', y: fraction(y, t) }
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
