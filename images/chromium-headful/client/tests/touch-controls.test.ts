import { describe, expect, test } from 'bun:test'
import {
  BAND_SIZE,
  DEFAULT_POSITION,
  controlLayout,
  decodePosition,
  dropControl,
  encodePosition,
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

describe('dragging the control', () => {
  test('snaps to the nearest side edge and keeps the height', () => {
    const dropped = dropControl('overlay', DEFAULT_POSITION, 350, 400, 390, 844)
    expect(dropped.side).toBe('right')
    expect(dropped.y).toBeGreaterThan(0.4)
    expect(dropped.y).toBeLessThan(0.5)
    expect(dropControl('left', dropped, 20, 30, 390, 844)).toEqual({ ...dropped, side: 'left', y: 0 })
  })

  test('moves only along the bottom band', () => {
    const dropped = dropControl('bottom', DEFAULT_POSITION, 390, 10, 390, 844)
    expect(dropped).toEqual({ ...DEFAULT_POSITION, x: 1 })
  })

  test('round-trips the stored position and falls back to the default', () => {
    const position = { side: 'right' as const, x: 0.25, y: 0.75 }
    expect(decodePosition(encodePosition(position))).toEqual(position)
    expect(decodePosition('')).toEqual(DEFAULT_POSITION)
    expect(decodePosition('up:abc:2000')).toEqual({ side: 'left', x: DEFAULT_POSITION.x, y: 1 })
  })
})
