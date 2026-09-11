import { createHmac } from 'node:crypto';
import type { Browser, ElementHandle, Frame, Page } from 'playwright-core';

export interface VaultFillRequest {
  bindings: { selector: string; value: string; type: 'text' | 'email' | 'password' | 'totp' }[];
  page_url?: string;
  timeout_ms?: number;
}

export interface VaultFillResult {
  status: 'filled' | 'failed' | 'partial' | 'unknown';
  fields: { index: number; status: 'filled' | 'failed' | 'not_attempted' | 'unknown' }[];
}

function decodeSeed(seed: string): Buffer {
  // RFC 4648 Base32, with optional canonical padding. Never accept partial bytes.
  if (!/^[A-Z2-7]+={0,6}$/i.test(seed)) throw new Error('invalid_seed');
  const text = seed.replace(/=+$/, '').toUpperCase();
  const remainder = text.length % 8;
  if (![0, 2, 4, 5, 7].includes(remainder) ||
      (seed.includes('=') && (seed.length % 8 !== 0 || seed.length - text.length !== (8 - remainder) % 8))) {
    throw new Error('invalid_seed');
  }
  const bytes: number[] = [];
  let bits = 0;
  let accumulator = 0;
  for (const char of text) {
    accumulator = (accumulator << 5) | 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'.indexOf(char);
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      bytes.push((accumulator >> bits) & 255);
      accumulator &= (1 << bits) - 1;
    }
  }
  if (accumulator !== 0 || bytes.length === 0) throw new Error('invalid_seed');
  return Buffer.from(bytes);
}

export function generateTOTP(seed: string, nowMs = Date.now()): string {
  const key = decodeSeed(seed);
  try {
    const counter = Buffer.alloc(8);
    counter.writeBigUInt64BE(BigInt(Math.floor(nowMs / 30000)));
    const digest = createHmac('sha1', key).update(counter).digest();
    const offset = digest[digest.length - 1] & 15;
    return ((digest.readUInt32BE(offset) & 0x7fffffff) % 1000000).toString().padStart(6, '0');
  } finally {
    key.fill(0);
  }
}

export function validVaultFillRequest(value: unknown): value is VaultFillRequest {
  if (!value || typeof value !== 'object') return false;
  const request = value as VaultFillRequest;
  return Array.isArray(request.bindings) && request.bindings.length > 0 && request.bindings.length <= 100 &&
    (request.page_url === undefined || (typeof request.page_url === 'string' && request.page_url.length <= 8192)) &&
    (request.timeout_ms === undefined || (Number.isInteger(request.timeout_ms) && request.timeout_ms >= 1 && request.timeout_ms <= 30000)) &&
    request.bindings.every(binding => binding && typeof binding.selector === 'string' &&
      binding.selector.length > 0 && binding.selector.length <= 4096 && typeof binding.value === 'string' &&
      binding.value.length <= 65536 && ['text', 'email', 'password', 'totp'].includes(binding.type));
}

export function vaultFillResult(request: VaultFillRequest): VaultFillResult {
  return { status: 'failed', fields: request.bindings.map((_, index) => ({ index, status: 'not_attempted' })) };
}

// Only status enums escape this function. Playwright errors can contain the value,
// selector, DOM and call log; none may cross the daemon protocol or reach logging.
export async function fillVaultFields(browser: Browser, request: VaultFillRequest, signal: AbortSignal): Promise<VaultFillResult> {
  const result = vaultFillResult(request);
  const handles: ElementHandle[] = [];
  const targets: ElementHandle[] = [];
  let page: Page | undefined;
  const changed = new AbortController();
  const stopped = AbortSignal.any([signal, changed.signal]);
  let current = 0;
  const invalidate = () => changed.abort();
  const guard = () => {
    stopped.throwIfAborted();
    if (page?.isClosed()) throw new Error('target_changed');
  };
  const bounded = async <T>(operation: () => Promise<T>): Promise<T> => {
    guard();
    let abort: () => void = () => {};
    try {
      return await Promise.race([
        operation(),
        new Promise<never>((_, reject) => {
          abort = () => reject(new Error('timeout'));
          stopped.addEventListener('abort', abort, { once: true });
          if (stopped.aborted) abort();
        }),
      ]);
    } finally {
      stopped.removeEventListener('abort', abort);
    }
  };
  const editable = async (handle: ElementHandle): Promise<boolean> =>
    await bounded(() => handle.evaluate(element => {
      if (!(element instanceof HTMLInputElement) && !(element instanceof HTMLTextAreaElement)) return false;
      return element.isConnected && element.ownerDocument === document && !element.matches(':disabled') && !element.readOnly &&
        (element instanceof HTMLTextAreaElement || ['text', 'email', 'password', 'search', 'tel', 'url', 'number'].includes(element.type));
    }));

  try {
    const pages = browser.contexts().flatMap(context => context.pages()).filter(candidate => !candidate.isClosed() &&
      (request.page_url === undefined || candidate.url() === request.page_url));
    if (pages.length !== 1) throw new Error('ambiguous_page');
    page = pages[0];
    page.on('framenavigated', invalidate);
    page.on('framedetached', invalidate);
    page.on('frameattached', invalidate);
    page.on('close', invalidate);
    const frames: Frame[] = page.frames();

    for (current = 0; current < request.bindings.length; current++) {
      guard();
      const binding = request.bindings[current];
      if (binding.type === 'totp') decodeSeed(binding.value).fill(0);
      const matches: ElementHandle[] = [];
      for (const frame of frames) {
        const found = await bounded(() => frame.$$(binding.selector));
        handles.push(...found);
        matches.push(...found);
      }
      // A selector itself must be unique, even if only one match is editable.
      if (matches.length !== 1) throw new Error('ambiguous_target');
      let target = matches[0];
      if (!(await editable(target))) {
        const descendants = await bounded(() => target.$$('input, textarea'));
        handles.push(...descendants);
        const candidates: ElementHandle[] = [];
        for (const descendant of descendants) {
          if (await editable(descendant)) candidates.push(descendant);
        }
        if (candidates.length !== 1) throw new Error('ambiguous_target');
        target = candidates[0];
      }
      if (!(await bounded(() => target.isVisible()))) throw new Error('invalid_target');
      for (const previous of targets) {
        if (await bounded(() => target.ownerFrame()) !== await bounded(() => previous.ownerFrame())) continue;
        if (await bounded(() => target.evaluate((element, other) => element === other, previous))) {
          throw new Error('duplicate_target');
        }
      }
      targets.push(target);
    }

    for (current = 0; current < targets.length; current++) {
      // Recheck pinned handles, never selectors. Navigation or removal cannot
      // cause a later field to be resolved against a replacement document/node.
      for (const target of targets) {
        if (!(await editable(target))) throw new Error('target_changed');
      }
      guard();
      const binding = request.bindings[current];
      const value = binding.type === 'totp' ? generateTOTP(binding.value) : binding.value;
      result.fields[current].status = 'unknown';
      const outcome = await bounded(() => targets[current].evaluate((element, value) => {
        if (!(element instanceof HTMLInputElement) && !(element instanceof HTMLTextAreaElement)) return 'failed';
        const rect = element.getBoundingClientRect();
        const visibility = getComputedStyle(element).visibility;
        if (!element.isConnected || element.ownerDocument !== document || element.matches(':disabled') || element.readOnly ||
            rect.width === 0 || rect.height === 0 || visibility === 'hidden' || visibility === 'collapse' ||
            (element instanceof HTMLInputElement && !['text', 'email', 'password', 'search', 'tel', 'url', 'number'].includes(element.type))) return 'failed';
        // ElementHandle.fill uses keyboard insertion after focusing, which can be
        // redirected by a site's focus handler. Write only this pinned node.
        const prototype = element instanceof HTMLInputElement ? HTMLInputElement.prototype : HTMLTextAreaElement.prototype;
        const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set;
        if (!setter) return 'failed';
        setter.call(element, value);
        if (element.value !== value) return 'unknown';
        element.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
        element.dispatchEvent(new Event('change', { bubbles: true }));
        return element.isConnected && element.ownerDocument === document ? 'filled' : 'unknown';
      }, value));
      guard();
      result.fields[current].status = outcome;
      if (outcome !== 'filled') throw new Error('write_stopped');
    }
    result.status = 'filled';
  } catch {
    const field = result.fields[Math.min(current, result.fields.length - 1)];
    if (field.status === 'not_attempted') field.status = 'failed';
    result.status = result.fields.some(field => field.status === 'unknown') ? 'unknown' :
      result.fields.some(field => field.status === 'filled') ? 'partial' : 'failed';
  } finally {
    page?.off('framenavigated', invalidate);
    page?.off('framedetached', invalidate);
    page?.off('frameattached', invalidate);
    page?.off('close', invalidate);
    // Disposal must not extend the request deadline if the transport is hung.
    for (const handle of handles) void handle.dispose().catch(() => {});
  }
  return result;
}
