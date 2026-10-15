import { describe, expect, test } from 'bun:test'
import { SoftKeyboard, isPhone } from '../src/utils/soft-keyboard'

const view = (height: number, scale = 1) => ({ height, scale })

describe('soft keyboard', () => {
  test('is up while it covers the layout viewport', () => {
    const k = new SoftKeyboard(true)
    expect(k.measure(view(744), 744)).toBe(null)
    expect(k.measure(view(463), 744)).toBe('up')
    expect(k.up).toBe(true)
    // switching to the emoji keyboard
    expect(k.measure(view(420), 744)).toBe(null)
    expect(k.measure(view(744), 744)).toBe('down')
  })

  test("Safari's toolbars coming and going are not a keyboard", () => {
    const k = new SoftKeyboard(true)
    // a page loaded full screen, then the toolbars appear
    expect(k.measure(view(932), 932)).toBe(null)
    expect(k.measure(view(744), 744)).toBe(null)
    expect(k.up).toBe(false)
    expect(k.measure(view(463), 744)).toBe('up')
  })

  test("iOS's shortcut bar for a hardware keyboard is not a keyboard", () => {
    const k = new SoftKeyboard(false)
    k.measure(view(1180), 1180)
    expect(k.measure(view(1125), 1180)).toBe(null)
    expect(k.up).toBe(false)
    expect(k.measurable).toBe(false)
  })

  test('a rotation keeps the keyboard as it was, or sees it go', () => {
    const k = new SoftKeyboard(true)
    k.measure(view(844), 844)
    k.measure(view(500), 844)
    expect(k.measure(view(200), 390)).toBe(null)
    expect(k.up).toBe(true)
    expect(k.measure(view(390), 390)).toBe('down')
    k.measure(view(200), 390)
    // rotated back with the keyboard going in the same change
    expect(k.measure(view(844), 844)).toBe('down')
  })

  test('a zoomed viewport says nothing about the keyboard', () => {
    const k = new SoftKeyboard(true)
    expect(k.measure(view(620, 1.2), 744)).toBe(null)
    expect(k.up).toBe(false)
  })

  test('is known on a phone, and elsewhere once a keyboard shows', () => {
    expect(new SoftKeyboard(true).measurable).toBe(true)
    const k = new SoftKeyboard(false)
    k.measure(view(300), 600)
    expect(k.measurable).toBe(true)
    expect(isPhone('Mozilla/5.0 (iPhone; CPU iPhone OS 26_6_2 like Mac OS X)')).toBe(true)
    expect(isPhone('Mozilla/5.0 (Linux; Android 15; Pixel 9) Mobile Safari/537.36')).toBe(true)
    expect(isPhone('Mozilla/5.0 (Linux; Android 15; SM-X910) Safari/537.36')).toBe(false)
    expect(isPhone('Mozilla/5.0 (iPad; CPU OS 18_0 like Mac OS X)')).toBe(false)
  })
})
