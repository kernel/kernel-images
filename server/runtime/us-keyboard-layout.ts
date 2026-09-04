export interface KeyDefinition {
  key: string;
  code: string;
  keyCode: number;
  text?: string;
  shiftKey?: string;
  shiftKeyCode?: number;
  shiftText?: string;
  location?: number;
}

export interface ResolvedKeyDefinition {
  key: string;
  code: string;
  keyCode: number;
  text: string;
  unmodifiedText: string;
  location: number;
}

const definitions = new Map<string, KeyDefinition>();

function define(name: string, definition: KeyDefinition): void {
  definitions.set(name, definition);
}

const shiftedDigits = ')!@#$%^&*(';
for (let i = 0; i <= 9; i++) {
  const key = String(i);
  const definition: KeyDefinition = {
    key,
    code: `Digit${i}`,
    keyCode: 48 + i,
    shiftKey: shiftedDigits[i],
  };
  define(key, definition);
  define(`Digit${i}`, definition);
}

for (let i = 0; i < 26; i++) {
  const lower = String.fromCharCode(97 + i);
  const upper = String.fromCharCode(65 + i);
  const definition: KeyDefinition = {
    key: lower,
    code: `Key${upper}`,
    keyCode: 65 + i,
    shiftKey: upper,
  };
  define(lower, definition);
  define(upper, { ...definition, key: upper });
  define(`Key${upper}`, definition);
}

for (const [name, keyCode, code, key, shiftKey] of [
  ['Semicolon', 186, 'Semicolon', ';', ':'],
  ['Equal', 187, 'Equal', '=', '+'],
  ['Comma', 188, 'Comma', ',', '<'],
  ['Minus', 189, 'Minus', '-', '_'],
  ['Period', 190, 'Period', '.', '>'],
  ['Slash', 191, 'Slash', '/', '?'],
  ['Backquote', 192, 'Backquote', '`', '~'],
  ['BracketLeft', 219, 'BracketLeft', '[', '{'],
  ['Backslash', 220, 'Backslash', '\\', '|'],
  ['BracketRight', 221, 'BracketRight', ']', '}'],
  ['Quote', 222, 'Quote', "'", '"'],
] as const) {
  const definition: KeyDefinition = { keyCode, code, key, shiftKey };
  define(name, definition);
  define(key, definition);
  define(shiftKey, { ...definition, key: shiftKey });
}

for (const [name, keyCode, code, key, location = 0, text] of [
  ['Abort', 3, 'Abort', 'Cancel'],
  ['Help', 6, 'Help', 'Help'],
  ['Backspace', 8, 'Backspace', 'Backspace'],
  ['Tab', 9, 'Tab', 'Tab'],
  ['Enter', 13, 'Enter', 'Enter', 0, '\r'],
  ['ShiftLeft', 16, 'ShiftLeft', 'Shift', 1],
  ['ShiftRight', 16, 'ShiftRight', 'Shift', 2],
  ['ControlLeft', 17, 'ControlLeft', 'Control', 1],
  ['ControlRight', 17, 'ControlRight', 'Control', 2],
  ['AltLeft', 18, 'AltLeft', 'Alt', 1],
  ['AltRight', 18, 'AltRight', 'Alt', 2],
  ['Pause', 19, 'Pause', 'Pause'],
  ['CapsLock', 20, 'CapsLock', 'CapsLock'],
  ['Escape', 27, 'Escape', 'Escape'],
  ['Convert', 28, 'Convert', 'Convert'],
  ['NonConvert', 29, 'NonConvert', 'NonConvert'],
  ['Space', 32, 'Space', ' ', 0, ' '],
  ['PageUp', 33, 'PageUp', 'PageUp'],
  ['PageDown', 34, 'PageDown', 'PageDown'],
  ['End', 35, 'End', 'End'],
  ['Home', 36, 'Home', 'Home'],
  ['ArrowLeft', 37, 'ArrowLeft', 'ArrowLeft'],
  ['ArrowUp', 38, 'ArrowUp', 'ArrowUp'],
  ['ArrowRight', 39, 'ArrowRight', 'ArrowRight'],
  ['ArrowDown', 40, 'ArrowDown', 'ArrowDown'],
  ['Select', 41, 'Select', 'Select'],
  ['Open', 43, 'Open', 'Execute'],
  ['PrintScreen', 44, 'PrintScreen', 'PrintScreen'],
  ['Insert', 45, 'Insert', 'Insert'],
  ['Delete', 46, 'Delete', 'Delete'],
  ['MetaLeft', 91, 'MetaLeft', 'Meta', 1],
  ['MetaRight', 92, 'MetaRight', 'Meta', 2],
  ['ContextMenu', 93, 'ContextMenu', 'ContextMenu'],
  ['NumLock', 144, 'NumLock', 'NumLock'],
  ['ScrollLock', 145, 'ScrollLock', 'ScrollLock'],
  ['AudioVolumeMute', 173, 'AudioVolumeMute', 'AudioVolumeMute'],
  ['AudioVolumeDown', 174, 'AudioVolumeDown', 'AudioVolumeDown'],
  ['AudioVolumeUp', 175, 'AudioVolumeUp', 'AudioVolumeUp'],
  ['MediaTrackNext', 176, 'MediaTrackNext', 'MediaTrackNext'],
  ['MediaTrackPrevious', 177, 'MediaTrackPrevious', 'MediaTrackPrevious'],
  ['MediaStop', 178, 'MediaStop', 'MediaStop'],
  ['MediaPlayPause', 179, 'MediaPlayPause', 'MediaPlayPause'],
  ['AltGraph', 225, 'AltGraph', 'AltGraph'],
] as const) {
  define(name, { keyCode, code, key, location, text });
}

define('\r', definitions.get('Enter')!);
define('\n', definitions.get('Enter')!);
define(' ', definitions.get('Space')!);
define('Shift', { keyCode: 16, code: 'ShiftLeft', key: 'Shift', location: 1 });
define('Control', { keyCode: 17, code: 'ControlLeft', key: 'Control', location: 1 });
define('Alt', { keyCode: 18, code: 'AltLeft', key: 'Alt', location: 1 });
define('Meta', { keyCode: 91, code: 'MetaLeft', key: 'Meta', location: 1 });

for (let i = 1; i <= 24; i++) {
  define(`F${i}`, { keyCode: 111 + i, code: `F${i}`, key: `F${i}` });
}

for (const [name, keyCode, key, shiftKey, shiftKeyCode] of [
  ['Numpad0', 45, 'Insert', '0', 96],
  ['Numpad1', 35, 'End', '1', 97],
  ['Numpad2', 40, 'ArrowDown', '2', 98],
  ['Numpad3', 34, 'PageDown', '3', 99],
  ['Numpad4', 37, 'ArrowLeft', '4', 100],
  ['Numpad5', 12, 'Clear', '5', 101],
  ['Numpad6', 39, 'ArrowRight', '6', 102],
  ['Numpad7', 36, 'Home', '7', 103],
  ['Numpad8', 38, 'ArrowUp', '8', 104],
  ['Numpad9', 33, 'PageUp', '9', 105],
] as const) {
  define(name, { keyCode, code: name, key, shiftKey, shiftKeyCode, location: 3 });
}

define('NumpadEnter', { keyCode: 13, code: 'NumpadEnter', key: 'Enter', text: '\r', location: 3 });
define('NumpadMultiply', { keyCode: 106, code: 'NumpadMultiply', key: '*', location: 3 });
define('NumpadAdd', { keyCode: 107, code: 'NumpadAdd', key: '+', location: 3 });
define('NumpadSubtract', { keyCode: 109, code: 'NumpadSubtract', key: '-', location: 3 });
define('NumpadDecimal', { keyCode: 46, code: 'NumpadDecimal', key: '\0', shiftKey: '.', shiftKeyCode: 110, location: 3 });
define('NumpadDivide', { keyCode: 111, code: 'NumpadDivide', key: '/', location: 3 });
define('NumpadEqual', { keyCode: 187, code: 'NumpadEqual', key: '=', location: 3 });

const aliases: Record<string, string> = {
  esc: 'Escape',
  return: 'Enter',
  spacebar: 'Space',
  del: 'Delete',
  cmd: 'Meta',
  command: 'Meta',
  ctrl: 'Control',
};

const namedKeys = new Map<string, string>();
for (const name of definitions.keys()) {
  if ([...name].length > 1) namedKeys.set(name.toLowerCase(), name);
}

function normalizeKeyName(input: string): string {
  if ([...input].length === 1) return input;
  const lower = input.toLowerCase();
  return aliases[lower] ?? namedKeys.get(lower) ?? input;
}

export function resolveUSKey(input: string, shift: boolean): ResolvedKeyDefinition {
  const normalized = normalizeKeyName(input);
  let definition = definitions.get(normalized);
  if (!definition && [...input].length === 1) {
    definition = { key: input, code: '', keyCode: 0, text: input };
  }
  if (!definition) {
    throw new Error(`unknown key: ${input}`);
  }

  const key = shift && definition.shiftKey !== undefined ? definition.shiftKey : definition.key;
  const keyCode = shift && definition.shiftKeyCode !== undefined
    ? definition.shiftKeyCode
    : definition.keyCode;
  const unmodifiedText = definition.text ?? (definition.key.length === 1 ? definition.key : '');
  const text = shift && definition.shiftText !== undefined
    ? definition.shiftText
    : definition.text ?? (key.length === 1 ? key : '');

  return {
    key,
    code: definition.code,
    keyCode,
    text,
    unmodifiedText,
    location: definition.location ?? 0,
  };
}

export function supportedUSKeyNames(): string[] {
  return [...definitions.keys()].filter((name) => [...name].length > 1).sort();
}
