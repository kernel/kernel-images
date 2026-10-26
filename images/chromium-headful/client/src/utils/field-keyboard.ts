// Chooses the soft keyboard for a remote field by copying what matters onto
// the overlay: the keyboard layout (inputmode, which also works on a textarea,
// where type="email" or "number" would not), one-time-code AutoFill, and
// autocorrect for plain text only. iOS reads everything but inputmode when the
// overlay gains focus, so a change to an open keyboard only switches layouts.
// Fields with AutoFill keep their text in the overlay too: iOS fills a field
// by replacing its text, which can take more than one edit.

import { RemoteField } from './remote-focus'

export type FieldKind = 'text' | 'password' | 'code' | 'email' | 'username' | 'url' | 'tel' | 'number'

export interface KeyboardAttributes {
  // null: the default text keyboard
  inputmode: string | null
  autocomplete: string
  autocorrect: 'on' | 'off'
  autocapitalize: string
  spellcheck: boolean
  enterkeyhint: string | null
  // the overlay keeps the typed text, which autocorrect and AutoFill replace
  keepText: boolean
}

const INPUT_MODES = new Set(['text', 'decimal', 'numeric', 'tel', 'search', 'email', 'url'])
const ENTER_KEY_HINTS = new Set(['enter', 'done', 'go', 'next', 'previous', 'search', 'send'])
const AUTOCAPITALIZE = new Set(['off', 'none', 'on', 'sentences', 'words', 'characters'])
// AutoFill that suggests the phone owner's own contact details, never credentials
const CONTACT_AUTOCOMPLETE = new Set([
  'name',
  'honorific-prefix',
  'given-name',
  'additional-name',
  'family-name',
  'honorific-suffix',
  'nickname',
  'organization',
  'street-address',
  'address-line1',
  'address-line2',
  'address-line3',
  'address-level1',
  'address-level2',
  'address-level3',
  'address-level4',
  'country',
  'country-name',
  'postal-code',
])

// For a keyboard opened without a known field: the button, or the cursor-shape
// guess. Codes from Messages are offered anywhere.
export const DEFAULT_KEYBOARD: KeyboardAttributes = {
  inputmode: null,
  autocomplete: 'one-time-code',
  autocorrect: 'off',
  autocapitalize: 'off',
  spellcheck: false,
  enterkeyhint: null,
  keepText: false,
}

function tokens(field: RemoteField) {
  return (field.autocomplete || '').toLowerCase().split(/\s+/).filter(Boolean)
}

export function fieldKind(field: RemoteField): FieldKind {
  const autocomplete = tokens(field)
  const inputMode = field.inputMode || ''
  if (field.type === 'password' || autocomplete.includes('current-password') || autocomplete.includes('new-password'))
    return 'password'
  if (field.code || autocomplete.includes('one-time-code')) return 'code'
  if (field.type === 'email' || inputMode === 'email' || autocomplete.includes('email')) return 'email'
  if (autocomplete.includes('username')) return 'username'
  if (field.type === 'url' || inputMode === 'url' || autocomplete.includes('url')) return 'url'
  if (field.type === 'tel' || inputMode === 'tel' || autocomplete.some((t) => t.startsWith('tel'))) return 'tel'
  if (field.type === 'number' || inputMode === 'numeric' || inputMode === 'decimal') return 'number'
  return 'text'
}

export function keyboardFor(field: RemoteField | null): KeyboardAttributes {
  if (!field) return DEFAULT_KEYBOARD
  const kind = fieldKind(field)
  const pageMode = INPUT_MODES.has(field.inputMode || '') ? (field.inputMode as string) : null
  const enterkeyhint = ENTER_KEY_HINTS.has(field.enterKeyHint || '')
    ? (field.enterKeyHint as string)
    : field.type === 'search'
    ? 'search'
    : null
  const exact = {
    autocorrect: 'off' as const,
    autocapitalize: 'off',
    spellcheck: false,
    enterkeyhint,
    keepText: false,
  }

  switch (kind) {
    case 'password':
      return { ...exact, inputmode: null, autocomplete: 'off' }
    case 'code':
      return {
        ...exact,
        inputmode: pageMode || (field.type === 'number' || field.type === 'tel' ? 'numeric' : null),
        autocomplete: 'one-time-code',
        keepText: true,
      }
    case 'email':
      return { ...exact, inputmode: 'email', autocomplete: 'email', keepText: true }
    case 'username':
      return { ...exact, inputmode: pageMode, autocomplete: 'off' }
    case 'url':
      return { ...exact, inputmode: 'url', autocomplete: 'off' }
    case 'tel':
      return { ...exact, inputmode: 'tel', autocomplete: 'tel', keepText: true }
    case 'number':
      return { ...exact, inputmode: pageMode || 'decimal', autocomplete: 'off' }
  }

  const contact = tokens(field).find((t) => CONTACT_AUTOCOMPLETE.has(t))
  const autocorrect = field.autocorrect === 'off' ? 'off' : 'on'
  const autocapitalize = AUTOCAPITALIZE.has(field.autocapitalize || '')
    ? (field.autocapitalize as string)
    : field.type === 'search'
    ? 'off'
    : 'sentences'
  return {
    inputmode: pageMode,
    autocomplete: contact || 'off',
    autocorrect,
    autocapitalize,
    spellcheck: field.spellcheck !== 'false',
    enterkeyhint,
    keepText: autocorrect === 'on' || !!contact,
  }
}

// Sets the attributes on the overlay. Returns whether anything but inputmode
// changed, which an open keyboard only picks up on a new focus.
export function applyKeyboard(el: HTMLElement, keyboard: KeyboardAttributes) {
  const attrs: [string, string | null][] = [
    ['autocomplete', keyboard.autocomplete],
    ['autocorrect', keyboard.autocorrect],
    ['autocapitalize', keyboard.autocapitalize],
    ['spellcheck', String(keyboard.spellcheck)],
    ['enterkeyhint', keyboard.enterkeyhint],
  ]
  let changed = false
  for (const [name, value] of attrs) {
    if (el.getAttribute(name) === value) continue
    changed = true
    if (value === null) el.removeAttribute(name)
    else el.setAttribute(name, value)
  }
  setInputMode(el, keyboard.inputmode)
  return changed
}

export function setInputMode(el: HTMLElement, mode: string | null) {
  if (mode === null) el.removeAttribute('inputmode')
  else el.setAttribute('inputmode', mode)
}
