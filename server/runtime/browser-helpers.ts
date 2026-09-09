
import { writeFileSync } from 'fs';
import sharp from 'sharp';
import { CdpClient, isInternalUrl, type CdpEvent } from './browser-cdp-client';
import {
  buildFunctionCallExpression,
  normalizeJsOptions,
  type JsOptions,
  type PageFunction,
} from './page-evaluation';
import { resolveUSKey, supportedUSKeyNames } from './us-keyboard-layout';

const DEFAULT_TIMEOUT_MS = 30_000;

// Bound wedged wheel commands well below the outer execution timeout.
const SCROLL_COMMAND_TIMEOUT_MS = 5_000;

// Leave time for helper errors to beat the destructive execution deadline.
const EXECUTION_DEADLINE_MARGIN_MS = 500;

type MouseButton = 'left' | 'right' | 'middle';
type ElementWaitState = 'attached' | 'detached' | 'visible' | 'hidden';

interface ClickPoint {
  x: number;
  y: number;
}

interface BackendNodeTarget {
  backendNodeId: number;
}

type ElementTarget = string | BackendNodeTarget;

export interface AccessibilityNode extends BackendNodeTarget {
  role: string;
  name: string;
  value?: string;
  checked?: boolean | 'mixed';
  pressed?: boolean | 'mixed';
  selected?: boolean;
  expanded?: boolean;
  disabled?: boolean;
}

export interface AccessibilitySnapshot {
  url: string;
  title: string;
  nodes: AccessibilityNode[];
}

interface ClickOptions {
  button?: MouseButton;
  clickCount?: number;
  timeoutSec?: number;
}

interface WaitForElementOptions {
  state?: ElementWaitState;
  timeoutSec?: number;
}

interface WaitForEventOptions {
  sessionId?: string | null;
  timeoutSec?: number;
  predicate?: (event: CdpEvent) => boolean;
}

interface FillInputOptions {
  clearFirst?: boolean;
  timeoutSec?: number;
}

function optionsObject(raw: unknown, helper: string): Record<string, unknown> {
  if (raw === undefined) return {};
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new Error(`${helper}: options must be an object`);
  }
  return raw as Record<string, unknown>;
}

function rejectUnknownOptions(
  options: Record<string, unknown>,
  allowed: readonly string[],
  helper: string,
): void {
  for (const key of Object.keys(options)) {
    if (!allowed.includes(key)) {
      throw new Error(`${helper}: unknown option: ${key}`);
    }
  }
}

function backendNodeId(target: unknown, helper: string): number | null {
  if (!target || typeof target !== 'object' || !Object.prototype.hasOwnProperty.call(target, 'backendNodeId')) {
    return null;
  }
  const id = (target as Record<string, unknown>).backendNodeId;
  if (!Number.isInteger(id) || (id as number) <= 0) {
    throw new Error(`${helper}: backendNodeId must be a positive integer`);
  }
  return id as number;
}

function accessibilityControlState(node: any): Partial<AccessibilityNode> {
  const state: Partial<AccessibilityNode> = {};
  for (const property of node.properties ?? []) {
    const name = property.name as 'checked' | 'pressed' | 'selected' | 'expanded' | 'disabled';
    const observed = property.value?.value;
    if (name === 'checked' || name === 'pressed') {
      if (observed === 'mixed') state[name] = 'mixed';
      else if (observed === true || observed === 'true') state[name] = true;
      else if (observed === false || observed === 'false') state[name] = false;
    } else if (
      (name === 'selected' || name === 'expanded' || name === 'disabled') &&
      typeof observed === 'boolean'
    ) {
      state[name] = observed;
    }
  }
  return state;
}

function nonNegativeSeconds(value: unknown, fallback: number, helper: string): number {
  if (value === undefined) return fallback;
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) {
    throw new Error(`${helper}: timeoutSec must be a non-negative finite number`);
  }
  return value;
}

const MODIFIER_SUGAR: Record<string, string> = {
  alt: 'Alt',
  ctrl: 'Control',
  control: 'Control',
  meta: 'Meta',
  shift: 'Shift',
};

function normalizeKeyModifiers(modifiers?: string[] | Record<string, boolean>): string[] {
  if (modifiers === undefined || modifiers === null) {
    return [];
  }
  if (Array.isArray(modifiers)) {
    return modifiers.map((name) => {
      const canonical = MODIFIER_SUGAR[String(name).toLowerCase()];
      if (canonical === undefined) {
        throw new Error(`pressKey: unknown modifier: ${name} (expected Alt, Control, Meta, or Shift)`);
      }
      return canonical;
    });
  }
  if (typeof modifiers === 'object') {
    const out: string[] = [];
    for (const [name, on] of Object.entries(modifiers)) {
      if (!on) continue;
      const canonical = MODIFIER_SUGAR[name.toLowerCase()];
      if (canonical === undefined) {
        throw new Error(`pressKey: unknown modifier: ${name} (expected Alt, Control, Meta, or Shift)`);
      }
      if (!out.includes(canonical)) {
        out.push(canonical);
      }
    }
    return out;
  }
  throw new Error(
    'pressKey: modifiers must be an array drawn from Alt, Control, Meta, Shift (or an object like {ctrl: true})',
  );
}

export class BrowserHelpers {
  private readonly client: CdpClient;

  executionDeadlineMs: number | null = null;
  onLog?: (message: string) => void;

  constructor(client: CdpClient) {
    this.client = client;
  }

  private waitDeadline(timeoutMs: number): { deadline: number; clamped: boolean } {
    const own = Date.now() + timeoutMs;
    const exec = this.executionDeadlineMs;
    if (exec !== null && exec - EXECUTION_DEADLINE_MARGIN_MS < own) {
      return { deadline: exec - EXECUTION_DEADLINE_MARGIN_MS, clamped: true };
    }
    return { deadline: own, clamped: false };
  }

  // Escape hatch + events

  cdp = async (method: string, params?: unknown, sessionId?: string | null): Promise<unknown> => {
    if (sessionId === null) {
      return this.client.browserCommand(method, params);
    }
    if (typeof sessionId === 'string') {
      return this.client.send(method, params, sessionId);
    }
    // Default: attached session for session-scoped domains. Target.* and
    // Browser.* style commands must be sent browser-level; callers should
    // pass null explicitly, but route obviously browser-scoped domains for
    // ergonomics.
    if (/^(Target|Browser|SystemInfo|Storage)\./.test(method)) {
      return this.client.browserCommand(method, params);
    }
    return this.client.sessionCommand(method, params);
  };

  drainEvents = async (): Promise<unknown[]> => {
    await this.client.ensureAttached();
    return this.client.drainEvents();
  };

  waitForEvent = (
    method: string,
    rawOptions?: WaitForEventOptions,
  ): Promise<CdpEvent | null> => {
    if (typeof method !== 'string' || method.trim() === '') {
      throw new Error('waitForEvent: method must be a non-empty string');
    }
    const options = optionsObject(rawOptions, 'waitForEvent');
    rejectUnknownOptions(options, ['sessionId', 'timeoutSec', 'predicate'], 'waitForEvent');
    const timeoutSec = nonNegativeSeconds(options.timeoutSec, 30, 'waitForEvent');
    const predicate = options.predicate;
    if (predicate !== undefined && typeof predicate !== 'function') {
      throw new Error('waitForEvent: predicate must be a function');
    }

    let sessionId: string | undefined;
    if (Object.prototype.hasOwnProperty.call(options, 'sessionId')) {
      if (
        options.sessionId !== null &&
        (typeof options.sessionId !== 'string' || options.sessionId === '')
      ) {
        throw new Error('waitForEvent: sessionId must be a non-empty string or null');
      }
      sessionId = options.sessionId === null ? undefined : options.sessionId as string;
    } else {
      if (!this.client.sessionId) {
        throw new Error(
          'waitForEvent: no target is attached; call ensureRealTab() or newTab() before arming a page event',
        );
      }
      sessionId = this.client.sessionId;
    }

    return this.client.waitForEvent(method, {
      sessionId,
      timeoutMs: timeoutSec * 1000,
      predicate: predicate as ((event: CdpEvent) => boolean) | undefined,
    });
  };

  // Navigation + page state

  gotoUrl = async (url: string): Promise<unknown> => {
    return this.client.sessionCommand('Page.navigate', { url });
  };

  accessibilitySnapshot = async (): Promise<AccessibilitySnapshot> => {
    await this.client.ensureAttached();
    const { nodes = [] } = await this.client.sessionCommand<any>('Accessibility.getFullAXTree');
    const info = await this.pageInfo();
    return {
      url: String(info.url ?? ''),
      title: String(info.title ?? ''),
      nodes: nodes
        .filter((node: any) => !node.ignored && node.backendDOMNodeId)
        .map((node: any) => ({
          backendNodeId: node.backendDOMNodeId,
          role: String(node.role?.value ?? ''),
          name: String(node.name?.value ?? '').replace(/\s+/g, ' ').trim(),
          ...(node.value ? { value: String(node.value.value) } : {}),
          ...accessibilityControlState(node),
        })),
    };
  };

  pageInfo = async (): Promise<Record<string, unknown>> => {
    // A pending modal JavaScript dialog freezes the renderer main thread, so
    // Runtime.evaluate would block until the CDP command timeout and the
    // dialog field would be unreachable exactly when it matters. Report the
    // dialog plus last-known target metadata (from the browser-level target
    // list, which does not block) instead of evaluating in the page.
    const pending = this.client.pendingDialog;
    if (pending) {
      const info: Record<string, unknown> = {
        dialog: { type: pending.type, message: pending.message },
      };
      try {
        const targets = await this.client.listTargets();
        const current = targets.find((t) => t.targetId === this.client.targetId);
        if (current) {
          info.url = current.url;
          info.title = current.title;
        }
      } catch {
        // Best effort: the dialog itself is the critical payload.
      }
      return info;
    }
    const evalRes = await this.client.sessionCommand<any>('Runtime.evaluate', {
      expression: `(() => ({
        url: location.href,
        title: document.title,
        viewport: { width: window.innerWidth, height: window.innerHeight },
        scroll: { x: window.scrollX, y: window.scrollY },
        page: {
          width: document.documentElement ? document.documentElement.scrollWidth : 0,
          height: document.documentElement ? document.documentElement.scrollHeight : 0,
        },
        ready_state: document.readyState,
      }))()`,
      returnByValue: true,
    });
    const value = evalRes.result?.value ?? {};
    const dialog = this.client.pendingDialog;
    return {
      ...value,
      dialog: dialog ? { type: dialog.type, message: dialog.message } : null,
    };
  };

  // Input

  click = async (target: string | ClickPoint | BackendNodeTarget, rawOptions?: ClickOptions): Promise<void> => {
    const options = optionsObject(rawOptions, 'click');
    rejectUnknownOptions(options, ['button', 'clickCount', 'timeoutSec'], 'click');

    const button = options.button ?? 'left';
    if (button !== 'left' && button !== 'right' && button !== 'middle') {
      throw new Error('click: button must be left, right, or middle');
    }
    const clickCount = options.clickCount ?? 1;
    if (!Number.isInteger(clickCount) || (clickCount as number) <= 0) {
      throw new Error('click: clickCount must be a positive integer');
    }

    let point: ClickPoint;
    if (typeof target === 'string') {
      if (target.length === 0) throw new Error('click: selector must not be empty');
      point = await this.waitForClickablePoint(
        target,
        nonNegativeSeconds(options.timeoutSec, 10, 'click'),
      );
    } else {
      const nodeId = backendNodeId(target, 'click');
      if (nodeId !== null) {
        point = await this.waitForClickableBackendNode(
          nodeId,
          nonNegativeSeconds(options.timeoutSec, 10, 'click'),
        );
      } else {
        const coordinate = target as ClickPoint;
        if (
          typeof coordinate.x !== 'number' ||
          !Number.isFinite(coordinate.x) ||
          typeof coordinate.y !== 'number' ||
          !Number.isFinite(coordinate.y)
        ) {
          throw new Error('click: target must be a selector, accessibility node, or finite {x, y} coordinates');
        }
        if (options.timeoutSec !== undefined) {
          throw new Error('click: timeoutSec is only supported for selector targets or accessibility-node targets');
        }
        point = { x: coordinate.x, y: coordinate.y };
      }
    }

    await this.client.sessionCommand('Input.dispatchMouseEvent', {
      type: 'mouseMoved',
      x: point.x,
      y: point.y,
      button: 'none',
    });
    const buttons = button === 'left' ? 1 : button === 'right' ? 2 : 4;
    for (let count = 1; count <= (clickCount as number); count++) {
      await this.client.sessionCommand('Input.dispatchMouseEvent', {
        type: 'mousePressed',
        x: point.x,
        y: point.y,
        button,
        buttons,
        clickCount: count,
      });
      await this.client.sessionCommand('Input.dispatchMouseEvent', {
        type: 'mouseReleased',
        x: point.x,
        y: point.y,
        button,
        buttons: 0,
        clickCount: count,
      });
    }
  };

  typeText = async (text: string): Promise<void> => {
    await this.client.sessionCommand('Input.insertText', { text });
  };

  fillInput = async (
    target: ElementTarget,
    text: string,
    rawOptions?: FillInputOptions,
  ): Promise<void> => {
    if (typeof target === 'string' && target.length === 0) {
      throw new Error('fillInput: selector must be a non-empty string');
    }
    if (typeof target !== 'string' && backendNodeId(target, 'fillInput') === null) {
      throw new Error('fillInput: target must be a selector or accessibility node');
    }
    if (typeof text !== 'string') {
      throw new Error('fillInput: text must be a string');
    }
    const options = optionsObject(rawOptions, 'fillInput');
    rejectUnknownOptions(options, ['clearFirst', 'timeoutSec'], 'fillInput');
    const clearFirst = options.clearFirst ?? true;
    if (typeof clearFirst !== 'boolean') {
      throw new Error('fillInput: clearFirst must be a boolean');
    }
    const timeoutSec = nonNegativeSeconds(options.timeoutSec, 10, 'fillInput');
    if (typeof target === 'string') {
      await this.waitForFillTarget(target, timeoutSec);
    } else {
      await this.waitForFillBackendNode(backendNodeId(target, 'fillInput')!, timeoutSec);
    }

    if (clearFirst) {
      const modifiers = process.platform === 'darwin' ? 4 : 2;
      const selectAll = {
        key: 'a',
        code: 'KeyA',
        modifiers,
        windowsVirtualKeyCode: 65,
        nativeVirtualKeyCode: 65,
      };
      await this.client.sessionCommand('Input.dispatchKeyEvent', { type: 'rawKeyDown', ...selectAll });
      await this.client.sessionCommand('Input.dispatchKeyEvent', { type: 'keyUp', ...selectAll });
      await this.pressKey('Backspace');
    }
    for (const char of text) {
      await this.pressKey(char);
    }
    await this.evaluateInPage(`(() => {
      const el = document.activeElement;
      if (!el) return;
      el.dispatchEvent(new Event('input', { bubbles: true }));
      el.dispatchEvent(new Event('change', { bubbles: true }));
    })()`);
  };

  private static readonly MODIFIER_BITS: Record<string, number> = {
    Alt: 1,
    Control: 2,
    Meta: 4,
    Shift: 8,
  };

  pressKey = async (
    key: string,
    modifiers?: number | string[] | Record<string, boolean>,
  ): Promise<void> => {
    let modifierBits = 0;
    if (typeof modifiers === 'number') {
      if (!Number.isInteger(modifiers) || modifiers < 0 || modifiers > 15) {
        throw new Error('pressKey: numeric modifiers must be a bitfield from 0 to 15');
      }
      modifierBits = modifiers;
    } else {
      for (const m of normalizeKeyModifiers(modifiers)) {
        const bit = BrowserHelpers.MODIFIER_BITS[m];
        if (bit === undefined) {
          throw new Error(`pressKey: unknown modifier: ${m} (expected Alt, Control, Meta, or Shift)`);
        }
        modifierBits |= bit;
      }
    }

    let def;
    try {
      def = resolveUSKey(key, (modifierBits & BrowserHelpers.MODIFIER_BITS.Shift) !== 0);
    } catch {
      throw new Error(
        `unknown key: ${key} (use one Unicode character or a supported US-layout key such as ${
          supportedUSKeyNames().slice(0, 20).join(', ')
        })`,
      );
    }

    const base = {
      code: def.code,
      key: def.key,
      windowsVirtualKeyCode: def.keyCode,
      nativeVirtualKeyCode: def.keyCode,
      modifiers: modifierBits,
      location: def.location,
      isKeypad: def.location === 3,
      unmodifiedText: def.unmodifiedText,
    };
    const shortcut = (modifierBits & (1 | 2 | 4)) !== 0;
    const printable = [...def.key].length === 1 && !!def.text && !shortcut;
    await this.client.sessionCommand('Input.dispatchKeyEvent', {
      ...base,
      type: 'keyDown',
      ...(!printable && def.text && !shortcut ? { text: def.text } : {}),
    });
    if (printable) {
      await this.client.sessionCommand('Input.dispatchKeyEvent', {
        ...base,
        type: 'char',
        text: def.text,
      });
    }
    await this.client.sessionCommand('Input.dispatchKeyEvent', { ...base, type: 'keyUp' });
  };

  scroll = async (x: number, y: number, dy = -300, dx = 0): Promise<void> => {
    await this.client.sessionCommand(
      'Input.dispatchMouseEvent',
      {
        type: 'mouseWheel',
        x,
        y,
        deltaX: dx,
        deltaY: dy,
      },
      SCROLL_COMMAND_TIMEOUT_MS,
    );
  };

  // Screenshots

  captureScreenshot = async (
    path?: string,
    fullPage = false,
    maxDim?: number,
  ): Promise<string> => {
    const outPath = path ?? '/tmp/shot.png';
    const shot = await this.client.sessionCommand<any>('Page.captureScreenshot', {
      format: 'png',
      captureBeyondViewport: fullPage,
    });
    let bytes = Buffer.from(shot.data, 'base64');
    if (maxDim !== undefined) {
      if (!Number.isInteger(maxDim) || maxDim <= 0) {
        throw new Error('captureScreenshot: maxDim must be a positive integer');
      }
      bytes = await sharp(bytes)
        .resize({ width: maxDim, height: maxDim, fit: 'inside', withoutEnlargement: true })
        .png()
        .toBuffer();
    }
    writeFileSync(outPath, bytes);
    return outPath;
  };

  // Tabs

  listTabs = async (includeChrome = true): Promise<Record<string, unknown>[]> => {
    const targets = await this.client.listTargets();
    return targets
      .filter((t) => t.type === 'page')
      .filter((t) => includeChrome || !isInternalUrl(t.url))
      .map((t) => ({ targetId: t.targetId, title: t.title, url: t.url }));
  };

  currentTab = async (): Promise<Record<string, unknown>> => {
    await this.client.ensureAttached();
    const targets = await this.client.listTargets();
    const current = targets.find((t) => t.targetId === this.client.targetId);
    if (!current) {
      throw new Error('attached target no longer exists');
    }
    return { targetId: current.targetId, url: current.url, title: current.title };
  };

  private targetId(target: unknown): string {
    if (typeof target === 'string') return target;
    if (target && typeof target === 'object' && typeof (target as any).targetId === 'string') {
      return (target as any).targetId;
    }
    throw new Error('expected a targetId string or a tab object returned by currentTab/listTabs');
  }

  switchTab = async (target: unknown): Promise<string> => {
    return this.client.attach(this.targetId(target));
  };

  newTab = async (url = 'about:blank'): Promise<string> => {
    if (url !== 'about:blank') {
      try {
        const current = await this.currentTab();
        const currentUrl = String(current.url ?? '');
        if (
          currentUrl === '' ||
          currentUrl === 'about:blank' ||
          currentUrl.startsWith('about:blank#') ||
          /^(chrome:\/\/(newtab|new-tab-page)|edge:\/\/newtab|about:newtab)/.test(currentUrl)
        ) {
          await this.gotoUrl(url);
          await this.client.waitForNavigationCommit(current.targetId as string, 5_000);
          return current.targetId as string;
        }
      } catch {
        // No attached reusable tab; create one below.
      }
    }
    await this.client.ensureConnected();
    const created = await this.client.browserCommand<{ targetId: string }>('Target.createTarget', {
      url: 'about:blank',
    });
    await this.client.attach(created.targetId);
    if (url !== 'about:blank') {
      await this.gotoUrl(url);
      await this.client.waitForNavigationCommit(created.targetId, 5_000);
    }
    return created.targetId;
  };

  closeTab = async (target?: unknown): Promise<void> => {
    await this.client.ensureConnected();
    const id = target === undefined ? this.client.targetId : this.targetId(target);
    if (!id) {
      throw new Error('no tab is attached and no target id was provided');
    }
    await this.client.browserCommand('Target.closeTarget', { targetId: id });
    if (id === this.client.targetId) {
      this.client.sessionId = null;
      this.client.targetId = null;
    }
    // Target.closeTarget resolves before the target is fully destroyed;
    // wait (best effort) so an immediate listTabs call no longer counts the
    // closed tab.
    await this.client.waitForTargetGone(id, 5_000);
  };

  ensureRealTab = async (): Promise<Record<string, unknown> | null> => {
    const tabs = await this.listTabs(false);
    if (tabs.length === 0) return null;
    try {
      const current = await this.currentTab();
      if (!isInternalUrl(String(current.url ?? ''))) return current;
    } catch {
      // No usable attached target; attach the first real page below.
    }
    await this.switchTab(tabs[0]);
    return tabs[0];
  };

  iframeTarget = async (urlSubstring: string): Promise<Record<string, unknown> | null> => {
    const targets = await this.client.listTargets();
    const match = targets.find((t) => t.type === 'iframe' && t.url.includes(urlSubstring));
    if (!match) return null;
    return { targetId: match.targetId, url: match.url, title: match.title, type: match.type };
  };

  // Waiting

  waitMs = async (milliseconds = 1_000): Promise<void> => {
    await new Promise((resolve) => setTimeout(resolve, milliseconds));
  };

  waitForLoad = async (timeoutSec = 15): Promise<boolean> => {
    const { deadline } = this.waitDeadline(timeoutSec * 1000);
    while (Date.now() <= deadline) {
      const res = await this.client.sessionCommand<any>('Runtime.evaluate', {
        expression: 'document.readyState',
        returnByValue: true,
      });
      if (res.result?.value === 'complete') return true;
      await this.waitMs(300);
    }
    return false;
  };

  waitForElement = async (
    target: ElementTarget,
    rawOptions?: WaitForElementOptions,
  ): Promise<boolean> => {
    if (typeof target === 'string' && target.length === 0) {
      throw new Error('waitForElement: selector must be a non-empty string');
    }
    if (typeof target !== 'string' && backendNodeId(target, 'waitForElement') === null) {
      throw new Error('waitForElement: target must be a selector or accessibility node');
    }
    const options = optionsObject(rawOptions, 'waitForElement');
    rejectUnknownOptions(options, ['state', 'timeoutSec'], 'waitForElement');
    const state = options.state ?? 'visible';
    if (state !== 'attached' && state !== 'detached' && state !== 'visible' && state !== 'hidden') {
      throw new Error('waitForElement: state must be attached, detached, visible, or hidden');
    }
    const timeoutSec = nonNegativeSeconds(options.timeoutSec, 10, 'waitForElement');
    const { deadline } = this.waitDeadline(timeoutSec * 1000);
    for (;;) {
      const found = typeof target === 'string' ? await this.evaluateInPage(
        `(function elementState(selector, state) {
          const elements = [...document.querySelectorAll(selector)];
          const visible = (el) => {
            if (!el.isConnected || el.getClientRects().length === 0) return false;
            if (typeof el.checkVisibility === 'function') {
              return el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
            }
            const style = getComputedStyle(el);
            return style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
          };
          if (state === 'attached') return elements.length > 0;
          if (state === 'detached') return elements.length === 0;
          const visibleCount = elements.filter(visible).length;
          return state === 'visible' ? visibleCount > 0 : visibleCount === 0;
        })(${JSON.stringify(target)}, ${JSON.stringify(state)})`,
      ) : await this.backendNodeMatchesState(backendNodeId(target, 'waitForElement')!, state);
      if (found) return true;
      if (Date.now() >= deadline) return false;
      await this.waitMs(Math.min(100, Math.max(0, deadline - Date.now())));
    }
  };

  waitForNetworkIdle = async (idleSec = 0.5, timeoutSec = 30): Promise<boolean> => {
    await this.client.ensureAttached();
    const { deadline } = this.waitDeadline(timeoutSec * 1000);
    for (;;) {
      const { inFlight, lastActivity } = this.client.networkIdleState();
      const now = Date.now();
      if (inFlight === 0 && now - lastActivity >= idleSec * 1000) {
        return true;
      }
      if (now > deadline) {
        return false;
      }
      await this.waitMs(100);
    }
  };

  // JavaScript evaluation + uploads

  js = async (
    expressionOrFunction: string | PageFunction,
    rawOptions?: JsOptions,
  ): Promise<unknown> => {
    const options = normalizeJsOptions(rawOptions);
    let expression: string;
    if (typeof expressionOrFunction === 'string') {
      if (Object.prototype.hasOwnProperty.call(options, 'arg')) {
        throw new Error('js: arg is only supported when evaluating a page function');
      }
      expression = expressionOrFunction;
    } else if (typeof expressionOrFunction === 'function') {
      expression = buildFunctionCallExpression(expressionOrFunction, options.arg);
    } else {
      throw new Error('js: expected a JavaScript expression string or page function');
    }

    if (options.targetId) {
      return this.client.evaluateOnTarget(options.targetId, expression);
    }
    return this.evaluateInPage(expression);
  };

  uploadFile = async (target: ElementTarget, path: string | string[]): Promise<void> => {
    const paths = typeof path === 'string' ? [path] : path;
    if (!Array.isArray(paths) || paths.length === 0 || paths.some((item) => typeof item !== 'string')) {
      throw new Error('uploadFile requires a VM-local file path or a non-empty array of paths');
    }
    if (typeof target === 'string') {
      if (target.length === 0) throw new Error('uploadFile: selector must be a non-empty string');
      const doc = await this.client.sessionCommand<any>('DOM.getDocument', { depth: 0 });
      const queried = await this.client.sessionCommand<any>('DOM.querySelector', {
        nodeId: doc.root.nodeId,
        selector: target,
      });
      if (!queried.nodeId) {
        throw new Error(`no element matches selector: ${target}`);
      }
      await this.client.sessionCommand('DOM.setFileInputFiles', {
        nodeId: queried.nodeId,
        files: paths,
      });
      return;
    }
    await this.client.sessionCommand('DOM.setFileInputFiles', {
      backendNodeId: backendNodeId(target, 'uploadFile'),
      files: paths,
    });
  };

  // HTTP

  httpGet = async (
    url: string,
    headers?: Record<string, string>,
    timeoutSec = 20,
  ): Promise<string> => {
    if (typeof timeoutSec !== 'number' || !Number.isFinite(timeoutSec) || timeoutSec < 0) {
      throw new Error('httpGet: timeoutSec must be a non-negative finite number');
    }
    const { deadline } = this.waitDeadline(timeoutSec * 1000);
    const timeoutMs = Math.max(0, deadline - Date.now());
    if (timeoutMs === 0) {
      throw new Error(`GET ${url} timed out before it could start`);
    }
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
      const res = await fetch(url, {
        headers: { 'user-agent': 'Mozilla/5.0', 'accept-encoding': 'gzip', ...(headers ?? {}) },
        signal: controller.signal,
      });
      if (!res.ok) {
        throw new Error(`GET ${url} failed with status ${res.status}`);
      }
      return await res.text();
    } catch (err) {
      if (controller.signal.aborted) {
        throw new Error(`GET ${url} timed out after ${timeoutMs}ms`);
      }
      throw err;
    } finally {
      clearTimeout(timer);
    }
  };

  // Internals

  private backendNodeGone(error: unknown): boolean {
    return /could not find node|no node with given id|could not resolve backend node/i.test(
      String((error as any)?.message ?? error),
    );
  }

  private async callOnBackendNode(
    id: number,
    functionDeclaration: string,
    args: unknown[] = [],
  ): Promise<any> {
    const resolved = await this.client.sessionCommand<any>('DOM.resolveNode', { backendNodeId: id });
    const objectId = resolved.object?.objectId;
    if (!objectId) throw new Error(`could not resolve backend node ${id}`);
    try {
      const result = await this.client.sessionCommand<any>('Runtime.callFunctionOn', {
        objectId,
        functionDeclaration,
        arguments: args.map((value) => ({ value })),
        awaitPromise: true,
        returnByValue: true,
      });
      if (result.exceptionDetails || result.result?.subtype === 'error') {
        const description =
          result.result?.description ??
          result.exceptionDetails?.exception?.description ??
          result.exceptionDetails?.text ??
          `backend node ${id} evaluation failed`;
        throw new Error(description);
      }
      return result.result?.value;
    } finally {
      try {
        await this.client.sessionCommand('Runtime.releaseObject', { objectId });
      } catch {
        // The object is already gone when its target or connection closes.
      }
    }
  }

  private async waitForClickableBackendNode(id: number, timeoutSec: number): Promise<ClickPoint> {
    const { deadline } = this.waitDeadline(timeoutSec * 1000);
    let lastStatus = 'not found';
    for (;;) {
      try {
        const result = await this.callOnBackendNode(
          id,
          `async function resolveBackendClickTarget() {
            const el = this;
            const visible = (candidate) => {
              if (!(candidate instanceof Element) || !candidate.isConnected || candidate.getClientRects().length === 0) return false;
              if (typeof candidate.checkVisibility === 'function') {
                return candidate.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
              }
              const style = getComputedStyle(candidate);
              return style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
            };
            if (!visible(el)) return { status: 'not visible' };
            if (el.matches(':disabled') || el.getAttribute('aria-disabled') === 'true') return { status: 'disabled' };
            el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
            const before = el.getBoundingClientRect();
            await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
            if (!visible(el)) return { status: 'detached or hidden' };
            const after = el.getBoundingClientRect();
            const stable =
              Math.abs(before.x - after.x) < 0.25 &&
              Math.abs(before.y - after.y) < 0.25 &&
              Math.abs(before.width - after.width) < 0.25 &&
              Math.abs(before.height - after.height) < 0.25;
            if (!stable) return { status: 'moving' };
            const left = Math.max(0, after.left);
            const right = Math.min(innerWidth, after.right);
            const top = Math.max(0, after.top);
            const bottom = Math.min(innerHeight, after.bottom);
            if (right <= left || bottom <= top) return { status: 'outside viewport' };
            const x = left + (right - left) / 2;
            const y = top + (bottom - top) / 2;
            const hit = document.elementFromPoint(x, y);
            if (!hit || (hit !== el && !el.contains(hit))) {
              return { status: 'intercepted', hit: hit ? hit.tagName.toLowerCase() : null };
            }
            return { status: 'ready', x, y };
          }`,
        ) as { status?: string; x?: number; y?: number };
        if (result?.status === 'ready' && typeof result.x === 'number' && typeof result.y === 'number') {
          return { x: result.x, y: result.y };
        }
        lastStatus = result?.status ?? 'not actionable';
      } catch (error) {
        if (!this.backendNodeGone(error)) throw error;
        lastStatus = 'not found';
      }
      if (Date.now() >= deadline) {
        throw new Error(`click: backend node ${id} was not actionable within ${timeoutSec}s (${lastStatus})`);
      }
      await this.waitMs(Math.min(50, Math.max(0, deadline - Date.now())));
    }
  }

  private async waitForFillBackendNode(id: number, timeoutSec: number): Promise<void> {
    const { deadline } = this.waitDeadline(timeoutSec * 1000);
    let lastStatus = 'not found';
    for (;;) {
      try {
        const result = await this.callOnBackendNode(
          id,
          `function resolveBackendFillTarget() {
            const el = this;
            const visible = (candidate) => {
              if (!(candidate instanceof Element) || !candidate.isConnected || candidate.getClientRects().length === 0) return false;
              if (typeof candidate.checkVisibility === 'function') {
                return candidate.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
              }
              const style = getComputedStyle(candidate);
              return style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
            };
            if (!visible(el)) return { status: 'not visible' };
            if (el.matches(':disabled') || el.getAttribute('aria-disabled') === 'true') return { status: 'disabled' };
            const tag = el.tagName;
            const inputType = tag === 'INPUT' ? (el.getAttribute('type') || 'text').toLowerCase() : null;
            const textInput = tag === 'INPUT' && ![
              'button', 'checkbox', 'color', 'file', 'hidden', 'image', 'radio', 'range', 'reset', 'submit',
            ].includes(inputType);
            const editable = ((textInput || tag === 'TEXTAREA') && !el.readOnly) || el.isContentEditable;
            if (!editable) return { status: 'not editable' };
            el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
            el.focus();
            if (!el.isConnected || (document.activeElement !== el && !el.contains(document.activeElement))) {
              return { status: 'could not focus' };
            }
            return { status: 'ready' };
          }`,
        ) as { status?: string };
        if (result?.status === 'ready') return;
        lastStatus = result?.status ?? 'not editable';
      } catch (error) {
        if (!this.backendNodeGone(error)) throw error;
        lastStatus = 'not found';
      }
      if (Date.now() >= deadline) {
        throw new Error(`fillInput: backend node ${id} was not editable within ${timeoutSec}s (${lastStatus})`);
      }
      await this.waitMs(Math.min(50, Math.max(0, deadline - Date.now())));
    }
  }

  private async backendNodeMatchesState(id: number, state: ElementWaitState): Promise<boolean> {
    try {
      return Boolean(await this.callOnBackendNode(
        id,
        `function backendNodeMatchesState(state) {
          const el = this;
          const attached = el instanceof Element && el.isConnected;
          if (state === 'attached') return attached;
          if (state === 'detached') return !attached;
          if (!attached || el.getClientRects().length === 0) return state === 'hidden';
          let visible;
          if (typeof el.checkVisibility === 'function') {
            visible = el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
          } else {
            const style = getComputedStyle(el);
            visible = style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
          }
          return state === 'visible' ? visible : !visible;
        }`,
        [state],
      ));
    } catch (error) {
      if (!this.backendNodeGone(error)) throw error;
      return state === 'detached' || state === 'hidden';
    }
  }

  private async waitForClickablePoint(selector: string, timeoutSec: number): Promise<ClickPoint> {
    const { deadline } = this.waitDeadline(timeoutSec * 1000);
    let lastStatus = 'not found';
    for (;;) {
      const result = await this.evaluateInPage(
        `(async function resolveClickTarget(selector) {
          const visible = (el) => {
            if (!el.isConnected || el.getClientRects().length === 0) return false;
            if (typeof el.checkVisibility === 'function') {
              return el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
            }
            const style = getComputedStyle(el);
            return style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
          };
          const candidates = [...document.querySelectorAll(selector)].filter(visible);
          if (candidates.length === 0) return { status: 'not visible' };
          if (candidates.length > 1) return { status: 'multiple', count: candidates.length };
          const el = candidates[0];
          if (el.matches(':disabled') || el.getAttribute('aria-disabled') === 'true') {
            return { status: 'disabled' };
          }
          el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
          const before = el.getBoundingClientRect();
          await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
          if (!el.isConnected || !visible(el)) return { status: 'detached or hidden' };
          const after = el.getBoundingClientRect();
          const stable =
            Math.abs(before.x - after.x) < 0.25 &&
            Math.abs(before.y - after.y) < 0.25 &&
            Math.abs(before.width - after.width) < 0.25 &&
            Math.abs(before.height - after.height) < 0.25;
          if (!stable) return { status: 'moving' };
          const left = Math.max(0, after.left);
          const right = Math.min(innerWidth, after.right);
          const top = Math.max(0, after.top);
          const bottom = Math.min(innerHeight, after.bottom);
          if (right <= left || bottom <= top) return { status: 'outside viewport' };
          const x = left + (right - left) / 2;
          const y = top + (bottom - top) / 2;
          const hit = document.elementFromPoint(x, y);
          if (!hit || (hit !== el && !el.contains(hit))) {
            return {
              status: 'intercepted',
              hit: hit ? hit.tagName.toLowerCase() : null,
            };
          }
          return { status: 'ready', x, y };
        })(${JSON.stringify(selector)})`,
      ) as { status?: string; count?: number; x?: number; y?: number };

      if (result?.status === 'ready' && typeof result.x === 'number' && typeof result.y === 'number') {
        return { x: result.x, y: result.y };
      }
      if (result?.status === 'multiple') {
        throw new Error(
          `click: selector ${JSON.stringify(selector)} matches ${result.count} visible elements; use a more specific selector`,
        );
      }
      lastStatus = result?.status ?? 'not actionable';
      if (Date.now() >= deadline) {
        throw new Error(
          `click: selector ${JSON.stringify(selector)} was not actionable within ${timeoutSec}s (${lastStatus})`,
        );
      }
      await this.waitMs(Math.min(50, Math.max(0, deadline - Date.now())));
    }
  }

  private async waitForFillTarget(selector: string, timeoutSec: number): Promise<void> {
    const { deadline } = this.waitDeadline(timeoutSec * 1000);
    let lastStatus = 'not found';
    for (;;) {
      const result = await this.evaluateInPage(
        `(function resolveFillTarget(selector) {
          const visible = (el) => {
            if (!el.isConnected || el.getClientRects().length === 0) return false;
            if (typeof el.checkVisibility === 'function') {
              return el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
            }
            const style = getComputedStyle(el);
            return style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
          };
          const candidates = [...document.querySelectorAll(selector)].filter(visible);
          if (candidates.length === 0) return { status: 'not visible' };
          if (candidates.length > 1) return { status: 'multiple', count: candidates.length };
          const el = candidates[0];
          if (el.matches(':disabled') || el.getAttribute('aria-disabled') === 'true') {
            return { status: 'disabled' };
          }
          const tag = el.tagName;
          const inputType = tag === 'INPUT' ? (el.getAttribute('type') || 'text').toLowerCase() : null;
          const textInput = tag === 'INPUT' && ![
            'button', 'checkbox', 'color', 'file', 'hidden', 'image', 'radio', 'range', 'reset', 'submit',
          ].includes(inputType);
          const editable =
            ((textInput || tag === 'TEXTAREA') && !el.readOnly) ||
            el.isContentEditable;
          if (!editable) return { status: 'not editable' };
          el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
          el.focus();
          if (!el.isConnected || (document.activeElement !== el && !el.contains(document.activeElement))) {
            return { status: 'could not focus' };
          }
          return { status: 'ready' };
        })(${JSON.stringify(selector)})`,
      ) as { status?: string; count?: number };

      if (result?.status === 'ready') return;
      if (result?.status === 'multiple') {
        throw new Error(
          `fillInput: selector ${JSON.stringify(selector)} matches ${result.count} visible elements; use a more specific selector`,
        );
      }
      lastStatus = result?.status ?? 'not editable';
      if (Date.now() >= deadline) {
        throw new Error(
          `fillInput: selector ${JSON.stringify(selector)} was not editable within ${timeoutSec}s (${lastStatus})`,
        );
      }
      await this.waitMs(Math.min(50, Math.max(0, deadline - Date.now())));
    }
  }

  private async evaluateInPage(expression: string): Promise<any> {
    await this.client.ensureAttached();
    return this.client.evaluate(this.client.sessionId!, expression);
  }
}

export function buildBrowserGlobals(helpers: BrowserHelpers): Record<string, unknown> {
  const namespace = {
    cdp: helpers.cdp,
    drainEvents: helpers.drainEvents,
    waitForEvent: helpers.waitForEvent,
    gotoUrl: helpers.gotoUrl,
    pageInfo: helpers.pageInfo,
    accessibilitySnapshot: helpers.accessibilitySnapshot,
    click: helpers.click,
    typeText: helpers.typeText,
    fillInput: helpers.fillInput,
    pressKey: helpers.pressKey,
    scroll: helpers.scroll,
    captureScreenshot: helpers.captureScreenshot,
    listTabs: helpers.listTabs,
    currentTab: helpers.currentTab,
    switchTab: helpers.switchTab,
    newTab: helpers.newTab,
    closeTab: helpers.closeTab,
    ensureRealTab: helpers.ensureRealTab,
    iframeTarget: helpers.iframeTarget,
    waitMs: helpers.waitMs,
    waitForLoad: helpers.waitForLoad,
    waitForElement: helpers.waitForElement,
    waitForNetworkIdle: helpers.waitForNetworkIdle,
    js: helpers.js,
    uploadFile: helpers.uploadFile,
    httpGet: helpers.httpGet,
  };
  const browser = Object.freeze({ ...namespace });
  return { browser, ...namespace };
}
