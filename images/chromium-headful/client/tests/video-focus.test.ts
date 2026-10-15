import { describe, expect, test } from 'bun:test'
import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import ts from 'typescript'
import { parseComponent } from 'vue-template-compiler'
import * as fieldKeyboard from '../src/utils/field-keyboard'
import { FocusKeyboard } from '../src/utils/focus-keyboard'
import { RemoteField } from '../src/utils/remote-focus'
import { SoftKeyboard } from '../src/utils/soft-keyboard'
import { TapKeyboard } from '../src/utils/tap-keyboard'
import { TypedText } from '../src/utils/typed-text'

// Load the real component methods without mounting its media player or child components.
const script = parseComponent(readFileSync(new URL('../src/components/video.vue', import.meta.url), 'utf8')).script!
  .content
const compiled = ts.transpileModule(script, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, experimentalDecorators: true },
}).outputText

function videoComponent(globals: Record<string, unknown>) {
  const exports: { default?: { prototype: Record<string, any> } } = {}
  const noDecorator = () => () => undefined
  runInNewContext(compiled, {
    exports,
    require: (name: string) => {
      if (name === 'vue-property-decorator') {
        return {
          Vue: class {},
          Component: () => (type: unknown) => type,
          Ref: noDecorator,
          Watch: noDecorator,
          Prop: noDecorator,
        }
      }
      if (name === '~/utils/field-keyboard') return fieldKeyboard
      return {}
    },
    ...globals,
  })
  return exports.default!.prototype
}

const email: RemoteField = { type: 'email', rect: { x: 10, y: 120, w: 300, h: 40 } }
const notes: RemoteField = { type: 'text', rect: { x: 10, y: 200, w: 300, h: 40 } }
// a 390x844 phone; the keyboard leaves 500px of it
const FULL = 844
const WITH_KEYBOARD = 500

// The live view on iOS Safari, as far as the soft keyboard goes: the keyboard
// shows for a focus inside a tap, or when inputmode="none" is removed from an
// overlay focused inside one, covers the visual viewport while it is up, and
// goes away on a blur. The remote browser keeps or drops focus on its fields.
// zoomOnFirstFocus: iOS zooms the page in as the first field takes focus.
// ignoreHeld: iOS shows the keyboard for a focus with inputmode="none".
// hardwareKeyboard: no soft keyboard ever shows.
function phone(options: { zoomOnFirstFocus?: boolean; ignoreHeld?: boolean; hardwareKeyboard?: boolean } = {}) {
  let now = 0
  let timers: { fn: () => void; at: number; id: number }[] = []
  let nextTimer = 1
  const clock = {
    now: () => now,
    setTimeout: (fn: () => void, ms: number) => {
      timers.push({ fn, at: now + ms, id: nextTimer })
      return nextTimer++
    },
    clearTimeout: (id: number) => {
      timers = timers.filter((t) => t.id !== id)
    },
  }
  const advance = (ms: number) => {
    const until = now + ms
    for (;;) {
      const due = timers.filter((t) => t.at <= until).sort((a, b) => a.at - b.at)[0]
      if (!due) break
      timers = timers.filter((t) => t !== due)
      now = due.at
      due.fn()
    }
    now = until
  }

  const document = { activeElement: null as unknown, hasFocus: () => true }
  const viewport = { width: 390, height: FULL, scale: 1 }
  const window = { visualViewport: viewport, innerWidth: 390, innerHeight: FULL, self: 1, top: 1 }
  const blurs: string[] = []
  const fetch = (url: string) => {
    blurs.push(url)
    return Promise.resolve()
  }
  const proto = videoComponent({ document, window, fetch, performance: { now: () => now } })

  let inTap = false
  let keyboardUp = false
  let zoomed = false
  const attrs = new Map<string, string>()
  const setKeyboard = (up: boolean) => {
    if (options.hardwareKeyboard) return
    if (up && options.zoomOnFirstFocus && !zoomed) {
      // the zoom comes in the same viewport change as the keyboard; iOS reports
      // the zoomed viewport through the window's size too
      zoomed = true
      viewport.scale = 1.2
      viewport.width = 390 / 1.2
      window.innerWidth = 390 / 1.2
      window.innerHeight = FULL / 1.2
    }
    keyboardUp = up
    viewport.height = (up ? WITH_KEYBOARD : FULL) / viewport.scale
    ctx.measureSoftKeyboard()
  }
  const el: Record<string, any> = {
    attrs,
    value: '',
    getAttribute: (name: string) => (attrs.has(name) ? attrs.get(name) : null),
    setAttribute: (name: string, value: string) => {
      attrs.set(name, value)
    },
    removeAttribute: (name: string) => {
      const held = name === 'inputmode' && attrs.get(name) === 'none' && document.activeElement === el
      attrs.delete(name)
      if (held) setKeyboard(true)
    },
    setSelectionRange: () => {},
    focus: () => {
      if (document.activeElement === el) return
      document.activeElement = el
      ctx.onOverlayFocus()
      if (inTap && (attrs.get('inputmode') !== 'none' || options.ignoreHeld)) setKeyboard(true)
    },
    blur: () => {
      if (document.activeElement !== el) return
      document.activeElement = null
      ctx.onOverlayBlur()
      setKeyboard(false)
    },
  }

  // the remote page: which field has focus, and where the fields are
  const remote = { focused: null as RemoteField | null, live: true, seq: 0 }
  const ctx: Record<string, any> = {
    is_touch_device: true,
    hosting: true,
    inputEnabled: true,
    showTouchControls: true,
    keyboardOpen: false,
    keepText: false,
    overlayHeld: false,
    overlayFocused: false,
    ownBlur: false,
    softKeyboardUp: false,
    keyboardMeasurable: false,
    keyboardChangedAt: -Infinity,
    keyboardAnchor: null,
    blurUrl: 'https://p/browser/kernel/live-view/focus/blur?jwt=x',
    softKeyboard: new SoftKeyboard(),
    _overlay: el,
    _player: { getBoundingClientRect: () => ({ top: 0 }) },
    $accessor: { video: { resolution: { w: 390, h: 844 } }, remote: { releaseCapsLock: () => {} } },
    $client: { sendData: () => {} },
    textInput: { composing: false, latchCtrl: () => {} },
    typedText: new TypedText(' ', { type: () => {}, backspace: () => {} }),
    remoteFocus: {
      get live() {
        return remote.live
      },
      get state() {
        return { focus: remote.focused && { ...remote.focused, seq: remote.seq } }
      },
      // the address bar is above y=87
      inViewport: (p: { y: number }) => p.y >= 87,
      fieldAt: (p: { x: number; y: number }) =>
        [email, notes].find((f) => p.y >= f.rect!.y && p.y <= f.rect!.y + f.rect!.h) || null,
    },
    syncClipboard: () => {},
    resetKeyboard: () => {},
    withdrawCopy: () => {},
    updateKeyboardView: () => {},
    clickAt: () => {},
    remotePoint: (p: unknown) => p,
    remotePerClient: () => 1,
  }
  for (const name of ['keyboardShown', 'showTouchKeys', 'focusLive']) {
    Object.defineProperty(ctx, name, { get: () => Object.getOwnPropertyDescriptor(proto, name)!.get!.call(ctx) })
  }
  for (const name of Object.getOwnPropertyNames(proto)) {
    const descriptor = Object.getOwnPropertyDescriptor(proto, name)!
    if (typeof descriptor.value === 'function' && name !== 'constructor' && !(name in ctx)) {
      ctx[name] = descriptor.value.bind(ctx)
    }
  }
  ctx.tapKeyboard = new TapKeyboard(
    'auto',
    true,
    {
      keyboardOpen: () => ctx.keyboardOpen,
      focus: () => ctx.focusForTyping(null),
      hold: ctx.holdFocus,
      release: ctx.blurOverlay,
    },
    clock,
  )
  ctx.focusKeyboard = new FocusKeyboard(
    'auto',
    true,
    {
      keyboardOpen: () => ctx.keyboardOpen,
      focus: ctx.focusForTyping,
      update: () => {},
      hold: ctx.holdFocus,
      release: ctx.blurOverlay,
    },
    clock,
  )
  ctx.measureSoftKeyboard()

  // a tap at a remote point; the click focuses the field under it
  const tap = (p: { x: number; y: number }) => {
    ctx.tapKeyboard.touchBegin()
    ctx.focusKeyboard.touchBegin()
    advance(80)
    inTap = true
    ctx.onGestureTap(p)
    inTap = false
    advance(30)
    // the click is a press for the remote's count, and focuses the field under it
    remote.seq++
    const field = ctx.remoteFocus.fieldAt(p)
    if (p.y >= 87 && field && remote.focused !== field) report(field)
  }
  const report = (field: RemoteField | null, pageFocused = true) => {
    remote.focused = field
    ctx.focusKeyboard.focus(field, pageFocused)
  }
  // the Keyboard / Hide control, which runs inside its own tap
  const toggle = () => {
    inTap = true
    ctx.toggleMobileKeyboard()
    inTap = false
  }

  return {
    ctx,
    el,
    tap,
    toggle,
    report,
    advance,
    blurs,
    document,
    setKeyboard,
    // to landscape, the keyboard staying as it is; the overlay keeps focus
    rotate: () => {
      viewport.width = 844
      viewport.height = keyboardUp ? 200 : 390
      window.innerWidth = 844
      window.innerHeight = 390
      ctx.measureSoftKeyboard()
    },
    // iOS dismisses the keyboard in landscape without a blur
    dismissLandscape: () => {
      keyboardUp = false
      viewport.height = 390
      ctx.measureSoftKeyboard()
    },
    keyboard: () => keyboardUp,
    row: () => ctx.showTouchKeys,
    setCursor: (kind: 'text' | 'other') => ctx.tapKeyboard.cursor(kind),
  }
}

describe('hiding and raising the keyboard again', () => {
  test('tap a field, Hide, tap it again: the keyboard comes back and the remote field was blurred', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    expect(t.keyboard()).toBe(true)
    expect(t.row()).toBe(true)

    t.toggle()
    expect(t.keyboard()).toBe(false)
    expect(t.row()).toBe(false)
    expect(t.blurs).toEqual(['https://p/browser/kernel/live-view/focus/blur?jwt=x&seq=1'])
    t.report(null)
    t.advance(1000)

    t.tap({ x: 50, y: 140 })
    expect(t.keyboard()).toBe(true)
    t.advance(1000)
    expect(t.keyboard()).toBe(true)
    expect(t.row()).toBe(true)
  })

  test('a tap on a field that kept its focus raises the keyboard inside the tap, without a new focus report', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    t.toggle()
    // the remote blur did not happen: the field still has focus
    t.tap({ x: 50, y: 140 })
    expect(t.keyboard()).toBe(true)
    t.advance(1000)
    expect(t.keyboard()).toBe(true)
  })

  test('iOS dismissing the keyboard and leaving the overlay focused is a Done', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    // the keyboard goes down; the overlay keeps its focus
    t.setKeyboard(false)
    expect(t.document.activeElement).toBe(null)
    expect(t.row()).toBe(false)
    expect(t.blurs).toHaveLength(1)
    t.report(null)

    t.tap({ x: 50, y: 140 })
    expect(t.keyboard()).toBe(true)
  })

  test("the keyboard's own Done blurs the remote field; leaving the page does not", () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    t.el.blur()
    expect(t.blurs).toHaveLength(1)

    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    t.document.hasFocus = () => false
    t.el.blur()
    expect(t.blurs).toHaveLength(1)
  })

  test('closing the keyboard for the page is not a Done', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    // a submit that navigates
    t.report(null)
    t.advance(1000)
    expect(t.keyboard()).toBe(false)
    expect(t.blurs).toEqual([])
  })

  test('a focus that iOS shows no keyboard for leaves the extra keys hidden, and a tap focuses afresh', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    t.toggle()
    t.report(null)
    // the keyboard button focuses the overlay, but no keyboard comes up
    t.ctx._overlay.focus()
    expect(t.ctx.keyboardOpen).toBe(true)
    expect(t.row()).toBe(false)

    t.tap({ x: 50, y: 140 })
    expect(t.keyboard()).toBe(true)
    expect(t.row()).toBe(true)
  })
})

describe('the first keyboard after loading the live view', () => {
  test('a first tap on a field shows the keyboard state, Hide and the keys row', () => {
    for (const options of [{}, { zoomOnFirstFocus: true }]) {
      const t = phone(options)
      t.tap({ x: 50, y: 140 })
      expect(t.keyboard()).toBe(true)
      expect(t.ctx.keyboardShown).toBe(true)
      expect(t.row()).toBe(true)
      t.advance(1000)
      expect(t.ctx.keyboardShown).toBe(true)

      // the control says Hide, and does
      t.toggle()
      expect(t.keyboard()).toBe(false)
      expect(t.ctx.keyboardShown).toBe(false)
      t.report(null)
      t.tap({ x: 50, y: 140 })
      expect(t.ctx.keyboardShown).toBe(true)
      expect(t.row()).toBe(true)
    }
  })

  test('a keyboard iOS shows for the held focus counts as shown', () => {
    const t = phone({ ignoreHeld: true })
    // no field under the tap: the overlay is held, and iOS shows the keyboard anyway
    t.tap({ x: 50, y: 400 })
    expect(t.keyboard()).toBe(true)
    expect(t.ctx.keyboardShown).toBe(true)
    expect(t.row()).toBe(true)
    // the control hides it
    t.toggle()
    expect(t.keyboard()).toBe(false)
    expect(t.row()).toBe(false)
  })
})

describe('rotating with the keyboard up', () => {
  test('a dismissal that comes with the rotation is a Done', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    t.dismissLandscape()
    // the rotation and the dismissal in one viewport change
    t.rotate()
    expect(t.document.activeElement).toBe(null)
    expect(t.row()).toBe(false)
    expect(t.blurs).toHaveLength(1)
  })

  test('keeps Hide and the keys row, and a dismissal after it is still a Done', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    t.rotate()
    expect(t.ctx.keyboardShown).toBe(true)
    expect(t.row()).toBe(true)
    t.dismissLandscape()
    expect(t.document.activeElement).toBe(null)
    expect(t.row()).toBe(false)
    expect(t.blurs).toHaveLength(1)
  })
})

describe('with a hardware keyboard', () => {
  test('Hide and the control follow the focus', () => {
    const t = phone({ hardwareKeyboard: true })
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    expect(t.ctx.keyboardShown).toBe(true)
    t.toggle()
    expect(t.document.activeElement).toBe(null)
    expect(t.ctx.keyboardShown).toBe(false)
  })
})

describe('the address bar', () => {
  test('tap the address bar, Hide, tap it again: the keyboard comes back', () => {
    const t = phone()
    t.tap({ x: 160, y: 63 })
    // the pointer moved over the address bar
    t.setCursor('text')
    expect(t.keyboard()).toBe(true)
    expect(t.row()).toBe(true)

    t.toggle()
    expect(t.keyboard()).toBe(false)
    expect(t.row()).toBe(false)
    // no page field has focus to blur
    expect(t.blurs).toEqual([])

    // where a text cursor was seen, the keyboard comes up inside the tap
    t.tap({ x: 160, y: 63 })
    expect(t.keyboard()).toBe(true)
    t.advance(1000)
    expect(t.keyboard()).toBe(true)
    expect(t.row()).toBe(true)
  })
})

describe('raising the keyboard for a remote field', () => {
  test('copies the field onto the overlay before focusing it', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    expect(Object.fromEntries(t.el.attrs)).toEqual({
      autocomplete: 'email',
      autocorrect: 'off',
      autocapitalize: 'off',
      spellcheck: 'false',
      inputmode: 'email',
    })
    expect(t.ctx.keepText).toBe(false)
  })

  test('refocuses an open keyboard inside the tap when more than its layout changes', () => {
    const t = phone()
    t.tap({ x: 50, y: 140 })
    t.advance(1000)
    const blur = t.el.blur
    let blurred = 0
    t.el.blur = () => {
      blurred++
      blur()
    }
    t.tap({ x: 50, y: 220 })
    expect(blurred).toBe(1)
    expect(t.keyboard()).toBe(true)
    expect(t.el.attrs.get('autocorrect')).toBe('on')
    expect(t.ctx.keepText).toBe(true)
    // refocusing is not a Done
    expect(t.blurs).toEqual([])
  })

  test('a held focus only changes its layout, which raises the keyboard', () => {
    const t = phone()
    // a tap with no field under it holds the overlay
    t.tap({ x: 50, y: 400 })
    expect(t.el.attrs.get('inputmode')).toBe('none')
    t.report(notes)
    expect(t.keyboard()).toBe(true)
    // iOS keeps the attributes it was focused with
    expect(t.el.attrs.get('autocorrect')).toBe('off')
    expect(t.ctx.keepText).toBe(false)
  })
})

describe('overlay text for autocorrect', () => {
  const textSetup = () => {
    const t = phone()
    const sent: string[] = []
    t.ctx.typedText = new TypedText(' ', {
      type: (s) => sent.push(`type ${s}`),
      backspace: () => sent.push('backspace'),
    })
    t.tap({ x: 50, y: 220 })
    return { t, sent }
  }

  test('sends the edits of a field that keeps its text', () => {
    const { t, sent } = textSetup()
    for (const value of [' t', ' te', ' teh', ' the', ' the ']) {
      t.el.value = value
      t.ctx.onOverlayInput({ inputType: 'insertText', data: null })
    }
    expect(sent).toEqual(['type t', 'type e', 'type h', 'backspace', 'backspace', 'type he', 'type  '])
    expect(t.el.value).toBe(' the ')
  })

  test('starts over when the sentinel is deleted', () => {
    const { t, sent } = textSetup()
    t.el.value = ''
    t.ctx.onOverlayInput({ inputType: 'deleteContentBackward', data: null })
    expect(sent).toEqual(['backspace'])
    expect(t.el.value).toBe(' ')
  })
})

describe('taps with the focus stream', () => {
  const tapContext = (live: { live: boolean; inViewport: boolean; field: unknown } | null) => {
    const calls: string[] = []
    const ctx: Record<string, any> = {
      keepText: false,
      keyboardOpen: false,
      remotePoint: (p: unknown) => p,
      remotePerClient: () => 1,
      clickAt: () => calls.push('click'),
      withdrawCopy: () => {},
      _player: { getBoundingClientRect: () => ({ top: 0 }) },
      $accessor: { video: { resolution: { w: 390, h: 844 } } },
      remoteFocus: live && {
        inViewport: () => live.inViewport,
        fieldAt: () => live.field,
      },
      focusLive: !!live && live.live,
      tapKeyboard: { tap: () => calls.push('cursor tap'), cancel: () => calls.push('cursor cancel') },
      focusKeyboard: {
        tap: (field: { type: string } | null) => calls.push(`focus tap ${field ? field.type : 'nothing'}`),
        cursorTap: (browserUi: boolean) => calls.push(browserUi ? 'browser tap' : 'untracked tap'),
      },
    }
    return { ctx, calls }
  }
  const methods = videoComponent({})

  test('a tap on the page follows the remote focus, with the field under it', () => {
    const { ctx, calls } = tapContext({ live: true, inViewport: true, field: { type: 'email' } })
    methods.onGestureTap.call(ctx, { x: 10, y: 200 })
    expect(calls).toEqual(['click', 'cursor cancel', 'focus tap email'])
  })

  test("a tap on the browser's UI uses the cursor shape", () => {
    const { ctx, calls } = tapContext({ live: true, inViewport: false, field: null })
    methods.onGestureTap.call(ctx, { x: 10, y: 40 })
    expect(calls).toEqual(['click', 'browser tap', 'cursor tap'])
  })

  test('without the focus stream every tap uses the cursor shape', () => {
    for (const live of [null, { live: false, inViewport: true, field: null }]) {
      const { ctx, calls } = tapContext(live)
      methods.onGestureTap.call(ctx, { x: 10, y: 200 })
      expect(calls).toEqual(['click', 'untracked tap', 'cursor tap'])
    }
  })
})
