// Keeps the text typed into a remote field in the overlay, so the soft
// keyboard can autocorrect it. iOS autocorrect, predictive text and dictation
// change text already typed (insertReplacementText, or a whole word at once)
// instead of sending keys, and Android keyboards compose a word and rewrite
// it. Each change to the overlay is diffed against what was sent and goes to
// the remote as backspaces and text, so the text before the remote caret
// keeps matching the overlay. AutoFill replaces the field's text the same
// way, so text it fills in more than one edit is sent once.

import { XK_BACKSPACE, textDiff } from './text-input'

// past this the overlay starts over, at the cost of the corrections reaching back
const MAX_TEXT = 1000

export function isModifier(keysym: number) {
  return (keysym >= 0xffe1 && keysym <= 0xffee) || keysym === 0xfe03
}

// keys whose effect the overlay shows: printable characters and Backspace
export function typesIntoOverlay(keysym: number) {
  return (
    keysym === XK_BACKSPACE ||
    (keysym >= 0x20 && keysym <= 0x7e) ||
    (keysym >= 0xa0 && keysym <= 0xff) ||
    (keysym & 0xff000000) === 0x01000000
  )
}

export interface TypedTextOutputs {
  type(text: string): void
  backspace(): void
}

export class TypedText {
  // the overlay's text after the sentinel, as sent to the remote
  private text = ''
  // sending an edit's keys
  editing = false

  constructor(private sentinel: string, private out: TypedTextOutputs) {}

  // the overlay was reset to the sentinel, or its text was dropped
  reset() {
    this.text = ''
  }

  // The overlay's value after an edit. Returns false when the overlay should
  // be reset: its sentinel was deleted, which stands for text typed before
  // the overlay last started over, or it holds too much. AutoFill can replace
  // the whole value, sentinel included, which keeps the text before the
  // overlay, so the caller puts the sentinel back instead.
  edit(value: string) {
    const kept = value.startsWith(this.sentinel)
    const replaced = !kept && value !== ''
    const next = kept ? value.slice(this.sentinel.length) : value
    const { deletes, insert } = textDiff(this.text, next)
    this.editing = true
    // the sentinel stands for one character before the overlay's text
    for (let i = 0; i < deletes + (kept || replaced ? 0 : 1); i++) this.out.backspace()
    if (insert) this.out.type(insert)
    this.editing = false
    this.text = next
    return (kept || replaced) && next.length <= MAX_TEXT
  }
}
