import { describe, expect, test } from 'bun:test'
import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import ts from 'typescript'
import { parseComponent } from 'vue-template-compiler'
import * as keyboardPan from '../src/utils/keyboard-pan'

// Load the real component methods without mounting its media player or child components.
const script = parseComponent(readFileSync(new URL('../src/components/video.vue', import.meta.url), 'utf8')).script!
  .content
const compiled = ts.transpileModule(script, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, experimentalDecorators: true },
}).outputText

function videoMethods(
  readText: () => Promise<string>,
  granted = async () => true,
  globals: Record<string, unknown> = {},
) {
  const exports: { default?: { prototype: Record<string, Function> } } = {}
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
      if (name === '~/utils/clipboard') return { isClipboardReadGranted: granted }
      if (name === '~/utils/keyboard-pan') return keyboardPan
      return {}
    },
    window: { self: {}, top: {}, document: { hasFocus: () => true }, setTimeout, clearTimeout },
    navigator: { clipboard: { readText } },
    setTimeout,
    ...globals,
  })
  return exports.default!.prototype
}

function context() {
  const sent: string[] = []
  const data: string[] = []
  const ctx = {
    hosting: true,
    locked: false,
    clipboard_read_available: true,
    clipboard: 'old remote clipboard',
    $accessor: {
      remote: {
        setClipboard(text: string) {
          ctx.clipboard = text
        },
        sendClipboard(text: string) {
          sent.push(text)
        },
      },
    },
    $log: {
      error: (error: unknown) => {
        throw error
      },
    },
    $client: { sendData: (event: string) => data.push(event) },
    keyMap: (key: number) => key,
    pastePending: false,
  }
  return { ctx, sent, data }
}

describe('video input during read-only transitions', () => {
  test('does not cache a clipboard read that resolves after locking; paste syncs after unlocking', async () => {
    let resolve!: (text: string) => void
    let begin!: () => void
    const started = new Promise<void>((done) => {
      begin = done
    })
    const methods = videoMethods(
      () =>
        new Promise<string>((done) => {
          resolve = done
          begin()
        }),
    )
    const { ctx, sent, data } = context()
    const pending = methods.syncClipboard.call(ctx)
    await started
    ctx.locked = true
    resolve('new clipboard')
    await pending
    expect(ctx.clipboard).toBe('old remote clipboard')
    expect(sent).toEqual([])

    ctx.locked = false
    await methods.onPaste.call(ctx, { clipboardData: { getData: () => 'new clipboard' } })
    expect(sent).toEqual(['new clipboard'])
    expect(data).toEqual(['keydown', 'keydown', 'keyup', 'keyup'])
  })

  test('does not begin a clipboard read if locked while waiting for permission', async () => {
    let resolve!: (granted: boolean) => void
    let reads = 0
    const methods = videoMethods(
      async () => {
        reads++
        return 'clipboard'
      },
      () =>
        new Promise<boolean>((done) => {
          resolve = done
        }),
    )
    const { ctx } = context()
    const pending = methods.syncClipboard.call(ctx)
    ctx.locked = true
    resolve(true)
    await pending
    expect(reads).toBe(0)
  })

  test('does not send a paste shortcut if locked during the clipboard propagation delay', async () => {
    const methods = videoMethods(async () => '')
    const { ctx, data } = context()
    const pending = methods.onPaste.call(ctx, { clipboardData: { getData: () => 'new clipboard' } })
    ctx.locked = true
    await pending
    expect(data).toEqual([])
  })

  test('mouse leave does not cache unsent modifier changes while locked', () => {
    const methods = videoMethods(async () => '')
    let reset = false
    methods.onMouseLeave.call(
      {
        hosting: true,
        locked: true,
        resetKeyboard: () => {
          reset = true
        },
      },
      {},
    )
    expect(reset).toBe(true)
  })

  test('unlocking while hovered keeps modifiers unsynchronized through leave and reentry', () => {
    const methods = videoMethods(async () => '')
    const sent: unknown[] = []
    const localModifiers = { capsLock: true, numLock: true, scrollLock: true }
    const remote = {
      keyboardModifierState: 0,
      setKeyboardModifierState: () => {
        remote.keyboardModifierState = 7
      },
      syncKeyboardModifierState: (value: unknown) => {
        sent.push(value)
      },
    }
    const ctx = { hosting: true, locked: true, $accessor: { remote }, resetKeyboard: () => {}, syncClipboard: () => {} }
    const event = { getModifierState: () => true }
    methods.onMouseLeave.call(ctx, event)
    expect(remote.keyboardModifierState).toBe(0)
    ctx.locked = false
    remote.keyboardModifierState = -1
    methods.onMouseLeave.call(ctx, event)
    expect(remote.keyboardModifierState).toBe(-1)
    methods.onMouseEnter.call(ctx, event)
    expect(sent).toEqual([localModifiers])
  })

  test('mouse entry does not synchronize keyboard modifiers or clipboard while locked', () => {
    const methods = videoMethods(async () => '')
    // These unprovided callbacks would throw if invoked while locked.
    methods.onMouseEnter.call({ hosting: true, locked: true }, {})
  })
})

describe('video touch input', () => {
  const touchContext = (sent: string[]) => ({
    zoomScale: 2,
    remotePoint: (p: { x: number; y: number }) => p,
    remotePerClient: () => 1,
    $client: { sendData: (event: string, data: unknown) => sent.push(`${event} ${JSON.stringify(data)}`) },
    tapKeyboard: { tap: () => {}, cancel: () => sent.push('keyboard cancel') },
    focusKeyboard: { browserTap: () => {}, cancel: () => sent.push('focus keyboard cancel') },
    $accessor: { video: { resolution: { w: 1920, h: 1080 } } },
    gestures: { cancel: () => sent.push('gestures cancel') },
    resetZoom: () => sent.push('reset zoom'),
    withdrawCopy: () => {},
    _player: { getBoundingClientRect: () => ({ top: 0 }) },
    keyboardOpen: false,
  })

  test('a double tap while zoomed clicks twice and keeps the zoom', () => {
    const methods = videoMethods(async () => '')
    const sent: string[] = []
    const ctx = {
      ...touchContext(sent),
      clickAt: (here: unknown, button: number) => methods.clickAt.call(ctx, here, button),
    }
    methods.onGestureTap.call(ctx, { x: 10, y: 20 })
    methods.onGestureTap.call(ctx, { x: 12, y: 21 })
    expect(sent).toEqual([
      'mousemove {"x":10,"y":20}',
      'mousedown {"key":1}',
      'mouseup {"key":1}',
      'mousemove {"x":12,"y":21}',
      'mousedown {"key":1}',
      'mouseup {"key":1}',
    ])
  })

  test('losing input ends the touch gesture and the keyboard wait', () => {
    const methods = videoMethods(async () => '')
    const sent: string[] = []
    const ctx = touchContext(sent)
    methods.onInputEnabledChanged.call(ctx, true)
    expect(sent).toEqual([])
    methods.onInputEnabledChanged.call(ctx, false)
    expect(sent).toEqual(['gestures cancel', 'keyboard cancel', 'focus keyboard cancel'])
  })
})

describe('touch keyboard opening', () => {
  const focus = (touch: boolean) => {
    const methods = videoMethods(async () => '')
    const calls: string[] = []
    const ctx = {
      hosting: true,
      is_touch_device: touch,
      keyboardOpen: false,
      tapKeyboard: { holding: false },
      focusKeyboard: { holding: false },
      $accessor: { remote: { releaseCapsLock: () => calls.push('release caps lock') } },
      syncClipboard: () => calls.push('sync clipboard'),
    }
    methods.onOverlayFocus.call(ctx)
    return { ctx, calls }
  }

  test('turns the remote Caps Lock off when the soft keyboard opens', () => {
    const { ctx, calls } = focus(true)
    expect(ctx.keyboardOpen).toBe(true)
    expect(calls).toEqual(['release caps lock', 'sync clipboard'])
  })

  test('leaves the remote Caps Lock alone on desktop', () => {
    expect(focus(false).calls).toEqual(['sync clipboard'])
  })
})

describe('video with the soft keyboard open', () => {
  // a 390x664 player, its last tap 500px down; the keyboard leaves the top 380px
  const keyboardContext = (open: boolean) => {
    const style: Record<string, string> = { width: '390px', height: '664px', transform: '' }
    const ctx: Record<string, any> = {
      is_touch_device: true,
      fullscreen: false,
      keyboardOpen: open,
      keyboardPan: 0,
      keyboardAnchor: 500,
      keyboardInset: 0,
      playerWidth: 390,
      playerHeight: 664,
      _component: { offsetWidth: 390, offsetHeight: 380, getBoundingClientRect: () => ({ top: 0, bottom: 380 }) },
      // mid-transition the on-screen position is neither the old nor the new pan
      _player: {
        style,
        offsetTop: 0,
        offsetHeight: 664,
        offsetParent: { getBoundingClientRect: () => ({ top: 0 }) },
        getBoundingClientRect: () => ({ top: -7, height: 664 }),
      },
      resetZoom: () => {
        throw new Error('the zoom must survive the keyboard')
      },
    }
    return { ctx, style }
  }

  test('keeps the video size and moves it up so the tap stays above the keyboard', () => {
    const methods = videoMethods(async () => '')
    const { ctx, style } = keyboardContext(true)
    ctx.updateKeyboardView = () => methods.updateKeyboardView.call(ctx)
    methods.onResize.call(ctx)
    expect(style.width).toBe('390px')
    expect(style.height).toBe('664px')
    expect(ctx.keyboardPan).toBe(216)
    expect(style.transform).toBe('translateY(-216px)')
    // the keyboard button is lifted by the part of the player still covered
    expect(ctx.keyboardInset).toBe(664 - 216 - 380)
  })

  test('drops the pan when the keyboard closes', () => {
    const methods = videoMethods(async () => '')
    const { ctx, style } = keyboardContext(false)
    ctx.keyboardPan = 216
    style.transform = 'translateY(-216px)'
    ctx._component.getBoundingClientRect = () => ({ top: 0, bottom: 664 })
    methods.updateKeyboardView.call(ctx)
    expect(ctx.keyboardPan).toBe(0)
    expect(style.transform).toBe('')
    expect(ctx.keyboardInset).toBe(0)
  })

  test('refits the video once the keyboard closes', () => {
    const methods = videoMethods(async () => '')
    const calls: string[] = []
    methods.onKeyboardOpenChanged.call(
      { is_touch_device: true, onResize: () => calls.push('resize'), updateKeyboardView: () => calls.push('view') },
      false,
    )
    expect(calls).toEqual(['resize'])
  })
})

// the remote side of the clipboard: control/clipboard updates and the keys sent
function clipboardSetup(globals: Record<string, unknown> = {}) {
  const timers: (() => void)[] = []
  const listeners: ((text: string) => void)[] = []
  const sent: string[] = []
  const typed: string[] = []
  const methods = videoMethods(async () => '', undefined, {
    window: { setTimeout: (fn: () => void) => timers.push(fn), clearTimeout: () => {} },
    ...globals,
  })
  const ctx: Record<string, any> = {
    inputEnabled: true,
    hosting: true,
    locked: false,
    is_touch_device: true,
    touchNotice: null,
    panel: null,
    copyWrites: 0,
    keyMap: (key: number) => key,
    textInput: { paste: (text: string) => typed.push(text) },
    $accessor: {
      remote: {
        setClipboard: () => {},
        sendClipboard: (text: string) => sent.push(`clipboard ${text}`),
      },
    },
    $client: {
      sendData: (event: string, data: { key: number }) => sent.push(`${event} ${data.key.toString(16)}`),
      on: (_: string, fn: (text: string) => void) => listeners.push(fn),
      off: (_: string, fn: (text: string) => void) => listeners.splice(listeners.indexOf(fn), 1),
    },
  }
  for (const name of ['nextRemoteClipboard', 'pressShortcut', 'pasteText']) {
    ctx[name] = (...args: unknown[]) => methods[name].call(ctx, ...args)
  }
  ctx.showTouchNotice = (text: string) => (ctx.touchNotice = text)
  ctx.openClipboard = (hint: string) => (ctx.panel = hint)
  return {
    ctx,
    methods,
    sent,
    typed,
    listeners,
    remoteClipboard: (text: string) => [...listeners].forEach((fn) => fn(text)),
    timeout: () => timers.splice(0).forEach((fn) => fn()),
  }
}
const settle = () => new Promise((done) => setTimeout(done, 20))
const long = 'a'.repeat(40)

describe('touch paste', () => {
  test('types short text', () => {
    const t = clipboardSetup()
    t.ctx.pasteText('123456\n')
    expect(t.typed).toEqual(['123456'])
    expect(t.sent).toEqual([])
  })

  test('pastes longer text through the remote clipboard and Ctrl+V once the remote confirms it', async () => {
    const t = clipboardSetup()
    t.ctx.pasteText(long)
    expect(t.sent).toEqual([`clipboard ${long}`])
    t.remoteClipboard(long)
    await settle()
    expect(t.sent.slice(1)).toEqual(['keydown ffe3', 'keydown 76', 'keyup 76', 'keyup ffe3'])
    expect(t.typed).toEqual([])
  })

  test('types longer text when the remote never confirms the clipboard', async () => {
    const t = clipboardSetup()
    t.ctx.pasteText(long)
    t.timeout()
    await settle()
    expect(t.typed).toEqual([long])
    expect(t.sent).toEqual([`clipboard ${long}`])
  })

  test('types longer text when the remote clipboard holds other text', async () => {
    const t = clipboardSetup()
    t.ctx.pasteText(long)
    t.remoteClipboard('something else')
    await settle()
    expect(t.typed).toEqual([long])
  })

  test('the Paste key reads the phone clipboard', async () => {
    const t = clipboardSetup({ navigator: { clipboard: { readText: async () => 'Secret1' } } })
    t.methods.pasteFromClipboard.call(t.ctx)
    await settle()
    expect(t.typed).toEqual(['Secret1'])
  })

  test('a refused or missing clipboard read opens the clipboard panel', async () => {
    const denied = clipboardSetup({
      navigator: { clipboard: { readText: () => Promise.reject(new DOMException('denied', 'NotAllowedError')) } },
    })
    denied.methods.pasteFromClipboard.call(denied.ctx)
    await settle()
    expect(denied.ctx.panel).toContain('not allowed')
    expect(denied.typed).toEqual([])

    const missing = clipboardSetup({ navigator: { clipboard: undefined } })
    missing.methods.pasteFromClipboard.call(missing.ctx)
    expect(missing.ctx.panel).toBe('Paste here, then tap Paste into page')
  })

  test('an empty clipboard says so', async () => {
    const t = clipboardSetup({ navigator: { clipboard: { readText: async () => '' } } })
    t.methods.pasteFromClipboard.call(t.ctx)
    await settle()
    expect(t.ctx.touchNotice).toBe('The clipboard is empty')
  })

  test('a native paste on a touch device goes the same way', async () => {
    const t = clipboardSetup()
    t.ctx.pastePending = true
    await t.methods.onPaste.call(t.ctx, { clipboardData: { getData: () => '123456' } })
    expect(t.typed).toEqual(['123456'])
    expect(t.ctx.pastePending).toBe(false)
  })
})

describe('clipboard panel', () => {
  test('on a touch device it closes the soft keyboard first, so the panel is not moved off screen', () => {
    const methods = videoMethods(async () => '')
    const calls: string[] = []
    const ctx = {
      is_touch_device: true,
      keyboardOpen: true,
      _overlay: { blur: () => calls.push('blur') },
      _clipboard: { open: (hint: string) => calls.push(`open ${hint}`) },
    }
    methods.openClipboard.call(ctx, 'Paste here')
    expect(calls).toEqual(['blur', 'open Paste here'])

    calls.length = 0
    methods.openClipboard.call({ ...ctx, is_touch_device: false })
    expect(calls).toEqual(['open undefined'])
  })
})

describe('touch copy', () => {
  class FakeClipboardItem {
    constructor(public items: Record<string, Promise<Blob>>) {}
  }
  const copySetup = (write?: (items: FakeClipboardItem[]) => Promise<void>) => {
    const written: string[] = []
    const clipboard = {
      write:
        write ||
        (async (items: FakeClipboardItem[]) => {
          written.push(await (await items[0].items['text/plain']).text())
        }),
    }
    const t = clipboardSetup({ ClipboardItem: FakeClipboardItem, Blob, navigator: { clipboard } })
    return { ...t, written, copy: () => t.methods.copyToClipboard.call(t.ctx) }
  }

  test('sends Ctrl+C and writes the remote clipboard text to the phone', async () => {
    const t = copySetup()
    t.copy()
    expect(t.sent).toEqual(['keydown ffe3', 'keydown 63', 'keyup 63', 'keyup ffe3'])
    t.remoteClipboard('Order #1234')
    await settle()
    expect(t.written).toEqual(['Order #1234'])
    expect(t.ctx.touchNotice).toBe('Copied')
    expect(t.listeners).toHaveLength(0)
    expect(t.ctx.copyWrites).toBe(0)
  })

  test('starts the clipboard write inside the tap', () => {
    let calledDuringTap = false
    const t = copySetup(() => {
      calledDuringTap = true
      return new Promise(() => {})
    })
    t.copy()
    expect(calledDuringTap).toBe(true)
  })

  test('says nothing was copied when the remote clipboard does not change', async () => {
    const t = copySetup()
    t.copy()
    t.timeout()
    await settle()
    expect(t.written).toEqual([])
    expect(t.ctx.touchNotice).toBe('Nothing was copied')
    expect(t.ctx.panel).toBe(null)
  })

  test('a refused clipboard write opens the clipboard panel', async () => {
    const t = copySetup(async (items) => {
      await items[0].items['text/plain']
      throw new DOMException('denied', 'NotAllowedError')
    })
    t.copy()
    t.remoteClipboard('secret')
    await settle()
    expect(t.ctx.panel).toContain('not allowed')
  })

  test('without ClipboardItem it copies on the remote and opens the clipboard panel', () => {
    const t = clipboardSetup({ ClipboardItem: undefined })
    t.methods.copyToClipboard.call(t.ctx)
    expect(t.sent).toEqual(['keydown ffe3', 'keydown 63', 'keyup 63', 'keyup ffe3'])
    expect(t.ctx.panel).toBe('Select the text and copy it')
  })

  test('the clipboard sync does not write over a pending Copy', async () => {
    const writes: string[] = []
    const methods = videoMethods(async () => '', undefined, {
      navigator: { clipboard: { writeText: async (text: string) => writes.push(text) } },
    })
    const ctx = { clipboard_write_available: true, copyWrites: 1, $accessor: { remote: { setClipboard: () => {} } } }
    await methods.onClipboardChanged.call(ctx, 'copied text')
    expect(writes).toEqual([])
    ctx.copyWrites = 0
    await methods.onClipboardChanged.call(ctx, 'copied text')
    expect(writes).toEqual(['copied text'])
  })

  test('Copy is offered after a drag selection and withdrawn by the next tap', () => {
    const methods = videoMethods(async () => '')
    const ctx: Record<string, any> = {
      copyOffered: false,
      pressedMouseButtons: new Set([1]),
      remotePoint: (p: unknown) => p,
      $client: { sendData: () => {} },
    }
    ctx.offerCopy = () => methods.offerCopy.call(ctx)
    ctx.withdrawCopy = () => methods.withdrawCopy.call(ctx)
    methods.onGestureDragEnd.call(ctx, { x: 1, y: 1 })
    expect(ctx.copyOffered).toBe(true)
    ctx.withdrawCopy()
    expect(ctx.copyOffered).toBe(false)
  })
})

describe('remote pointer after touch', () => {
  test('stays where the touch left it', () => {
    const methods = videoMethods(async () => '')
    const sent: string[] = []
    methods.onTouchEnd.call(
      { gestures: { touchEnd: () => {} }, $client: { sendData: (event: string) => sent.push(event) } },
      { touches: [] },
    )
    expect(sent).toEqual([])
  })
})
