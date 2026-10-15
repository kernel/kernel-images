import { afterEach, describe, expect, test } from 'bun:test'

import createKeyboard from '../src/utils/guacamole-keyboard'

class KeyboardTarget {
  private listeners = new Map<string, Array<(event: KeyboardEvent) => void>>()

  addEventListener(type: string, listener: (event: KeyboardEvent) => void) {
    const listeners = this.listeners.get(type) ?? []
    listeners.push(listener)
    this.listeners.set(type, listeners)
  }

  dispatch(type: string, key: string, keyCode: number, repeat = false, capsLock = false) {
    const event = {
      key,
      keyCode,
      which: keyCode,
      location: 0,
      repeat,
      shiftKey: false,
      ctrlKey: false,
      altKey: false,
      metaKey: false,
      preventDefault() {},
      getModifierState(name: string) {
        return name === 'CapsLock' && capsLock
      },
    } as KeyboardEvent

    for (const listener of this.listeners.get(type) ?? []) {
      listener(event)
    }
  }
}

describe('GuacamoleKeyboard repeat handling', () => {
  test('does not synthesize repeats while waiting for keyup', async () => {
    const keyboard = createKeyboard()
    const events: string[] = []
    keyboard.onkeydown = (key) => {
      events.push(`down:${key}`)
      return false
    }
    keyboard.onkeyup = (key) => events.push(`up:${key}`)

    keyboard.press(0x006f)
    await Bun.sleep(550)
    keyboard.release(0x006f)

    expect(events).toEqual(['down:111', 'up:111'])
  })

  test('forwards native browser repeat events as release and press pairs', () => {
    const target = new KeyboardTarget()
    const keyboard = createKeyboard(target as unknown as Element)
    const events: string[] = []
    keyboard.onkeydown = (key) => {
      events.push(`down:${key}`)
      return false
    }
    keyboard.onkeyup = (key) => events.push(`up:${key}`)

    target.dispatch('keydown', 'o', 79)
    target.dispatch('keypress', 'o', 111)
    target.dispatch('keydown', 'o', 79, true)
    target.dispatch('keypress', 'o', 111, true)
    target.dispatch('keyup', 'o', 79)

    expect(events).toEqual(['down:111', 'up:111', 'down:111', 'up:111'])
  })
})

describe('GuacamoleKeyboard Caps Lock handling', () => {
  const platform = Object.getOwnPropertyDescriptor(navigator, 'platform')!

  afterEach(() => {
    Object.defineProperty(navigator, 'platform', platform)
  })

  function keyboardOn(platformName: string) {
    Object.defineProperty(navigator, 'platform', { value: platformName, configurable: true })
    const target = new KeyboardTarget()
    const keyboard = createKeyboard(target as unknown as Element)
    const events: string[] = []
    keyboard.onkeydown = (key) => {
      events.push(`down:${key}`)
      return false
    }
    keyboard.onkeyup = (key) => events.push(`up:${key}`)
    keyboard.oncapslock = (capsLock) => events.push(`caps:${capsLock}`)
    return { target, events }
  }

  test('reports macOS Caps Lock keydown and keyup as lock state', () => {
    const { target, events } = keyboardOn('MacIntel')

    target.dispatch('keydown', 'CapsLock', 20, false, true)
    target.dispatch('keydown', 'a', 65, false, true)
    target.dispatch('keypress', 'A', 65, false, true)
    target.dispatch('keyup', 'a', 65, false, true)
    target.dispatch('keyup', 'CapsLock', 20, false, false)

    expect(events).toEqual(['caps:true', 'down:65', 'up:65', 'caps:false'])
  })

  test('keeps Caps Lock as a key press on other platforms', () => {
    const { target, events } = keyboardOn('Win32')

    target.dispatch('keydown', 'CapsLock', 20, false, true)
    target.dispatch('keyup', 'CapsLock', 20, false, true)

    expect(events).toEqual(['down:65509', 'up:65509'])
  })
})
