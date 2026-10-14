import { describe, expect, test } from 'bun:test'
import { DEFAULT_KEYBOARD, applyKeyboard, fieldKind, keyboardFor } from '../src/utils/field-keyboard'

describe('field keyboard', () => {
  test('sorts fields by the keyboard they need', () => {
    expect(fieldKind({ type: 'password' })).toBe('password')
    expect(fieldKind({ type: 'text', autocomplete: 'current-password' })).toBe('password')
    expect(fieldKind({ type: 'text', autocomplete: 'one-time-code' })).toBe('code')
    expect(fieldKind({ type: 'tel', code: true })).toBe('code')
    expect(fieldKind({ type: 'email' })).toBe('email')
    expect(fieldKind({ type: 'text', autocomplete: 'section-login username' })).toBe('username')
    expect(fieldKind({ type: 'url' })).toBe('url')
    expect(fieldKind({ type: 'text', autocomplete: 'tel-national' })).toBe('tel')
    expect(fieldKind({ type: 'number' })).toBe('number')
    expect(fieldKind({ type: 'text', inputMode: 'decimal' })).toBe('number')
    expect(fieldKind({ type: 'search' })).toBe('text')
    expect(fieldKind({ type: 'textarea' })).toBe('text')
    expect(fieldKind({ type: 'contenteditable' })).toBe('text')
  })

  test('plain text gets autocorrect, capitals and spellcheck, and keeps its text for corrections', () => {
    expect(keyboardFor({ type: 'textarea', enterKeyHint: 'send' })).toEqual({
      inputmode: null,
      autocomplete: 'off',
      autocorrect: 'on',
      autocapitalize: 'sentences',
      spellcheck: true,
      enterkeyhint: 'send',
      keepText: true,
    })
  })

  test('a search box gets the search key and no capitals, unless the page asks', () => {
    expect(keyboardFor({ type: 'search' })).toMatchObject({
      autocapitalize: 'off',
      enterkeyhint: 'search',
      autocorrect: 'on',
    })
    expect(keyboardFor({ type: 'search', autocapitalize: 'words' }).autocapitalize).toBe('words')
  })

  test('a page that turns autocorrect or spellcheck off is followed', () => {
    expect(keyboardFor({ type: 'text', autocorrect: 'off' })).toMatchObject({ autocorrect: 'off', keepText: false })
    expect(keyboardFor({ type: 'text', spellcheck: 'false' }).spellcheck).toBe(false)
  })

  test('exact fields turn autocorrect, capitals and spellcheck off', () => {
    for (const field of [
      { type: 'password' },
      { type: 'text', autocomplete: 'username' },
      { type: 'text', autocomplete: 'one-time-code' },
      { type: 'email' },
      { type: 'url' },
      { type: 'tel' },
      { type: 'number' },
    ]) {
      expect(keyboardFor(field)).toMatchObject({
        autocorrect: 'off',
        autocapitalize: 'off',
        spellcheck: false,
        keepText: false,
      })
    }
  })

  test('chooses the keyboard layout', () => {
    expect(keyboardFor({ type: 'email' }).inputmode).toBe('email')
    expect(keyboardFor({ type: 'tel' }).inputmode).toBe('tel')
    expect(keyboardFor({ type: 'url' }).inputmode).toBe('url')
    expect(keyboardFor({ type: 'number' }).inputmode).toBe('decimal')
    expect(keyboardFor({ type: 'text', inputMode: 'numeric' }).inputmode).toBe('numeric')
    expect(keyboardFor({ type: 'tel', code: true }).inputmode).toBe('numeric')
    // an alphanumeric code keeps the text keyboard
    expect(keyboardFor({ type: 'text', code: true }).inputmode).toBe(null)
    expect(keyboardFor({ type: 'text', inputMode: 'none' }).inputmode).toBe(null)
  })

  test('offers codes from Messages for code fields, and never credentials', () => {
    expect(keyboardFor({ type: 'text', autocomplete: 'one-time-code' }).autocomplete).toBe('one-time-code')
    expect(keyboardFor({ type: 'text', code: true }).autocomplete).toBe('one-time-code')
    expect(keyboardFor({ type: 'password', autocomplete: 'current-password' }).autocomplete).toBe('off')
    expect(keyboardFor({ type: 'text', autocomplete: 'username' }).autocomplete).toBe('off')
    expect(keyboardFor({ type: 'text', autocomplete: 'cc-number' }).autocomplete).toBe('off')
    // the phone owner's own details
    expect(keyboardFor({ type: 'email' }).autocomplete).toBe('email')
    expect(keyboardFor({ type: 'text', autocomplete: 'shipping postal-code' }).autocomplete).toBe('postal-code')
  })

  test('without a field the keyboard is the default one', () => {
    expect(keyboardFor(null)).toBe(DEFAULT_KEYBOARD)
  })
})

describe('applying a keyboard', () => {
  const element = () => {
    const attrs = new Map<string, string>()
    return {
      attrs,
      getAttribute: (name: string) => (attrs.has(name) ? attrs.get(name)! : null),
      setAttribute: (name: string, value: string) => attrs.set(name, value),
      removeAttribute: (name: string) => attrs.delete(name),
    }
  }

  test('reports whether more than the layout changed', () => {
    const el = element()
    expect(applyKeyboard(el as any, keyboardFor({ type: 'email' }))).toBe(true)
    expect(el.attrs.get('inputmode')).toBe('email')
    expect(el.attrs.get('autocorrect')).toBe('off')
    expect(el.attrs.has('enterkeyhint')).toBe(false)
    // tel differs only in its layout and AutoFill
    expect(applyKeyboard(el as any, keyboardFor({ type: 'url' }))).toBe(true)
    expect(applyKeyboard(el as any, keyboardFor({ type: 'url', inputMode: 'url' }))).toBe(false)
    expect(applyKeyboard(el as any, keyboardFor({ type: 'number' }))).toBe(false)
    expect(el.attrs.get('inputmode')).toBe('decimal')
    expect(applyKeyboard(el as any, keyboardFor({ type: 'search' }))).toBe(true)
    expect(el.attrs.get('enterkeyhint')).toBe('search')
    expect(el.attrs.has('inputmode')).toBe(false)
  })
})
