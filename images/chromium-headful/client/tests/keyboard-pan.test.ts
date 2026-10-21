import { describe, expect, test } from 'bun:test'
import { ANCHOR_GAP, keyboardPan, keyboardShrink } from '../src/utils/keyboard-pan'

// a 390x664 player at the top of the screen; the keyboard leaves the top 380px
const view = { top: 0, height: 664, anchor: null, current: 0, visibleTop: 0, visibleBottom: 380 }

describe('keyboard shrink', () => {
  test('a shorter frame while the keyboard is open is the keyboard', () => {
    expect(keyboardShrink(true, { width: 390, height: 664 }, { width: 390, height: 380 })).toBe(true)
  })

  test('a closed keyboard, a new width or a taller frame is a new layout', () => {
    expect(keyboardShrink(false, { width: 390, height: 664 }, { width: 390, height: 380 })).toBe(false)
    expect(keyboardShrink(true, { width: 390, height: 664 }, { width: 664, height: 300 })).toBe(false)
    expect(keyboardShrink(true, { width: 390, height: 380 }, { width: 390, height: 664 })).toBe(false)
  })
})

describe('keyboard pan', () => {
  test('moves a tap under the keyboard up to sit above it', () => {
    const pan = keyboardPan({ ...view, anchor: 500 })
    expect(pan).toBe(500 + ANCHOR_GAP - 380)
    expect(500 - pan + ANCHOR_GAP).toBe(380)
  })

  test('leaves a tap that is already above the keyboard where it is', () => {
    expect(keyboardPan({ ...view, anchor: 120 })).toBe(0)
  })

  test('keeps the current pan while the tap stays visible', () => {
    expect(keyboardPan({ ...view, anchor: 400, current: 200 })).toBe(200)
  })

  test('lowers the pan when a later tap would go off the top', () => {
    // the player is already moved up 250px; a tap 100px from its top is above the screen
    expect(keyboardPan({ ...view, anchor: 100, current: 250 })).toBe(100 - 16)
  })

  test('never moves the video further than the part the keyboard covers', () => {
    expect(keyboardPan({ ...view, anchor: 650 })).toBe(664 - 380)
  })

  test('keeps the pan within the covered part without a tap, and drops it once nothing is covered', () => {
    expect(keyboardPan({ ...view, current: 100 })).toBe(100)
    expect(keyboardPan({ ...view, current: 100, visibleBottom: 664 })).toBe(0)
  })
})
