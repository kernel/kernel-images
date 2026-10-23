import { describe, expect, test } from 'bun:test'
import {
  charToKeysym,
  keysymNeedsShift,
  needsShift,
  textDiff,
  TextInput,
  XK_BACKSPACE,
  XK_CAPS_LOCK,
  XK_CONTROL_L,
  XK_RETURN,
  XK_SHIFT_L,
  XK_SHIFT_R,
} from '../src/utils/text-input'
import createKeyboard from '../src/utils/guacamole-keyboard'
import { ZoomPan } from '../src/utils/zoom-pan'
import { GestureHandlers, Point, TouchGestures } from '../src/utils/touch-gestures'

describe('text input', () => {
  test('maps characters to keysyms', () => {
    expect(charToKeysym('a')).toBe(0x61)
    expect(charToKeysym('é')).toBe(0xe9)
    expect(charToKeysym('€')).toBe(0x010020ac)
    expect(charToKeysym('\n')).toBe(XK_RETURN)
  })

  test('detects letters that need shift', () => {
    expect(needsShift('A')).toBe(true)
    expect(needsShift('a')).toBe(false)
    expect(needsShift('!')).toBe(false)
    expect(keysymNeedsShift(0x41)).toBe(true)
    expect(keysymNeedsShift(0x61)).toBe(false)
    expect(keysymNeedsShift(0xc9)).toBe(true)
    expect(keysymNeedsShift(0x01000416)).toBe(true)
  })

  test('diffs composition updates', () => {
    expect(textDiff('', 'te')).toEqual({ deletes: 0, insert: 'te' })
    expect(textDiff('tez', 'tes')).toEqual({ deletes: 1, insert: 's' })
    expect(textDiff('hello', 'help')).toEqual({ deletes: 2, insert: 'p' })
  })
})

// The remote X server as far as typing goes: Shift held, Caps Lock toggled by
// its key, and a letter's case from Shift xor Caps Lock (neko maps a keysym to
// its key, so the keysym's own case does not matter).
function remoteX() {
  const keys: number[] = []
  const shift = new Set<number>()
  let capsLock = false
  let text = ''
  const sink = (event: 'keydown' | 'keyup', key: number) => {
    keys.push(event === 'keydown' ? key : -key)
    if (key === XK_SHIFT_L || key === XK_SHIFT_R) {
      if (event === 'keydown') shift.add(key)
      else shift.delete(key)
      return
    }
    if (event === 'keyup') return
    if (key === XK_CAPS_LOCK) capsLock = !capsLock
    else if (key === XK_BACKSPACE) text = text.slice(0, -1)
    else if (key === XK_RETURN) text += '\n'
    else if (key < 0x100 || (key & 0xff000000) === 0x01000000) {
      const ch = String.fromCodePoint(key & 0x00ffffff)
      const cased = ch.toUpperCase() !== ch.toLowerCase()
      text += !cased ? ch : shift.size > 0 !== capsLock ? ch.toUpperCase() : ch.toLowerCase()
    }
  }
  return {
    sink,
    keys,
    get text() {
      return text
    },
    get capsLock() {
      return capsLock
    },
  }
}

describe('soft keyboard text', () => {
  const setup = (touch = true) => {
    const remote = remoteX()
    const input = new TextInput(remote.sink, () => touch)
    return { input, remote }
  }

  test('text from a key event is not typed again by its input event', () => {
    const { input, remote } = setup()
    // the Guacamole keyboard on iOS releases a key right after pressing it
    input.keyDown(0x61)
    input.keyUp(0x61)
    input.input('insertText', 'a')
    input.keyEvent(true)
    // a keyboard that reports keyCode 229 sends only the input event
    input.input('insertText', 'b')
    expect(remote.text).toBe('ab')
  })

  test('a key without text does not swallow the next input event', () => {
    const { input, remote } = setup()
    input.keyDown(0xff51)
    input.keyUp(0xff51)
    input.keyEvent(true)
    input.input('insertText', 'x')
    expect(remote.text).toBe('x')
  })

  test('a soft Shift does not swallow the next input event', () => {
    const { input, remote } = setup()
    input.keyDown(XK_SHIFT_L)
    input.keyUp(XK_SHIFT_L)
    input.input('insertReplacementText', 'Hi')
    expect(remote.text).toBe('Hi')
  })

  test('types line breaks and backspace from input events', () => {
    const { input, remote } = setup()
    input.input('insertText', 'ab')
    input.input('deleteContentBackward', null)
    input.input('insertLineBreak', null)
    expect(remote.text).toBe('a\n')
  })

  test('types composition updates as edits and ignores the echo after compositionend', () => {
    const { input, remote } = setup()
    input.compositionStart()
    input.input('insertCompositionText', 'te')
    input.input('insertCompositionText', 'tez')
    input.input('insertCompositionText', 'te')
    input.compositionEnd('tea', true)
    input.input('insertText', 'tea')
    expect(remote.text).toBe('tea')
    expect(remote.keys.filter((k) => k === XK_BACKSPACE)).toHaveLength(1)
    expect(input.composing).toBe(false)

    // only the event right after compositionend is an echo
    input.input('insertText', 'tea')
    expect(remote.text).toBe('teatea')
  })

  test('a key event ends the window for a composition echo', () => {
    const { input, remote } = setup()
    input.compositionStart()
    input.compositionEnd('a', true)
    input.keyEvent(false)
    input.input('insertText', 'a')
    expect(remote.text).toBe('aa')
  })

  test('a latched Ctrl holds for the next key only, from any path', () => {
    const { input, remote } = setup()
    const latches: boolean[] = []
    input.onCtrlLatch = (latched) => latches.push(latched)
    input.latchCtrl(true)
    input.input('insertText', 'c')
    input.input('insertText', 'd')
    expect(remote.keys).toEqual([XK_CONTROL_L, 0x63, -0x63, -XK_CONTROL_L, 0x64, -0x64])
    expect(latches).toEqual([true, false])

    remote.keys.length = 0
    input.latchCtrl(true)
    // a soft Shift from the Guacamole keyboard does not use up the latch
    input.keyDown(XK_SHIFT_L)
    input.keyUp(XK_SHIFT_L)
    input.key(0xff09)
    expect(remote.keys).toEqual([XK_SHIFT_L, -XK_SHIFT_L, XK_CONTROL_L, 0xff09, -0xff09, -XK_CONTROL_L])
  })

  test('an unlatched Ctrl sends nothing', () => {
    const { input, remote } = setup()
    input.latchCtrl(true)
    input.latchCtrl(false)
    input.key(0xff1b)
    expect(remote.keys).toEqual([0xff1b, -0xff1b])
  })

  test('types pasted text without its trailing line break', () => {
    const { input, remote } = setup()
    input.paste('Pass Word\n2\n')
    expect(remote.text).toBe('Pass Word\n2')
    expect(remote.keys.slice(0, 2)).toEqual([XK_SHIFT_L, 0x50])
  })

  test('drops composed text while input is disabled', () => {
    const { input, remote } = setup()
    input.compositionStart()
    input.compositionEnd('hi', false)
    expect(remote.keys).toEqual([])
  })

  test('sends Shift around capitals', () => {
    const { input, remote } = setup()
    input.keyDown(0x41)
    input.keyUp(0x41)
    input.keyEvent(true)
    input.input('insertText', 'Bc')
    expect(remote.text).toBe('ABc')
    expect(remote.keys.slice(0, 4)).toEqual([XK_SHIFT_L, 0x41, -0x41, -XK_SHIFT_L])
  })

  test('never sends Caps Lock from a touch device', () => {
    const { input, remote } = setup()
    input.keyDown(XK_CAPS_LOCK)
    input.keyUp(XK_CAPS_LOCK)
    input.input('insertText', 'Hi')
    expect(remote.text).toBe('Hi')
    expect(remote.keys.some((k) => Math.abs(k) === XK_CAPS_LOCK)).toBe(false)
  })

  test('a Shift held on the remote types capitals without another Shift', () => {
    const { input, remote } = setup()
    input.keyDown(XK_SHIFT_L)
    input.keyDown(0x41)
    input.keyUp(0x41)
    input.keyUp(XK_SHIFT_L)
    expect(remote.text).toBe('A')
    expect(remote.keys.filter((k) => k === XK_SHIFT_L)).toHaveLength(1)
  })

  test('sends desktop keys as they are, Caps Lock included', () => {
    const { input, remote } = setup(false)
    input.keyDown(XK_CAPS_LOCK)
    input.keyUp(XK_CAPS_LOCK)
    input.keyDown(0x41, 0x41)
    input.keyUp(0x41, 0x41)
    expect(remote.keys).toEqual([XK_CAPS_LOCK, -XK_CAPS_LOCK, 0x41, -0x41])
  })
})

// The events iOS Safari sends for its soft keyboard, replayed through the real
// Guacamole keyboard wired to TextInput as the live view wires them. WebKit
// reports the keyboard's Shift and Caps Lock state changes as synthetic key
// events with no text (WKSyntheticFlagsChangedWebEvent), and the Guacamole
// keyboard treats every keyup on iOS as unreliable. The modifier flags on the
// letter events themselves are not documented, so every combination is tried.
describe('iOS soft keyboard', () => {
  const ios = () => {
    const saved = Object.getOwnPropertyDescriptor(navigator, 'platform')!
    Object.defineProperty(navigator, 'platform', { value: 'iPhone', configurable: true })
    const remote = remoteX()
    const input = new TextInput(remote.sink, () => true)
    const listeners: Record<string, ((e: unknown) => void)[]> = {}
    const target = {
      addEventListener: (type: string, fn: (e: unknown) => void) => (listeners[type] ||= []).push(fn),
    }
    const keyboard = createKeyboard(target as unknown as Element)
    Object.defineProperty(navigator, 'platform', saved)
    keyboard.onkeydown = (key) => {
      input.keyDown(key)
      return false
    }
    keyboard.onkeyup = (key) => input.keyUp(key)

    // capture listeners (the Guacamole keyboard) run before the live view's own at the target
    const fire = (type: string, init: Record<string, unknown>, modifiers: { shift?: boolean; caps?: boolean } = {}) => {
      const e = {
        type,
        shiftKey: !!modifiers.shift,
        ctrlKey: false,
        altKey: false,
        metaKey: false,
        location: 0,
        repeat: false,
        preventDefault() {},
        getModifierState: (name: string) =>
          name === 'CapsLock' ? !!modifiers.caps : name === 'Shift' && !!modifiers.shift,
        ...init,
      }
      for (const fn of listeners[type] || []) fn(e)
      if (type === 'keydown' || type === 'keyup') input.keyEvent(type === 'keyup')
    }
    const flags = (key: 'Shift' | 'CapsLock', down: boolean) =>
      fire(
        down ? 'keydown' : 'keyup',
        { key, keyCode: key === 'Shift' ? 16 : 20 },
        {
          shift: key === 'Shift' && down,
          caps: key === 'CapsLock' && down,
        },
      )
    const letter = (ch: string, modifiers: { shift?: boolean; caps?: boolean } = {}) => {
      const code = ch.toUpperCase().charCodeAt(0)
      fire('keydown', { key: ch, keyCode: code }, modifiers)
      fire('keypress', { key: ch, keyCode: ch.charCodeAt(0), charCode: ch.charCodeAt(0) }, modifiers)
      input.input('insertText', ch)
      fire('keyup', { key: ch, keyCode: code }, modifiers)
    }
    return { remote, flags, letter }
  }

  const combos = [
    { shift: false, caps: false },
    { shift: false, caps: true },
    { shift: true, caps: false },
    { shift: true, caps: true },
  ]

  for (const modifiers of combos) {
    test(`a Shift tap capitalizes one letter (letter flags ${JSON.stringify(modifiers)})`, () => {
      const { remote, flags, letter } = ios()
      flags('Shift', true)
      letter('H', modifiers)
      flags('Shift', false)
      for (const ch of 'ello') letter(ch)
      expect(remote.text).toBe('Hello')
      expect(remote.capsLock).toBe(false)
    })

    test(`soft Caps Lock types capitals and leaves the remote Caps Lock alone (letter flags ${JSON.stringify(
      modifiers,
    )})`, () => {
      const { remote, flags, letter } = ios()
      flags('CapsLock', true)
      letter('A', modifiers)
      letter('B', modifiers)
      flags('CapsLock', false)
      letter('c')
      expect(remote.text).toBe('ABc')
      expect(remote.capsLock).toBe(false)
      expect(remote.keys.some((k) => Math.abs(k) === XK_CAPS_LOCK)).toBe(false)
    })
  }
})

describe('zoom and pan', () => {
  const element = { left: 0, top: 200, width: 400, height: 225 }
  const viewport = { left: 0, top: 0, width: 400, height: 625 }

  test('keeps the content point under the fingers while scaling', () => {
    const zoom = new ZoomPan(
      () => element,
      () => viewport,
      () => 8,
    )
    zoom.pinchStart({ x: 200, y: 312 }, 100)
    zoom.pinchMove({ x: 200, y: 312 }, 300)
    expect(zoom.scale).toBe(3)
    // content point (200, 112) in element px stays at client (200, 312)
    expect(element.left + zoom.tx + 200 * zoom.scale).toBeCloseTo(200)
    expect(element.top + zoom.ty + 112 * zoom.scale).toBeCloseTo(312)
  })

  test('clamps scale and keeps the viewport covered', () => {
    const zoom = new ZoomPan(
      () => element,
      () => viewport,
      () => 4,
    )
    zoom.pinchStart({ x: 10, y: 210 }, 100)
    zoom.pinchMove({ x: 390, y: 600 }, 1000)
    expect(zoom.scale).toBe(4)
    expect(element.left + zoom.tx).toBeLessThanOrEqual(viewport.left)
    expect(element.left + zoom.tx + element.width * 4).toBeGreaterThanOrEqual(viewport.left + viewport.width)
    expect(element.top + zoom.ty).toBeLessThanOrEqual(viewport.top)
    expect(element.top + zoom.ty + element.height * 4).toBeGreaterThanOrEqual(viewport.top + viewport.height)
  })

  test('never zooms out below the fitted size', () => {
    const zoom = new ZoomPan(
      () => element,
      () => viewport,
      () => 4,
    )
    zoom.pinchStart({ x: 200, y: 312 }, 300)
    zoom.pinchMove({ x: 200, y: 312 }, 100)
    expect(zoom.scale).toBe(1)
    expect(zoom.transform).toBe('')
  })
})

describe('touch gestures', () => {
  const touches = (...points: Point[]) => points.map((p) => ({ clientX: p.x, clientY: p.y })) as unknown as TouchList
  const event = (timeStamp: number, ...points: Point[]) =>
    ({ timeStamp, touches: touches(...points) } as unknown as TouchEvent)

  const recorder = (overrides: Partial<GestureHandlers> = {}) => {
    const calls: string[] = []
    const handlers = new Proxy(overrides as GestureHandlers, {
      get: (target, name: string) =>
        name in target
          ? target[name as keyof GestureHandlers]
          : (...args: unknown[]) => calls.push(`${name} ${JSON.stringify(args)}`),
    })
    return { calls, handlers }
  }

  test('a short touch without movement is a tap', () => {
    const { calls, handlers } = recorder()
    const g = new TouchGestures(handlers)
    g.touchStart(event(0, { x: 10, y: 10 }))
    g.touchMove(event(20, { x: 12, y: 11 }))
    g.touchEnd(event(60))
    g.destroy()
    expect(calls.map((c) => c.split(' ')[0])).toEqual(['onTouchBegin', 'onTap'])
  })

  test('movement past the slop scrolls by the finger delta', () => {
    const { calls, handlers } = recorder()
    const g = new TouchGestures(handlers)
    g.touchStart(event(0, { x: 100, y: 300 }))
    g.touchMove(event(16, { x: 100, y: 280 }))
    g.touchMove(event(32, { x: 100, y: 250 }))
    g.touchEnd(event(200))
    g.destroy()
    const scrolls = calls.filter((c) => c.startsWith('onScroll'))
    expect(scrolls.length).toBe(2)
    expect(scrolls[0]).toContain('[0,-20,')
    expect(scrolls[1]).toContain('[0,-30,')
    expect(calls.some((c) => c.startsWith('onTap'))).toBe(false)
  })

  test('two fingers pinch and never scroll or tap', () => {
    const { calls, handlers } = recorder()
    const g = new TouchGestures(handlers)
    g.touchStart(event(0, { x: 100, y: 300 }))
    g.touchStart(event(10, { x: 100, y: 300 }, { x: 200, y: 300 }))
    g.touchMove(event(26, { x: 80, y: 300 }, { x: 220, y: 300 }))
    g.touchEnd(event(40, { x: 220, y: 300 }))
    g.touchMove(event(56, { x: 260, y: 300 }))
    g.touchEnd(event(70))
    g.destroy()
    const names = calls.map((c) => c.split(' ')[0])
    expect(names).toEqual(['onTouchBegin', 'onPinchStart', 'onPinchMove', 'onPinchEnd'])
  })

  // fake timers and animation frames, swapped into the globals the recognizer uses
  const fakeTime = () => {
    let now = 0
    let timers: { fn: () => void; at: number; id: number }[] = []
    let frames = new Map<number, (t: number) => void>()
    let nextId = 1
    const saved = {
      setTimeout: globalThis.setTimeout,
      clearTimeout: globalThis.clearTimeout,
      requestAnimationFrame: globalThis.requestAnimationFrame,
      cancelAnimationFrame: globalThis.cancelAnimationFrame,
      now: performance.now,
    }
    Object.assign(globalThis, {
      setTimeout: (fn: () => void, ms: number) => {
        timers.push({ fn, at: now + ms, id: nextId })
        return nextId++
      },
      clearTimeout: (id: number) => {
        timers = timers.filter((t) => t.id !== id)
      },
      requestAnimationFrame: (fn: (t: number) => void) => {
        frames.set(nextId, fn)
        return nextId++
      },
      cancelAnimationFrame: (id: number) => frames.delete(id),
    })
    performance.now = () => now
    return {
      advance(ms: number) {
        now += ms
        for (const t of timers.filter((t) => t.at <= now)) {
          timers = timers.filter((x) => x !== t)
          t.fn()
        }
      },
      // runs pending animation frames 16ms apart, up to a limit
      frames(limit: number) {
        let ran = 0
        while (frames.size > 0 && ran < limit) {
          now += 16
          const pending = frames
          frames = new Map()
          for (const fn of pending.values()) fn(now)
          ran++
        }
        return ran
      },
      get pendingFrames() {
        return frames.size
      },
      restore() {
        Object.assign(globalThis, saved)
        performance.now = saved.now
      },
    }
  }

  const fling = (g: TouchGestures) => {
    g.touchStart(event(0, { x: 100, y: 400 }))
    g.touchMove(event(16, { x: 100, y: 380 }))
    g.touchMove(event(32, { x: 100, y: 340 }))
    g.touchMove(event(48, { x: 100, y: 300 }))
    g.touchEnd(event(50))
  }

  test('a fast swipe flings until the velocity decays', () => {
    const time = fakeTime()
    try {
      const { calls, handlers } = recorder()
      const g = new TouchGestures(handlers)
      fling(g)
      expect(g.flinging).toBe(true)
      const before = calls.length
      expect(time.frames(1000)).toBeLessThan(1000)
      expect(calls.length).toBeGreaterThan(before + 10)
      expect(g.flinging).toBe(false)
    } finally {
      time.restore()
    }
  })

  test('stopping the fling from the scroll handler ends it', () => {
    const time = fakeTime()
    try {
      let scrolls = 0
      const { handlers } = recorder({
        onScroll: () => {
          scrolls++
          if (g.flinging) g.stopFling()
        },
      })
      const g = new TouchGestures(handlers)
      fling(g)
      const swipe = scrolls
      time.frames(100)
      expect(scrolls).toBe(swipe + 1)
      expect(time.pendingFrames).toBe(0)
      expect(g.flinging).toBe(false)
    } finally {
      time.restore()
    }
  })

  test('cancel stops a fling and ends a scroll without further handlers', () => {
    const time = fakeTime()
    try {
      const { calls, handlers } = recorder()
      const g = new TouchGestures(handlers)
      fling(g)
      time.frames(2)
      g.cancel()
      expect(g.flinging).toBe(false)
      expect(time.pendingFrames).toBe(0)

      calls.length = 0
      g.touchStart(event(1000, { x: 100, y: 400 }))
      g.touchMove(event(1016, { x: 100, y: 380 }))
      g.cancel()
      g.touchMove(event(1032, { x: 100, y: 300 }))
      g.touchEnd(event(1034))
      time.frames(100)
      expect(calls.map((c) => c.split(' ')[0])).toEqual(['onTouchBegin', 'onScroll'])
    } finally {
      time.restore()
    }
  })

  test('cancel releases a drag once and delivers nothing after it', () => {
    const time = fakeTime()
    try {
      const { calls, handlers } = recorder()
      const g = new TouchGestures(handlers)
      g.touchStart(event(0, { x: 50, y: 60 }))
      time.advance(500)
      g.touchMove(event(520, { x: 80, y: 60 }))
      g.cancel()
      g.touchMove(event(540, { x: 120, y: 60 }))
      g.touchEnd(event(600))
      expect(calls.map((c) => c.split(' ')[0])).toEqual([
        'onTouchBegin',
        'onLongPress',
        'onDragStart',
        'onDragMove',
        'onDragEnd',
      ])
      expect(calls[calls.length - 1]).toBe('onDragEnd [{"x":80,"y":60}]')
    } finally {
      time.restore()
    }
  })

  test('a cancelled touch can still pinch', () => {
    const { calls, handlers } = recorder()
    const g = new TouchGestures(handlers)
    g.touchStart(event(0, { x: 100, y: 300 }))
    g.cancel()
    g.touchStart(event(10, { x: 100, y: 300 }, { x: 200, y: 300 }))
    g.touchMove(event(26, { x: 80, y: 300 }, { x: 220, y: 300 }))
    g.touchEnd(event(40))
    expect(calls.map((c) => c.split(' ')[0])).toEqual(['onTouchBegin', 'onPinchStart', 'onPinchMove', 'onPinchEnd'])
  })

  test('a touch the page does not take only pinches', () => {
    const time = fakeTime()
    try {
      const { calls, handlers } = recorder({ onTouchBegin: () => false })
      const g = new TouchGestures(handlers)
      g.touchStart(event(0, { x: 100, y: 300 }))
      time.advance(600)
      g.touchMove(event(620, { x: 100, y: 200 }))
      g.touchEnd(event(640))
      g.touchStart(event(1000, { x: 100, y: 300 }))
      g.touchEnd(event(1050))
      expect(calls).toEqual([])

      g.touchStart(event(2000, { x: 100, y: 300 }))
      g.touchStart(event(2010, { x: 100, y: 300 }, { x: 200, y: 300 }))
      g.touchEnd(event(2040))
      expect(calls.map((c) => c.split(' ')[0])).toEqual(['onPinchStart', 'onPinchEnd'])
    } finally {
      time.restore()
    }
  })

  test('a long press released in place is a right click', () => {
    const time = fakeTime()
    try {
      const { calls, handlers } = recorder()
      const g = new TouchGestures(handlers)
      g.touchStart(event(0, { x: 50, y: 60 }))
      time.advance(500)
      g.touchMove(event(520, { x: 53, y: 62 }))
      g.touchEnd(event(600))
      expect(calls.map((c) => c.split(' ')[0])).toEqual(['onTouchBegin', 'onLongPress', 'onLongPressRelease'])
    } finally {
      time.restore()
    }
  })

  test('a long press then move drags', () => {
    const time = fakeTime()
    try {
      const { calls, handlers } = recorder()
      const g = new TouchGestures(handlers)
      g.touchStart(event(0, { x: 50, y: 60 }))
      time.advance(500)
      g.touchMove(event(520, { x: 80, y: 60 }))
      g.touchMove(event(540, { x: 120, y: 60 }))
      g.touchEnd(event(600))
      expect(calls).toEqual([
        'onTouchBegin [{"x":50,"y":60}]',
        'onLongPress [{"x":50,"y":60}]',
        'onDragStart [{"x":50,"y":60}]',
        'onDragMove [{"x":80,"y":60}]',
        'onDragMove [{"x":120,"y":60}]',
        'onDragEnd [{"x":120,"y":60}]',
      ])
    } finally {
      time.restore()
    }
  })

  test('touchcancel ends a drag or pinch and never taps', () => {
    const time = fakeTime()
    try {
      const { calls, handlers } = recorder()
      const g = new TouchGestures(handlers)
      g.touchStart(event(0, { x: 50, y: 60 }))
      g.touchCancel()
      time.advance(600)
      g.touchEnd(event(700))

      g.touchStart(event(1000, { x: 50, y: 60 }))
      time.advance(500)
      g.touchMove(event(1520, { x: 90, y: 60 }))
      g.touchCancel()

      g.touchStart(event(2000, { x: 100, y: 300 }))
      g.touchStart(event(2010, { x: 100, y: 300 }, { x: 200, y: 300 }))
      g.touchCancel()
      expect(calls.map((c) => c.split(' ')[0])).toEqual([
        'onTouchBegin',
        'onTouchBegin',
        'onLongPress',
        'onDragStart',
        'onDragMove',
        'onDragEnd',
        'onTouchBegin',
        'onPinchStart',
        'onPinchEnd',
      ])
    } finally {
      time.restore()
    }
  })
})
