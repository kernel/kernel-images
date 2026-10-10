import { describe, expect, test } from 'bun:test'
import {
  BAND_SIZE,
  ControlFrame,
  DEFAULT_POSITION,
  controlLayout,
  decodePosition,
  dropControl,
  encodePosition,
  placeControl,
} from '../src/utils/touch-controls'

const noInsets = { bottom: 0, left: 0, right: 0 }

describe('control layout', () => {
  test('uses the bottom band when the video leaves vertical room', () => {
    // a 16:9 stream on a portrait phone
    expect(controlLayout(390, 664, 390, 219, noInsets, 'left')).toEqual({ mode: 'bottom', band: BAND_SIZE })
  })

  test('uses a side strip on the preferred side when only horizontal room is left', () => {
    // a portrait stream pillarboxed on a shorter screen
    expect(controlLayout(390, 664, 307, 664, noInsets, 'right')).toEqual({ mode: 'right', band: BAND_SIZE })
  })

  test('includes the safe-area inset in the band', () => {
    const insets = { bottom: 34, left: 0, right: 0 }
    expect(controlLayout(390, 664, 390, 219, insets, 'left')).toEqual({ mode: 'bottom', band: BAND_SIZE + 34 })
    // 52 + 34 does not fit in a 60px band, so the strip or shrinking decides instead
    expect(controlLayout(390, 844, 390, 784, insets, 'left').mode).not.toBe('overlay')
  })

  test('shrinks the video slightly rather than overlaying it', () => {
    // a stream that exactly fills its frame: shrinking by 52px is about 6%
    expect(controlLayout(390, 844, 390, 844, noInsets, 'left')).toEqual({ mode: 'bottom', band: BAND_SIZE })
  })

  test('overlays the stream when making room would shrink it too much', () => {
    expect(controlLayout(300, 200, 300, 200, noInsets, 'left')).toEqual({ mode: 'overlay', band: 0 })
  })
})

const frame = (overrides: Partial<ControlFrame> = {}): ControlFrame => ({
  area: { width: 390, height: 844 },
  control: { width: 44, height: 44 },
  safe: { top: 0, bottom: 0, left: 0, right: 0 },
  keyboardInset: 0,
  ...overrides,
})

describe('placing and dragging the control', () => {
  test('a dropped control snaps to the nearest side edge and keeps its height', () => {
    const dropped = dropControl('overlay', DEFAULT_POSITION, 330, 380, frame())
    expect(dropped.side).toBe('right')
    expect(placeControl('overlay', dropped, frame())).toEqual({ x: 390 - 6 - 44, y: 380 })
    expect(dropControl('left', dropped, 20, -50, frame())).toEqual({ ...dropped, side: 'left', y: 0 })
  })

  test('drops a wide control where it was released, not where a 44px one would be', () => {
    const wide = frame({ control: { width: 120, height: 44 } })
    const dropped = dropControl('bottom', DEFAULT_POSITION, 150, 790, wide)
    expect(placeControl('bottom', dropped, wide).x).toBeCloseTo(150)
    // the bottom band only moves sideways
    expect(placeControl('bottom', dropped, wide).y).toBe(844 - 4 - 44)
  })

  test('keeps the control out of the safe areas and above the soft keyboard', () => {
    const notched = frame({ safe: { top: 47, bottom: 34, left: 0, right: 0 }, keyboardInset: 300 })
    const top = placeControl('overlay', { ...DEFAULT_POSITION, y: 0 }, notched)
    const bottom = placeControl('overlay', { ...DEFAULT_POSITION, y: 1 }, notched)
    expect(top.y).toBe(47 + 8)
    expect(bottom.y + 44).toBe(844 - 300 - 34 - 8)
    expect(placeControl('bottom', DEFAULT_POSITION, notched).y).toBe(844 - 300 - 34 - 4 - 44)
  })

  test('round-trips the stored position and falls back to the default', () => {
    const position = { side: 'right' as const, x: 0.25, y: 0.75 }
    expect(decodePosition(encodePosition(position))).toEqual(position)
    expect(decodePosition('')).toEqual(DEFAULT_POSITION)
    expect(decodePosition('up:abc:2000')).toEqual({ side: 'left', x: DEFAULT_POSITION.x, y: 1 })
  })
})
