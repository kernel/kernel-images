import { describe, expect, test } from 'bun:test'
import { TapKeyboard, TapKeyboardMode } from '../src/utils/tap-keyboard'
import { focusNeedsGesture } from '../src/utils/text-input'

// A 1920x1080 stream shown 390px wide, as on a phone in portrait: about 4.9
// remote px per CSS px. Taps are given in CSS px.
const SCALE = 1920 / 390

function setup(mode: TapKeyboardMode, ios = true) {
  let clock = 0
  let timers: { fn: () => void; at: number; id: number }[] = []
  let nextTimer = 1
  const events: string[] = []
  // the overlay: unfocused, focused with inputmode="none", or focused with the keyboard up
  let overlay: 'blurred' | 'held' | 'open' = 'blurred'
  let holdTakes = true

  const keyboard: TapKeyboard = new TapKeyboard(
    mode,
    ios,
    {
      keyboardOpen: () => overlay === 'open',
      focus: () => {
        events.push(overlay === 'held' ? 'raise-held' : 'focus')
        overlay = 'open'
      },
      hold: () => {
        events.push('hold')
        if (!holdTakes) return false
        expect(keyboard.holding).toBe(true)
        overlay = 'held'
        return true
      },
      release: () => {
        if (overlay === 'blurred') return
        events.push('release')
        overlay = 'blurred'
        keyboard.blur()
      },
    },
    {
      now: () => clock,
      setTimeout: (fn, ms) => {
        timers.push({ fn, at: clock + ms, id: nextTimer })
        return nextTimer++
      },
      clearTimeout: (id) => {
        timers = timers.filter((t) => t.id !== id)
      },
    },
  )

  const advance = (ms: number) => {
    const until = clock + ms
    for (;;) {
      const due = timers.filter((t) => t.at <= until).sort((a, b) => a.at - b.at)[0]
      if (!due) break
      timers = timers.filter((t) => t !== due)
      clock = due.at
      due.fn()
    }
    clock = until
  }

  // the finger lifts 80ms after touching down
  const tap = (p: { x: number; y: number }) => {
    keyboard.touchBegin()
    advance(80)
    keyboard.tap({ x: Math.round(p.x * SCALE), y: Math.round(p.y * SCALE) }, SCALE)
  }
  // the user or the OS dismisses the keyboard
  const dismiss = () => {
    overlay = 'blurred'
    keyboard.blur()
  }

  return {
    keyboard,
    tap,
    advance,
    dismiss,
    events,
    overlay: () => overlay,
    failHold: () => (holdTakes = false),
  }
}

const field = { x: 100, y: 50 }
const blank = { x: 200, y: 180 }

describe('tap keyboard on iOS', () => {
  test('a text cursor after the finger lifts raises the keyboard through the focus held since the tap', () => {
    const t = setup('auto')
    t.tap(field)
    expect(t.events).toEqual(['hold'])
    expect(t.keyboard.holding).toBe(true)

    t.advance(150)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['hold', 'raise-held'])
    expect(t.overlay()).toBe('open')
    expect(t.keyboard.holding).toBe(false)
  })

  test('a text cursor during the touch raises the keyboard inside the tap', () => {
    const t = setup('auto')
    t.keyboard.touchBegin()
    t.advance(40)
    t.keyboard.cursor('text')
    t.advance(40)
    t.keyboard.tap(field, SCALE)
    expect(t.events).toEqual(['focus'])
  })

  test('a text cursor flashed after another cursor still counts', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(100)
    t.keyboard.cursor('other')
    t.advance(300)
    t.keyboard.cursor('text')
    expect(t.overlay()).toBe('open')
  })

  test('a non-text tap releases the held focus without a keyboard', () => {
    const t = setup('auto')
    t.tap(blank)
    t.advance(100)
    t.keyboard.cursor('other')
    t.advance(800)
    expect(t.events).toEqual(['hold', 'release'])
    expect(t.overlay()).toBe('blurred')
  })

  test('a text cursor after the window does nothing, leaving it to the keyboard button', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(900)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['hold', 'release'])
  })

  test('a text cursor long after the window does nothing', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(2500)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['hold', 'release'])
  })

  test('tapping a field under an unchanged text cursor raises the keyboard at the end of the window', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(150)
    t.keyboard.cursor('text')
    t.dismiss()
    t.events.length = 0

    // 30 CSS px from the remembered spot, inside the same wide field
    t.advance(2000)
    t.tap({ x: field.x + 30, y: field.y })
    expect(t.events).toEqual(['hold'])
    t.advance(800)
    expect(t.events).toEqual(['hold', 'raise-held'])
  })

  test('a tap near where a text cursor was seen raises the keyboard inside the tap', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(150)
    t.keyboard.cursor('text')
    t.dismiss()
    t.events.length = 0

    t.advance(2000)
    t.tap({ x: field.x + 8, y: field.y })
    expect(t.events).toEqual(['focus'])
  })

  test('a tap near a remembered spot that turns out not to be text closes the keyboard and forgets the spot', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(150)
    t.keyboard.cursor('text')
    t.dismiss()

    t.advance(2000)
    t.tap({ x: field.x, y: field.y + 8 })
    expect(t.overlay()).toBe('open')
    t.advance(100)
    t.keyboard.cursor('other')
    t.advance(800)
    expect(t.overlay()).toBe('blurred')

    t.events.length = 0
    t.advance(2000)
    t.tap({ x: field.x, y: field.y + 8 })
    expect(t.events).toEqual(['hold'])
  })

  test('a non-text tap closes an open keyboard', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(150)
    t.keyboard.cursor('text')
    t.events.length = 0

    t.advance(1000)
    t.tap(blank)
    t.advance(100)
    t.keyboard.cursor('other')
    t.advance(800)
    expect(t.events).toEqual(['release'])
  })

  test('a new touch before the cursor arrives drops the tap', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(50)
    t.keyboard.touchBegin()
    expect(t.events).toEqual(['hold', 'release'])
    t.advance(100)
    t.keyboard.cursor('text')
    t.advance(2000)
    expect(t.events).toEqual(['hold', 'release'])
  })

  test("the previous tap's I-beam arriving during the next tap does not raise the keyboard", () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(20)
    // the next tap lands on a blank spot before the field's I-beam arrives
    t.tap(blank)
    t.advance(30)
    t.keyboard.cursor('text')
    expect(t.overlay()).not.toBe('open')
    t.advance(50)
    t.keyboard.cursor('other')
    t.advance(800)
    expect(t.events).toEqual(['hold', 'release', 'hold', 'release'])
  })

  test('an I-beam that may be stale and is never replaced raises nothing', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(20)
    t.tap({ x: field.x, y: field.y + 40 })
    t.advance(30)
    t.keyboard.cursor('text')
    t.advance(800)
    expect(t.events).toEqual(['hold', 'release', 'hold', 'release'])
  })

  test('a field tap after a non-text tap raises the keyboard', () => {
    const t = setup('auto')
    t.tap(blank)
    t.advance(100)
    t.keyboard.cursor('other')
    t.advance(800)
    t.events.length = 0
    t.advance(500)
    t.tap(field)
    t.advance(150)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['hold', 'raise-held'])
  })

  test('a possibly stale I-beam does not affect a later tap under it', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(20)
    t.tap({ x: field.x, y: field.y + 40 })
    t.advance(30)
    t.keyboard.cursor('text')
    t.advance(800)
    t.events.length = 0
    // the I-beam is still showing and the next tap lands on the same field
    t.advance(2000)
    t.tap({ x: field.x + 60, y: field.y + 40 })
    t.advance(800)
    expect(t.events).toEqual(['hold', 'raise-held'])
  })

  test('a tap after the previous cursor settled is not affected', () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(150)
    t.keyboard.cursor('text')
    t.dismiss()
    t.events.length = 0
    t.advance(2000)
    t.tap({ x: field.x + 100, y: field.y + 60 })
    t.advance(100)
    t.keyboard.cursor('other')
    t.advance(100)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['hold', 'raise-held'])
  })

  test('a held focus lost to the OS raises nothing', () => {
    const t = setup('auto')
    t.tap(field)
    t.dismiss()
    t.advance(150)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['hold'])
  })

  test('raises nothing when the focus could not be held', () => {
    const t = setup('auto')
    t.failHold()
    t.tap(field)
    t.advance(150)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['hold'])
  })

  test('cancel releases a held focus and ends the wait', () => {
    const t = setup('auto')
    t.tap(field)
    t.keyboard.cancel()
    t.advance(150)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['hold', 'release'])
  })
})

describe('keyboard after Enter', () => {
  // the keyboard is open on a field under an I-beam
  const typing = () => {
    const t = setup('auto')
    t.tap(field)
    t.advance(150)
    t.keyboard.cursor('text')
    t.events.length = 0
    return t
  }

  test('closes when the I-beam goes away after Enter', () => {
    const t = typing()
    t.keyboard.submit()
    t.advance(300)
    t.keyboard.cursor('other')
    t.advance(700)
    expect(t.events).toEqual(['release'])
  })

  test('stays open while the I-beam stays, as in a chat box', () => {
    const t = typing()
    t.keyboard.submit()
    t.advance(1500)
    expect(t.events).toEqual([])
    expect(t.overlay()).toBe('open')
  })

  test('stays open when another cursor flashes and the I-beam comes back', () => {
    const t = typing()
    t.keyboard.submit()
    t.advance(200)
    t.keyboard.cursor('other')
    t.advance(100)
    t.keyboard.cursor('text')
    t.advance(1000)
    expect(t.events).toEqual([])
  })

  test('ignores a cursor change after the window and a touch during it', () => {
    const late = typing()
    late.keyboard.submit()
    late.advance(1100)
    late.keyboard.cursor('other')
    late.advance(1000)
    expect(late.events).toEqual([])

    const touched = typing()
    touched.keyboard.submit()
    touched.advance(100)
    touched.keyboard.touchBegin()
    touched.keyboard.cursor('other')
    touched.advance(1000)
    expect(touched.events).toEqual([])
  })

  test('does nothing without an open keyboard on an I-beam', () => {
    const t = setup('auto')
    t.keyboard.cursor('other')
    t.keyboard.submit()
    t.advance(1000)
    expect(t.events).toEqual([])
  })
})

describe('tap keyboard elsewhere', () => {
  test('focuses when the cursor arrives without holding focus during the tap', () => {
    const t = setup('auto', false)
    t.tap(field)
    expect(t.events).toEqual([])
    t.advance(150)
    t.keyboard.cursor('text')
    expect(t.events).toEqual(['focus'])
  })

  test('off mode does nothing and always mode focuses inside the tap', () => {
    const off = setup('off')
    off.tap(field)
    off.advance(150)
    off.keyboard.cursor('text')
    expect(off.events).toEqual([])

    const always = setup('always')
    always.tap(blank)
    expect(always.events).toEqual(['focus'])
  })
})

describe('focusNeedsGesture', () => {
  test('detects iOS and iPadOS', () => {
    const iphone = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15'
    expect(focusNeedsGesture({ userAgent: iphone, platform: 'iPhone', maxTouchPoints: 5 })).toBe(true)
    expect(focusNeedsGesture({ userAgent: 'Macintosh', platform: 'MacIntel', maxTouchPoints: 5 })).toBe(true)
    expect(focusNeedsGesture({ userAgent: 'Macintosh', platform: 'MacIntel', maxTouchPoints: 0 })).toBe(false)
    expect(focusNeedsGesture({ userAgent: 'Linux; Android 14', platform: 'Linux armv8l', maxTouchPoints: 5 })).toBe(
      false,
    )
  })
})
