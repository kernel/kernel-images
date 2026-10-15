import { describe, expect, test } from 'bun:test'
import { SoftKeyboard } from '../src/utils/soft-keyboard'

const view = (width: number, height: number, scale = 1) => ({ width, height, scale })

describe('soft keyboard', () => {
  test('is up while it covers the visual viewport', () => {
    const k = new SoftKeyboard(true)
    expect(k.measure(view(390, 844))).toBe(null)
    expect(k.measure(view(390, 500))).toBe('up')
    expect(k.up).toBe(true)
    // switching to the emoji keyboard
    expect(k.measure(view(390, 460))).toBe(null)
    expect(k.measure(view(390, 844))).toBe('down')
  })

  test('the first keyboard after load is seen through a zoom that comes with it', () => {
    const k = new SoftKeyboard(true)
    k.measure(view(390, 664))
    // iOS zooms in as the first field takes focus, so the viewport shrinks in
    // CSS pixels on both sides as the keyboard comes up
    expect(k.measure(view(325, 553, 1.2))).toBe(null)
    expect(k.measure(view(325.4, 300, 1.2))).toBe('up')
    expect(k.measure(view(325, 553, 1.2))).toBe('down')
    // later keyboards at that zoom
    expect(k.measure(view(325, 300, 1.2))).toBe('up')
  })

  test("is up while it shrinks the page, the way Android's does", () => {
    const k = new SoftKeyboard(true)
    k.measure(view(412, 900))
    expect(k.measure(view(412, 560))).toBe('up')
    expect(k.measure(view(412, 900))).toBe('down')
  })

  test("iOS's shortcut bar for a hardware keyboard is not a keyboard", () => {
    const k = new SoftKeyboard(true)
    k.measure(view(820, 1180))
    expect(k.measure(view(820, 1125))).toBe(null)
    expect(k.up).toBe(false)
  })

  test('a rotation is neither the keyboard coming nor going', () => {
    const k = new SoftKeyboard(true)
    k.measure(view(390, 844))
    k.measure(view(390, 500))
    expect(k.measure(view(844, 200))).toBe(null)
    expect(k.measure(view(844, 390))).toBe(null)
    expect(k.measure(view(844, 200))).toBe('up')
  })

  test('in a frame it is not known until a keyboard shows', () => {
    const k = new SoftKeyboard(false)
    k.measure(view(390, 600))
    expect(k.measurable).toBe(false)
    k.measure(view(390, 300))
    expect(k.measurable).toBe(true)
  })
})
