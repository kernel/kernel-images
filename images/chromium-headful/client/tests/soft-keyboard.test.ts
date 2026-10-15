import { describe, expect, test } from 'bun:test'
import { SoftKeyboard } from '../src/utils/soft-keyboard'

describe('soft keyboard', () => {
  test('is up while it covers the visual viewport', () => {
    const k = new SoftKeyboard(true)
    expect(k.measure(390, 844, 844)).toBe(null)
    expect(k.measure(390, 500, 844)).toBe('up')
    expect(k.up).toBe(true)
    // switching to the emoji keyboard
    expect(k.measure(390, 460, 844)).toBe(null)
    expect(k.measure(390, 844, 844)).toBe('down')
  })

  test("is up while it shrinks the page, the way Android's does", () => {
    const k = new SoftKeyboard(true)
    k.measure(412, 900, 900)
    expect(k.measure(412, 560, 560)).toBe('up')
    expect(k.measure(412, 900, 900)).toBe('down')
  })

  test("iOS's shortcut bar for a hardware keyboard is not a keyboard", () => {
    const k = new SoftKeyboard(true)
    k.measure(820, 1180, 1180)
    expect(k.measure(820, 1125, 1180)).toBe(null)
    expect(k.up).toBe(false)
  })

  test('a rotation is not the keyboard coming or going', () => {
    const k = new SoftKeyboard(true)
    k.measure(390, 844, 844)
    k.measure(390, 500, 844)
    expect(k.measure(844, 200, 390)).toBe(null)
    expect(k.up).toBe(true)
    expect(k.measure(844, 390, 390)).toBe('down')
  })

  test('in a frame it is not known until a keyboard shows', () => {
    const k = new SoftKeyboard(false)
    k.measure(390, 600, 600)
    expect(k.measurable).toBe(false)
    k.measure(390, 300, 300)
    expect(k.measurable).toBe(true)
  })
})
