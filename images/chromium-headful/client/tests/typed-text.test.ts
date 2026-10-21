import { describe, expect, test } from 'bun:test'
import { TextInput, XK_BACKSPACE, XK_SHIFT_L } from '../src/utils/text-input'
import { TypedText, isModifier, typesIntoOverlay } from '../src/utils/typed-text'

const SENTINEL = ' '

// the remote field as far as typing goes: text before the caret
function setup() {
  let text = ''
  let shift = false
  const input = new TextInput(
    (event, key) => {
      if (key === XK_SHIFT_L) {
        shift = event === 'keydown'
        return
      }
      if (event === 'keyup') return
      if (key === XK_BACKSPACE) {
        text = Array.from(text).slice(0, -1).join('')
        return
      }
      const ch = String.fromCodePoint(key & 0x00ffffff)
      text += shift ? ch.toUpperCase() : ch.toLowerCase()
    },
    () => true,
  )
  const typed = new TypedText(SENTINEL, {
    type: (t) => input.type(t),
    backspace: () => input.key(XK_BACKSPACE),
  })
  // the overlay's value after each edit the keyboard makes
  const edit = (...values: string[]) => values.map((value) => typed.edit(SENTINEL + value))
  return { typed, edit, remote: () => text }
}

describe('typed text', () => {
  test('sends what is typed into the overlay', () => {
    const t = setup()
    t.edit('H', 'He', 'Hel')
    expect(t.remote()).toBe('Hel')
  })

  test('an autocorrect replacement deletes the word and types the correction', () => {
    const t = setup()
    t.edit('t', 'te', 'teh')
    // iOS replaces the word, then inserts the space
    t.edit('the', 'the ')
    expect(t.remote()).toBe('the ')
    // or inserts the space and replaces the word with it
    t.edit('the w', 'the wrold', 'the wrold ', 'the world ')
    expect(t.remote()).toBe('the world ')
  })

  test('a predictive suggestion replaces the word being typed', () => {
    const t = setup()
    t.edit('I', 'I a', 'I am g', 'I am going ')
    expect(t.remote()).toBe('I am going ')
  })

  test('an edit before the end deletes back to it and retypes the rest', () => {
    const t = setup()
    t.edit('hello world', 'hello, world')
    expect(t.remote()).toBe('hello, world')
  })

  test('a composing keyboard rewrites its word', () => {
    const t = setup()
    t.edit('hel', 'help', 'hello')
    expect(t.remote()).toBe('hello')
  })

  test('deleting the sentinel sends a backspace for text typed before the overlay started over', () => {
    const t = setup()
    t.edit('ab')
    t.typed.reset()
    expect(t.typed.edit('')).toBe(false)
    expect(t.remote()).toBe('a')
  })

  test('backspace removes what was typed', () => {
    const t = setup()
    t.edit('abc', 'ab')
    expect(t.remote()).toBe('ab')
  })

  test('asks to start over once the text is long', () => {
    const t = setup()
    expect(t.edit('x'.repeat(1000))).toEqual([true])
    expect(t.edit('x'.repeat(1001))).toEqual([false])
  })

  test('only printable keys and Backspace are left to the overlay', () => {
    expect(typesIntoOverlay(0x61)).toBe(true)
    expect(typesIntoOverlay(0xe9)).toBe(true)
    expect(typesIntoOverlay(0x010020ac)).toBe(true)
    expect(typesIntoOverlay(XK_BACKSPACE)).toBe(true)
    // Return, Tab, arrows
    expect(typesIntoOverlay(0xff0d)).toBe(false)
    expect(typesIntoOverlay(0xff09)).toBe(false)
    expect(typesIntoOverlay(0xff51)).toBe(false)
    expect(isModifier(XK_SHIFT_L)).toBe(true)
    expect(isModifier(0xff51)).toBe(false)
  })
})
