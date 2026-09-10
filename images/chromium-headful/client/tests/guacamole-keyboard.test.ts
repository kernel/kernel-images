import { describe, expect, test } from 'bun:test'

import createKeyboard from '../src/utils/guacamole-keyboard'

class KeyboardTarget {
  private listeners = new Map<string, Array<(event: KeyboardEvent) => void>>()

  addEventListener(type: string, listener: (event: KeyboardEvent) => void) {
    const listeners = this.listeners.get(type) ?? []
    listeners.push(listener)
    this.listeners.set(type, listeners)
  }

  dispatch(type: string, key: string, keyCode: number, repeat = false) {
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
      getModifierState() {
        return false
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
