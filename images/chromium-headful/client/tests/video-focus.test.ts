import { describe, expect, test } from 'bun:test'
import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import ts from 'typescript'
import { parseComponent } from 'vue-template-compiler'
import * as fieldKeyboard from '../src/utils/field-keyboard'
import { TypedText } from '../src/utils/typed-text'

// Load the real component methods without mounting its media player or child components.
const script = parseComponent(readFileSync(new URL('../src/components/video.vue', import.meta.url), 'utf8')).script!
  .content
const compiled = ts.transpileModule(script, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, experimentalDecorators: true },
}).outputText

function videoMethods(document: { activeElement: unknown }) {
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
      if (name === '~/utils/field-keyboard') return fieldKeyboard
      return {}
    },
    document,
  })
  return exports.default!.prototype
}

// the overlay textarea, focused or not, with its attributes
function overlay(document: { activeElement: unknown }, events: string[]) {
  const attrs = new Map<string, string>()
  const el: Record<string, any> = {
    attrs,
    value: '',
    getAttribute: (name: string) => (attrs.has(name) ? attrs.get(name) : null),
    setAttribute: (name: string, value: string) => attrs.set(name, value),
    removeAttribute: (name: string) => attrs.delete(name),
    setSelectionRange: () => {},
    focus: () => {
      events.push('focus')
      document.activeElement = el
    },
    blur: () => {
      events.push('blur')
      document.activeElement = null
    },
  }
  return el
}

function setup() {
  const document: { activeElement: unknown } = { activeElement: null }
  const events: string[] = []
  const methods = videoMethods(document)
  const sent: string[] = []
  const ctx: Record<string, any> = {
    is_touch_device: true,
    inputEnabled: true,
    keyboardOpen: false,
    keepText: false,
    _overlay: overlay(document, events),
    textInput: { composing: false },
    typedText: new TypedText(' ', { type: (t) => sent.push(`type ${t}`), backspace: () => sent.push('backspace') }),
    onOverlayFocus: () => {
      events.push('keyboard')
      ctx.keyboardOpen = true
    },
  }
  for (const name of ['resetInputSentinel', 'focusForTyping', 'onOverlayInput']) {
    ctx[name] = (...args: unknown[]) => methods[name].apply(ctx, args)
  }
  return { ctx, methods, events, sent, document }
}

describe('raising the keyboard for a remote field', () => {
  test('copies the field onto the overlay before focusing it', () => {
    const { ctx, events } = setup()
    ctx.focusForTyping({ type: 'text', autocomplete: 'one-time-code', inputMode: 'numeric' })
    expect(events).toEqual(['focus'])
    expect(Object.fromEntries(ctx._overlay.attrs)).toEqual({
      autocomplete: 'one-time-code',
      autocorrect: 'off',
      autocapitalize: 'off',
      spellcheck: 'false',
      inputmode: 'numeric',
    })
    expect(ctx.keepText).toBe(false)
  })

  test('refocuses an open keyboard inside the tap when more than its layout changes', () => {
    const { ctx, events } = setup()
    ctx.focusForTyping({ type: 'url' })
    ctx.keyboardOpen = true
    ctx.focusForTyping({ type: 'number' })
    expect(events).toEqual(['focus'])
    expect(ctx._overlay.attrs.get('inputmode')).toBe('decimal')
    ctx.focusForTyping({ type: 'textarea' })
    expect(events).toEqual(['focus', 'blur', 'focus'])
    expect(ctx._overlay.attrs.get('autocorrect')).toBe('on')
    expect(ctx.keepText).toBe(true)
  })

  test('a held focus only changes its layout, which raises the keyboard', () => {
    const { ctx, events } = setup()
    ctx._overlay.focus()
    ctx._overlay.attrs.set('inputmode', 'none')
    ctx._overlay.attrs.set('autocorrect', 'off')
    ctx.focusForTyping({ type: 'textarea' }, false)
    expect(events).toEqual(['focus', 'keyboard'])
    expect(ctx._overlay.attrs.has('inputmode')).toBe(false)
    // iOS keeps the attributes it was focused with
    expect(ctx._overlay.attrs.get('autocorrect')).toBe('off')
    expect(ctx.keepText).toBe(false)
  })
})

describe('overlay text for autocorrect', () => {
  test('sends the edits of a field that keeps its text', () => {
    const { ctx, methods, sent } = setup()
    ctx.focusForTyping({ type: 'textarea' })
    for (const value of [' t', ' te', ' teh', ' the', ' the ']) {
      ctx._overlay.value = value
      methods.onOverlayInput.call(ctx, { inputType: 'insertText', data: null })
    }
    expect(sent).toEqual(['type t', 'type e', 'type h', 'backspace', 'backspace', 'type he', 'type  '])
    expect(ctx._overlay.value).toBe(' the ')
  })

  test('starts over when the sentinel is deleted', () => {
    const { ctx, methods, sent } = setup()
    ctx.focusForTyping({ type: 'textarea' })
    ctx._overlay.value = ''
    methods.onOverlayInput.call(ctx, { inputType: 'deleteContentBackward', data: null })
    expect(sent).toEqual(['backspace'])
    expect(ctx._overlay.value).toBe(' ')
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

  test('a tap on the page follows the remote focus, with the field under it', () => {
    const methods = videoMethods({ activeElement: null })
    const { ctx, calls } = tapContext({ live: true, inViewport: true, field: { type: 'email' } })
    methods.onGestureTap.call(ctx, { x: 10, y: 200 })
    expect(calls).toEqual(['click', 'cursor cancel', 'focus tap email'])
  })

  test("a tap on the browser's UI uses the cursor shape", () => {
    const methods = videoMethods({ activeElement: null })
    const { ctx, calls } = tapContext({ live: true, inViewport: false, field: null })
    methods.onGestureTap.call(ctx, { x: 10, y: 40 })
    expect(calls).toEqual(['click', 'browser tap', 'cursor tap'])
  })

  test('without the focus stream every tap uses the cursor shape', () => {
    const methods = videoMethods({ activeElement: null })
    for (const live of [null, { live: false, inViewport: true, field: null }]) {
      const { ctx, calls } = tapContext(live)
      methods.onGestureTap.call(ctx, { x: 10, y: 200 })
      expect(calls).toEqual(['click', 'untracked tap', 'cursor tap'])
    }
  })
})
