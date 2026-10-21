import { describe, expect, test } from 'bun:test'
import { BLUR_GRACE_MS, FOCUS_WAIT_MS, FocusKeyboard } from '../src/utils/focus-keyboard'
import { TapKeyboardMode } from '../src/utils/tap-keyboard'
import { RemoteField } from '../src/utils/remote-focus'

const search: RemoteField = { type: 'search', rect: { x: 10, y: 100, w: 300, h: 40 } }
const email: RemoteField = { type: 'email' }
const password: RemoteField = { type: 'password' }
const chat: RemoteField = { type: 'textarea' }
const digit = (n: number): RemoteField => ({
  type: 'text',
  inputMode: 'numeric',
  code: true,
  rect: { x: n * 40, y: 0, w: 30, h: 30 },
})

function setup(mode: TapKeyboardMode = 'auto', ios = true) {
  let clock = 0
  let timers: { fn: () => void; at: number; id: number }[] = []
  let nextTimer = 1
  const events: string[] = []
  // the overlay: unfocused, focused with inputmode="none", or focused with the keyboard up
  let overlay: 'blurred' | 'held' | 'open' = 'blurred'

  const keyboard: FocusKeyboard = new FocusKeyboard(
    mode,
    ios,
    {
      keyboardOpen: () => overlay === 'open',
      focus: (field, inGesture) => {
        events.push(
          `${overlay === 'held' ? 'raise-held' : 'focus'} ${field ? field.type : 'default'}${
            inGesture ? ' in tap' : ''
          }`,
        )
        overlay = 'open'
      },
      update: (field) => events.push(`update ${field.type}`),
      hold: () => {
        events.push('hold')
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

  // a tap on the page; field is what the last rects sent put under it
  const tap = (field: RemoteField | null) => {
    keyboard.touchBegin()
    advance(80)
    keyboard.tap(field)
  }
  // a focus report from the remote
  const report = (field: RemoteField | null, pageFocused = true) => keyboard.focus(field, pageFocused)

  // the keyboard button
  const button = () => {
    overlay = 'open'
    keyboard.opened()
  }

  return { keyboard, tap, report, button, advance, events, overlay: () => overlay }
}

describe('focus keyboard on iOS', () => {
  test('a tap on a known field raises its keyboard inside the tap, and the focus report confirms it', () => {
    const t = setup()
    t.tap(search)
    expect(t.events).toEqual(['focus search in tap'])
    t.advance(120)
    t.report(search)
    t.advance(FOCUS_WAIT_MS)
    expect(t.overlay()).toBe('open')
  })

  test('a field that does not take focus closes the keyboard at the end of the wait', () => {
    const t = setup()
    // a stale rect, or a field covered by a dialog
    t.tap(search)
    t.advance(FOCUS_WAIT_MS)
    expect(t.events).toEqual(['focus search in tap', 'release'])
  })

  test('a focus report after the finger lifts raises the keyboard from the focus held since the tap', () => {
    const t = setup()
    // a label, or a button that focuses a field
    t.tap(null)
    expect(t.events).toEqual(['hold'])
    expect(t.keyboard.holding).toBe(true)
    t.advance(300)
    t.report(email)
    expect(t.events).toEqual(['hold', 'raise-held email'])
    expect(t.keyboard.holding).toBe(false)
  })

  test('a tap that focuses nothing releases the held focus without a keyboard', () => {
    const t = setup()
    t.tap(null)
    t.advance(FOCUS_WAIT_MS)
    expect(t.events).toEqual(['hold', 'release'])
    expect(t.overlay()).toBe('blurred')
  })

  test('tapping the field that already has focus keeps the keyboard, with no new report', () => {
    const t = setup()
    t.report(search)
    t.tap(search)
    t.advance(FOCUS_WAIT_MS)
    expect(t.events).toEqual(['focus search in tap'])
    expect(t.overlay()).toBe('open')
  })

  test('a scroll or pinch ends the wait', () => {
    const t = setup()
    t.tap(null)
    t.keyboard.touchBegin()
    t.report(email)
    expect(t.events).toEqual(['hold', 'release'])
  })

  test('a field focused by the page without a tap leaves the keyboard closed', () => {
    const t = setup()
    t.report(search)
    t.advance(FOCUS_WAIT_MS)
    expect(t.events).toEqual([])
  })
})

describe('focus keyboard closing', () => {
  const opened = (field: RemoteField) => {
    const t = setup()
    t.tap(field)
    t.report(field)
    t.advance(FOCUS_WAIT_MS)
    t.events.length = 0
    return t
  }

  test('a submit that navigates closes the keyboard, even with a search box in the same place on the next page', () => {
    const t = opened(search)
    // Enter: the old document goes, and the results page's search box is not focused
    t.report(null)
    t.advance(BLUR_GRACE_MS)
    expect(t.events).toEqual(['release'])
  })

  test('a chat box that keeps focus after Enter keeps the keyboard', () => {
    const t = opened(chat)
    t.report(chat)
    t.advance(5000)
    expect(t.events).toEqual([])
    expect(t.overlay()).toBe('open')
  })

  test('a field re-rendered right after a blur keeps the keyboard', () => {
    const t = opened(chat)
    t.report(null)
    t.advance(BLUR_GRACE_MS - 50)
    t.report(chat)
    t.advance(BLUR_GRACE_MS)
    expect(t.events).toEqual([])
  })

  test('focus moved by the page switches the open keyboard', () => {
    const t = opened(digit(0))
    t.report(digit(1))
    t.report(email)
    t.report(email)
    expect(t.events).toEqual(['update email'])
  })

  test('a tap on the page that focuses nothing closes the keyboard', () => {
    const t = opened(email)
    t.tap(null)
    t.report(null)
    t.advance(FOCUS_WAIT_MS)
    expect(t.events).toEqual(['release'])
  })

  test('the keyboard button with nothing focused is not closed by focus reports', () => {
    const t = setup()
    t.button()
    t.report(null)
    t.advance(5000)
    expect(t.overlay()).toBe('open')
  })

  test('the keyboard button on a focused field closes with its blur', () => {
    const t = setup()
    t.report(search)
    t.button()
    t.report(null)
    t.advance(BLUR_GRACE_MS)
    expect(t.events).toEqual(['release'])
  })
})

describe('focus keyboard and the address bar', () => {
  // the cursor-shape guess raised the keyboard for a tap on the address bar
  const addressBar = (t: ReturnType<typeof setup>) => {
    t.keyboard.cursorTap(true)
    t.keyboard.blur()
    t.events.push('cursor opens keyboard')
  }

  test('an address bar submit closes the keyboard once the page gets focus back', () => {
    const t = setup()
    t.report(null, true)
    addressBar(t)
    // the tap focused the address bar
    t.report(null, false)
    // Enter navigates, and the new page gets focus
    t.report(null, true)
    expect(t.events).toEqual(['cursor opens keyboard'])
  })

  test('the page reporting focus before the address bar takes it does not close it', () => {
    const t = setup()
    t.report(search, true)
    addressBar(t)
    t.keyboard.focus(null, true)
    expect(t.events).toEqual(['cursor opens keyboard'])
  })

  test('a page field losing focus to the address bar does not close it', () => {
    const t = setup()
    t.tap(search)
    t.report(search)
    t.advance(FOCUS_WAIT_MS)
    t.keyboard.cursorTap(true)
    t.report(null, false)
    t.advance(BLUR_GRACE_MS)
    expect(t.events).toEqual(['focus search in tap'])
    expect(t.overlay()).toBe('open')
    // the address bar navigates and the page has focus again
    t.report(null, true)
    expect(t.events).toEqual(['focus search in tap', 'release'])
  })

  test('a tap back on a page field takes over from the address bar', () => {
    const t = setup()
    t.tap(search)
    t.report(search)
    t.advance(FOCUS_WAIT_MS)
    t.keyboard.cursorTap(true)
    t.report(null, false)
    t.tap(search)
    t.report(search, true)
    t.advance(FOCUS_WAIT_MS)
    expect(t.overlay()).toBe('open')
    expect(t.events).toEqual(['focus search in tap', 'focus search in tap'])
  })
})

describe('focus keyboard without the stream', () => {
  test('a keyboard raised while the stream was down is closed by a field blur once it is back', () => {
    const t = setup()
    t.keyboard.cursorTap(false)
    t.keyboard.blur()
    t.events.push('cursor opens keyboard')
    // the overlay is focused by the cursor-shape path
    t.button()
    t.report(null)
    t.advance(BLUR_GRACE_MS)
    expect(t.overlay()).toBe('open')
    t.report(search)
    t.report(null)
    t.advance(BLUR_GRACE_MS)
    expect(t.events).toEqual(['cursor opens keyboard', 'update search', 'release'])
  })
})

describe('focus keyboard elsewhere', () => {
  test('other browsers raise the keyboard when the focus report arrives', () => {
    const t = setup('auto', false)
    t.tap(null)
    expect(t.events).toEqual([])
    t.report(password)
    expect(t.events).toEqual(['focus password'])
  })

  test('off leaves the keyboard to the button, always raises it on every tap', () => {
    const off = setup('off')
    off.tap(search)
    off.report(search)
    expect(off.events).toEqual([])

    const always = setup('always')
    always.tap(null)
    expect(always.events).toEqual(['focus default in tap'])
  })
})
