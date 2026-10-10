// Helpers for turning text from mobile soft keyboards into X keysyms.
// Android keyboards report keyCode 229 for most keys, which the Guacamole
// keyboard ignores, so their text only arrives through input events.

export const XK_SHIFT_L = 0xffe1
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

// Minimal edit that turns prev into next: delete from the end, then insert.
export function textDiff(prev: string, next: string) {
  const a = Array.from(prev)
  const b = Array.from(next)
  let common = 0
  while (common < a.length && common < b.length && a[common] === b[common]) common++
  return { deletes: a.length - common, insert: b.slice(common).join('') }
}
