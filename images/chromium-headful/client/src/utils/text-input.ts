// Helpers for turning text from mobile soft keyboards into X keysyms.
// Android keyboards report keyCode 229 for most keys, which the Guacamole
// keyboard ignores, so their text only arrives through input events.

export const XK_SHIFT_L = 0xffe1
export const XK_SHIFT_R = 0xffe2
export const XK_CAPS_LOCK = 0xffe5
export const XK_CONTROL_L = 0xffe3
export const XK_BACKSPACE = 0xff08
export const XK_RETURN = 0xff0d

export function charToKeysym(ch: string): number {
  if (ch === '\n' || ch === '\r') return XK_RETURN
  const cp = ch.codePointAt(0) as number
  // Latin-1 keysyms equal their code points; everything else uses the Unicode range
  if ((cp >= 0x20 && cp <= 0x7e) || (cp >= 0xa0 && cp <= 0xff)) return cp
  return 0x01000000 | cp
}

// Neko maps a keysym to a keycode using the current modifier state. An upper
// case letter sent without Shift lands on the lower case key, so soft keyboards
// that never report Shift type the wrong case.
export function needsShift(ch: string) {
  return ch !== ch.toLowerCase() && ch === ch.toUpperCase()
}

export function keysymNeedsShift(keysym: number) {
  if (keysym >= 0x41 && keysym <= 0x5a) return true
  if (keysym >= 0xc0 && keysym <= 0xde && keysym !== 0xd7) return true
  if ((keysym & 0xff000000) === 0x01000000) return needsShift(String.fromCodePoint(keysym & 0x00ffffff))
  return false
}

// iOS (every browser there is WebKit) raises the soft keyboard only for focus()
// inside a user gesture handler. Other mobile browsers also raise it for a
// later focus() once the page has had a user gesture.
export function focusNeedsGesture(nav: { userAgent: string; platform: string; maxTouchPoints: number }) {
  return /iPad|iPhone|iPod/.test(nav.userAgent) || (nav.platform === 'MacIntel' && nav.maxTouchPoints > 1)
}

// Minimal edit that turns prev into next: delete from the end, then insert.
export function textDiff(prev: string, next: string) {
  const a = Array.from(prev)
  const b = Array.from(next)
  let common = 0
  while (common < a.length && common < b.length && a[common] === b[common]) common++
  return { deletes: a.length - common, insert: b.slice(common).join('') }
}

function isModifier(keysym: number) {
  return (keysym >= 0xffe1 && keysym <= 0xffee) || keysym === 0xfe03
}

type KeySink = (event: 'keydown' | 'keyup', keysym: number) => void

// The one path from keyboard input to X key events: key events from the
// Guacamole keyboard, and text that arrives only as input or composition
// events. On touch devices each letter's case comes from its keysym. Soft
// keyboards report their Shift and Caps Lock as separate key events (iOS sends
// them as synthetic modifier events, and the Guacamole keyboard releases every
// key on iOS right after pressing it), so Caps Lock is never sent, the remote's
// Caps Lock is turned off when the keyboard opens, and Shift is sent around a
// capital unless the remote already holds Shift.
export class TextInput {
  composing = false
  private composed = ''
  // a key event already sent the text the next input event reports
  private keyed = false
  // compositionend already sent this text, and some keyboards repeat it in an input event
  private compositionEcho: string | null = null
  // Shift keys held on the remote, from the keys sent
  private shiftDown = new Set<number>()
  private shiftWrapped = new Set<number>()
  // a Ctrl tapped on the extra keys row holds for the next key only
  private ctrlLatched = false
  private ctrlWrapped: number | null = null
  onCtrlLatch: (latched: boolean) => void = () => {}

  constructor(private send: KeySink, private touch: () => boolean) {}

  // a key from the Guacamole keyboard; sent is the keysym after layout mapping
  keyDown(keysym: number, sent = keysym) {
    this.down(keysym, sent)
    // modifiers type nothing, and iOS sends its soft Shift as one
    if (!isModifier(keysym)) this.keyed = true
  }

  keyUp(keysym: number, sent = keysym) {
    this.up(keysym, sent)
  }

  // A native keydown or keyup on the overlay. The Guacamole keyboard can
  // release a key before its input event arrives (it does on iOS), so only the
  // native keyup ends the key's claim on that event.
  keyEvent(keyup: boolean) {
    if (keyup) this.keyed = false
    this.compositionEcho = null
  }

  input(inputType: string, data: string | null) {
    const keyed = this.keyed
    const echo = this.compositionEcho !== null && data === this.compositionEcho
    this.keyed = false
    this.compositionEcho = null

    if (inputType === 'insertCompositionText') {
      this.updateComposition(data || '')
      return
    }
    if (keyed || echo) return

    switch (inputType) {
      case 'insertText':
      case 'insertReplacementText':
        this.type(data || '')
        break
      case 'insertLineBreak':
      case 'insertParagraph':
        this.press(XK_RETURN)
        break
      case 'deleteContentBackward':
        this.press(XK_BACKSPACE)
        break
    }
  }

  latchCtrl(latched: boolean) {
    this.ctrlLatched = latched
    this.onCtrlLatch(latched)
  }

  // a key from the extra keys row
  key(keysym: number) {
    this.press(keysym)
  }

  // pasted text, without the line break copied text often ends with
  paste(text: string) {
    this.type(text.replace(/[\r\n]+$/, ''))
  }

  compositionStart() {
    this.composing = true
    this.composed = ''
    this.compositionEcho = null
  }

  // send: false when input is not enabled, so the text is dropped
  compositionEnd(data: string, send: boolean) {
    if (send) this.updateComposition(data)
    this.compositionEcho = data
    this.composing = false
    this.composed = ''
  }

  private updateComposition(text: string) {
    const { deletes, insert } = textDiff(this.composed, text)
    for (let i = 0; i < deletes; i++) this.press(XK_BACKSPACE)
    this.type(insert)
    this.composed = text
  }

  // types text as given, line breaks included
  type(text: string) {
    for (const ch of Array.from(text)) this.press(charToKeysym(ch))
  }

  private press(keysym: number) {
    this.down(keysym, keysym)
    this.up(keysym, keysym)
  }

  private down(keysym: number, sent: number) {
    if (this.touch() && keysym === XK_CAPS_LOCK) return
    if (this.ctrlLatched && !isModifier(keysym) && this.ctrlWrapped === null) {
      this.ctrlWrapped = keysym
      this.send('keydown', XK_CONTROL_L)
    }
    if (this.needsShift(keysym)) {
      this.shiftWrapped.add(keysym)
      this.send('keydown', XK_SHIFT_L)
    }
    this.send('keydown', sent)
    if (keysym === XK_SHIFT_L || keysym === XK_SHIFT_R) this.shiftDown.add(keysym)
  }

  private up(keysym: number, sent: number) {
    if (this.touch() && keysym === XK_CAPS_LOCK) return
    this.send('keyup', sent)
    this.shiftDown.delete(keysym)
    if (this.shiftWrapped.delete(keysym)) this.send('keyup', XK_SHIFT_L)
    if (this.ctrlWrapped === keysym) {
      this.ctrlWrapped = null
      this.send('keyup', XK_CONTROL_L)
      this.latchCtrl(false)
    }
  }

  private needsShift(keysym: number) {
    return this.touch() && this.shiftDown.size === 0 && keysymNeedsShift(keysym)
  }
}
