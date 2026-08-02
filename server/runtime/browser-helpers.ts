/**
 * Browser-control helper surface for the persistent browser REPL.
 *
 * Every helper is exposed twice in the REPL context: as a bare global
 * (e.g. `goto_url`) and as a method on the frozen `browser` namespace
 * (e.g. `browser.goto_url`). Helpers are implemented with raw CDP today but
 * the endpoint contract does not guarantee any particular implementation.
 */

import { writeFileSync } from 'fs';
import { CdpClient, CdpTarget, isInternalUrl } from './browser-cdp-client';

const DEFAULT_TIMEOUT_MS = 30_000;

// EXECUTION_DEADLINE_MARGIN_MS is how far below the executing request's
// deadline a wait-style helper places its own deadline when the helper's
// timeout would otherwise tie or exceed it. The margin leaves room for the
// helper's error to unwind and the response to be written before the
// daemon's execution timer fires and destructively kills the REPL.
const EXECUTION_DEADLINE_MARGIN_MS = 500;

const API_PORT = process.env.KERNEL_API_PORT || process.env.PORT || '10001';
const API_BASE = `http://127.0.0.1:${API_PORT}`;

export interface RecordingState {
  recorderId: string;
  dir: string | null;
}

// MODIFIER_SUGAR maps the object-sugar modifier keys accepted by press_key
// ({ctrl: true}) to their canonical CDP names.
const MODIFIER_SUGAR: Record<string, string> = {
  alt: 'Alt',
  ctrl: 'Control',
  control: 'Control',
  meta: 'Meta',
  shift: 'Shift',
};

/**
 * Normalize the modifiers argument of press_key: an array drawn from Alt,
 * Control, Meta, Shift, or the object sugar {ctrl/alt/shift/meta: true}.
 * Anything else is a usage error and must say so clearly — a bare object
 * used to fail with a cryptic "(modifiers ?? []) is not iterable" TypeError
 * and a bare string was iterated character by character.
 */
function normalizeKeyModifiers(modifiers?: string[] | Record<string, boolean>): string[] {
  if (modifiers === undefined || modifiers === null) {
    return [];
  }
  if (Array.isArray(modifiers)) {
    return modifiers;
  }
  if (typeof modifiers === 'object') {
    const out: string[] = [];
    for (const [name, on] of Object.entries(modifiers)) {
      if (!on) continue;
      const canonical = MODIFIER_SUGAR[name.toLowerCase()];
      if (canonical === undefined) {
        throw new Error(`press_key: unknown modifier: ${name} (expected Alt, Control, Meta, or Shift)`);
      }
      if (!out.includes(canonical)) {
        out.push(canonical);
      }
    }
    return out;
  }
  throw new Error(
    'press_key: modifiers must be an array drawn from Alt, Control, Meta, Shift (or an object like {ctrl: true})',
  );
}

export class BrowserHelpers {
  private readonly client: CdpClient;
  private activeRecording: RecordingState | null = null;

  /**
   * Deadline (epoch ms) of the in-flight REPL execution, set by the daemon
   * around each execution. Wait-style helpers clamp their internal deadline
   * to just below it: a routine helper timeout must surface as a clean
   * error, not tie the execution timeout, which destructively kills the
   * REPL.
   */
  executionDeadlineMs: number | null = null;

  constructor(client: CdpClient) {
    this.client = client;
  }

  /**
   * Effective deadline for a wait-style helper: the helper's own timeout,
   * clamped to just below the executing request's deadline (when one is
   * set) so the helper's error wins over the destructive execution timeout.
   */
  private waitDeadline(timeoutMs: number): { deadline: number; clamped: boolean } {
    const own = Date.now() + timeoutMs;
    const exec = this.executionDeadlineMs;
    if (exec !== null && exec - EXECUTION_DEADLINE_MARGIN_MS < own) {
      return { deadline: exec - EXECUTION_DEADLINE_MARGIN_MS, clamped: true };
    }
    return { deadline: own, clamped: false };
  }

  // -----------------------------------------------------------------------
  // Escape hatch + events
  // -----------------------------------------------------------------------

  /**
   * Send an unrestricted CDP command. By default commands are routed to the
   * attached page session; pass session_id === null for browser-level
   * commands (e.g. Target.createTarget) or a session ID string to route to a
   * specific session.
   */
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

  /** Return and clear buffered CDP events for the attached session. */
  drainEvents = async (): Promise<unknown[]> => {
    await this.client.ensureAttached();
    return this.client.drainEvents();
  };

  // -----------------------------------------------------------------------
  // Navigation + page state
  // -----------------------------------------------------------------------

  /** Navigate the attached tab. */
  gotoUrl = async (url: string): Promise<unknown> => {
    const res = await this.client.sessionCommand<any>('Page.navigate', { url });
    if (res.errorText) {
      throw new Error(`navigation to ${url} failed: ${res.errorText}`);
    }
    return { url, frame_id: res.frameId, loader_id: res.loaderId ?? null };
  };

  /** URL, title, viewport, scroll position, page dimensions, pending dialog. */
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

  // -----------------------------------------------------------------------
  // Input
  // -----------------------------------------------------------------------

  /** Dispatch mouse press/release at CSS viewport coordinates. */
  clickAtXy = async (x: number, y: number): Promise<void> => {
    await this.client.sessionCommand('Input.dispatchMouseEvent', {
      type: 'mousePressed',
      x,
      y,
      button: 'left',
      clickCount: 1,
    });
    await this.client.sessionCommand('Input.dispatchMouseEvent', {
      type: 'mouseReleased',
      x,
      y,
      button: 'left',
      clickCount: 1,
    });
  };

  /** Insert text through CDP input (as if typed/pasted into the focused element). */
  typeText = async (text: string): Promise<void> => {
    await this.client.sessionCommand('Input.insertText', { text });
  };

  /**
   * Fill a framework-managed input: focuses the element, sets the value via
   * the native setter (so React/Vue/Angular detect the change), and
   * dispatches the expected DOM events.
   */
  fillInput = async (selector: string, value: string): Promise<void> => {
    await this.evaluateInPage(
      `(function (selector, value) {
        const el = document.querySelector(selector);
        if (!el) throw new Error('no element matches selector: ' + selector);
        el.focus();
        const proto = el instanceof HTMLTextAreaElement
          ? HTMLTextAreaElement.prototype
          : HTMLInputElement.prototype;
        const descriptor = Object.getOwnPropertyDescriptor(proto, 'value');
        if (descriptor && descriptor.set) {
          descriptor.set.call(el, value);
        } else {
          el.value = value;
        }
        el.dispatchEvent(new Event('input', { bubbles: true }));
        el.dispatchEvent(new Event('change', { bubbles: true }));
        return true;
      })(${JSON.stringify(selector)}, ${JSON.stringify(value)})`,
    );
  };

  private static readonly MODIFIER_BITS: Record<string, number> = {
    Alt: 1,
    Control: 2,
    Meta: 4,
    Shift: 8,
  };

  private static readonly KEY_DEFS: Record<
    string,
    { windowsVirtualKeyCode: number; code: string; key: string; text?: string }
  > = {
    Enter: { windowsVirtualKeyCode: 13, code: 'Enter', key: 'Enter', text: '\r' },
    Tab: { windowsVirtualKeyCode: 9, code: 'Tab', key: 'Tab' },
    Escape: { windowsVirtualKeyCode: 27, code: 'Escape', key: 'Escape' },
    Backspace: { windowsVirtualKeyCode: 8, code: 'Backspace', key: 'Backspace' },
    Delete: { windowsVirtualKeyCode: 46, code: 'Delete', key: 'Delete' },
    ArrowLeft: { windowsVirtualKeyCode: 37, code: 'ArrowLeft', key: 'ArrowLeft' },
    ArrowUp: { windowsVirtualKeyCode: 38, code: 'ArrowUp', key: 'ArrowUp' },
    ArrowRight: { windowsVirtualKeyCode: 39, code: 'ArrowRight', key: 'ArrowRight' },
    ArrowDown: { windowsVirtualKeyCode: 40, code: 'ArrowDown', key: 'ArrowDown' },
    Home: { windowsVirtualKeyCode: 36, code: 'Home', key: 'Home' },
    End: { windowsVirtualKeyCode: 35, code: 'End', key: 'End' },
    PageUp: { windowsVirtualKeyCode: 33, code: 'PageUp', key: 'PageUp' },
    PageDown: { windowsVirtualKeyCode: 34, code: 'PageDown', key: 'PageDown' },
    Space: { windowsVirtualKeyCode: 32, code: 'Space', key: ' ', text: ' ' },
  };

  /**
   * Dispatch keyboard events. `key` is a single printable character or a
   * navigation key name (Enter, Tab, Escape, Backspace, Delete, Arrow*,
   * Home, End, PageUp, PageDown, Space). `modifiers` is an optional array
   * drawn from Alt, Control, Meta, Shift, or the object sugar
   * {ctrl/alt/shift/meta: true}.
   */
  pressKey = async (key: string, modifiers?: string[] | Record<string, boolean>): Promise<void> => {
    let modifierBits = 0;
    for (const m of normalizeKeyModifiers(modifiers)) {
      const bit = BrowserHelpers.MODIFIER_BITS[m];
      if (bit === undefined) {
        throw new Error(`press_key: unknown modifier: ${m} (expected Alt, Control, Meta, or Shift)`);
      }
      modifierBits |= bit;
    }

    let def = BrowserHelpers.KEY_DEFS[key];
    if (!def) {
      if (key.length === 1) {
        const upper = key.toUpperCase();
        def = {
          windowsVirtualKeyCode: upper.charCodeAt(0),
          code: /^[a-zA-Z]$/.test(key) ? `Key${upper}` : key,
          key,
          text: key,
        };
      } else {
        throw new Error(
          `unknown key: ${key} (use a single character or one of ${Object.keys(BrowserHelpers.KEY_DEFS).join(', ')})`,
        );
      }
    }

    const base = {
      code: def.code,
      key: def.key,
      windowsVirtualKeyCode: def.windowsVirtualKeyCode,
      nativeVirtualKeyCode: def.windowsVirtualKeyCode,
      modifiers: modifierBits,
    };
    await this.client.sessionCommand('Input.dispatchKeyEvent', {
      ...base,
      type: def.text ? 'keyDown' : 'rawKeyDown',
      ...(def.text ? { text: def.text } : {}),
    });
    await this.client.sessionCommand('Input.dispatchKeyEvent', { ...base, type: 'keyUp' });
  };

  /** Dispatch a mouse-wheel event at a viewport coordinate. */
  scroll = async (x: number, y: number, deltaX = 0, deltaY = 0): Promise<void> => {
    await this.client.sessionCommand('Input.dispatchMouseEvent', {
      type: 'mouseWheel',
      x,
      y,
      deltaX,
      deltaY,
    });
  };

  /**
   * Dispatch DOM KeyboardEvents (keydown + keyup) against a selected
   * element. Unlike press_key this does not require focus and is visible
   * only to JavaScript listeners.
   */
  dispatchKey = async (
    selector: string,
    key: string,
    opts?: Record<string, unknown>,
  ): Promise<void> => {
    await this.evaluateInPage(
      `(function (selector, key, opts) {
        const el = document.querySelector(selector);
        if (!el) throw new Error('no element matches selector: ' + selector);
        const init = Object.assign({ key, bubbles: true, cancelable: true }, opts || {});
        el.dispatchEvent(new KeyboardEvent('keydown', init));
        el.dispatchEvent(new KeyboardEvent('keyup', init));
        return true;
      })(${JSON.stringify(selector)}, ${JSON.stringify(key)}, ${JSON.stringify(opts ?? null)})`,
    );
  };

  // -----------------------------------------------------------------------
  // Screenshots
  // -----------------------------------------------------------------------

  /**
   * Save a PNG screenshot inside the VM and return its path. When
   * full_page is true the full scrollable page is captured; max_dim scales
   * the capture down so neither dimension exceeds max_dim pixels.
   */
  captureScreenshot = async (
    path?: string,
    fullPage = false,
    maxDim?: number,
  ): Promise<string> => {
    const outPath = path ?? `/tmp/screenshot-${Date.now()}.png`;

    const metrics = await this.client.sessionCommand<any>('Page.getLayoutMetrics');
    const viewport = metrics.cssLayoutViewport ?? metrics.layoutViewport;
    let width: number;
    let height: number;
    if (fullPage) {
      const content = metrics.cssContentSize ?? metrics.contentSize;
      width = Math.ceil(content.width);
      height = Math.ceil(content.height);
    } else {
      width = Math.ceil(viewport.clientWidth);
      height = Math.ceil(viewport.clientHeight);
    }

    let scale = 1;
    if (maxDim && maxDim > 0) {
      const largest = Math.max(width, height);
      if (largest > maxDim) {
        scale = maxDim / largest;
      }
    }

    const shot = await this.client.sessionCommand<any>('Page.captureScreenshot', {
      format: 'png',
      clip: { x: 0, y: 0, width, height, scale },
      ...(fullPage ? { captureBeyondViewport: true } : {}),
    });
    writeFileSync(outPath, Buffer.from(shot.data, 'base64'));
    return outPath;
  };

  // -----------------------------------------------------------------------
  // Tabs
  // -----------------------------------------------------------------------

  /** List browser page targets; internal pages excluded unless requested. */
  listTabs = async (includeInternal = false): Promise<Record<string, unknown>[]> => {
    const targets = await this.client.listTargets();
    return targets
      .filter((t) => t.type === 'page')
      .filter((t) => includeInternal || !isInternalUrl(t.url))
      .map((t) => ({
        id: t.targetId,
        url: t.url,
        title: t.title,
        internal: isInternalUrl(t.url),
        attached: t.targetId === this.client.targetId,
      }));
  };

  /** Metadata for the attached target. */
  currentTab = async (): Promise<Record<string, unknown>> => {
    await this.client.ensureAttached();
    const targets = await this.client.listTargets();
    const current = targets.find((t) => t.targetId === this.client.targetId);
    if (!current) {
      throw new Error('attached target no longer exists');
    }
    return {
      id: current.targetId,
      url: current.url,
      title: current.title,
      internal: isInternalUrl(current.url),
      attached: true,
    };
  };

  /** Attach the runtime to another page target. */
  switchTab = async (targetId: string): Promise<Record<string, unknown>> => {
    await this.client.attach(targetId);
    return this.currentTab();
  };

  /** Create and attach to a new page target, optionally navigating it. */
  newTab = async (url?: string): Promise<Record<string, unknown>> => {
    await this.client.ensureConnected();
    const created = await this.client.browserCommand<{ targetId: string }>('Target.createTarget', {
      url: url ?? 'about:blank',
    });
    await this.client.attach(created.targetId);
    // Target.createTarget resolves before the initial navigation commits;
    // wait (best effort) so an immediate page_info/list_tabs observes the
    // requested URL rather than about:blank.
    if (url && url !== 'about:blank') {
      await this.client.waitForNavigationCommit(created.targetId, 5_000);
    }
    return this.currentTab();
  };

  /** Close a page target (the attached tab when no id is given). */
  closeTab = async (targetId?: string): Promise<void> => {
    await this.client.ensureConnected();
    const id = targetId ?? this.client.targetId;
    if (!id) {
      throw new Error('no tab is attached and no target id was provided');
    }
    await this.client.browserCommand('Target.closeTarget', { targetId: id });
    if (id === this.client.targetId) {
      this.client.sessionId = null;
      this.client.targetId = null;
    }
    // Target.closeTarget resolves before the target is fully destroyed;
    // wait (best effort) so an immediate list_tabs no longer counts the
    // closed tab.
    await this.client.waitForTargetGone(id, 5_000);
  };

  /** Ensure the runtime is attached to a non-internal page. */
  ensureRealTab = async (): Promise<Record<string, unknown>> => {
    const target: CdpTarget = await this.client.ensureRealTab();
    return {
      id: target.targetId,
      url: target.url,
      title: target.title,
      internal: isInternalUrl(target.url),
      attached: true,
    };
  };

  /** Find an out-of-process iframe target by URL substring. */
  iframeTarget = async (urlSubstring: string): Promise<Record<string, unknown> | null> => {
    const targets = await this.client.listTargets();
    const match = targets.find((t) => t.type === 'iframe' && t.url.includes(urlSubstring));
    if (!match) return null;
    return { id: match.targetId, url: match.url, title: match.title, type: match.type };
  };

  // -----------------------------------------------------------------------
  // Waiting
  // -----------------------------------------------------------------------

  /** Promise-based sleep; seconds for API compatibility. */
  wait = async (seconds: number): Promise<void> => {
    await new Promise((resolve) => setTimeout(resolve, seconds * 1000));
  };

  /** Wait for the document ready state (default 'complete'). */
  waitForLoad = async (state = 'complete', timeoutSec = 30): Promise<string> => {
    const { deadline, clamped } = this.waitDeadline(timeoutSec * 1000);
    const order = ['loading', 'interactive', 'complete'];
    const want = order.indexOf(state);
    if (want === -1) {
      throw new Error(`unknown ready state: ${state} (expected loading, interactive, or complete)`);
    }
    let current = 'loading';
    for (;;) {
      // Check the deadline before issuing another CDP command: near the
      // deadline the command's own clamped timeout would otherwise
      // preempt this helper's (clearer) timeout error.
      if (Date.now() > deadline) {
        throw new Error(
          `timed out waiting for readyState ${state} (still ${current})` +
            (clamped ? ' (bounded by the execution timeout)' : ''),
        );
      }
      const res = await this.client.sessionCommand<any>('Runtime.evaluate', {
        expression: 'document.readyState',
        returnByValue: true,
      });
      current = res.result?.value ?? 'loading';
      if (order.indexOf(current) >= want) {
        return current;
      }
      await this.wait(0.1);
    }
  };

  /** Wait for a selector, optionally requiring visibility. */
  waitForElement = async (
    selector: string,
    opts?: { visible?: boolean; timeout_sec?: number },
  ): Promise<boolean> => {
    if (opts !== undefined && opts !== null && (typeof opts !== 'object' || Array.isArray(opts))) {
      throw new Error('wait_for_element: opts must be an object {visible, timeout_sec}');
    }
    const timeoutMs = (opts?.timeout_sec ?? 30) * 1000;
    const { deadline, clamped } = this.waitDeadline(timeoutMs);
    for (;;) {
      // Check the deadline before issuing another CDP command: near the
      // deadline the command's own clamped timeout would otherwise
      // preempt this helper's (clearer) timeout error.
      if (Date.now() > deadline) {
        throw new Error(
          `timed out waiting for element: ${selector}` +
            (clamped ? ' (bounded by the execution timeout)' : ''),
        );
      }
      const found = await this.evaluateInPage(
        `(function (selector, requireVisible) {
          const el = document.querySelector(selector);
          if (!el) return false;
          if (!requireVisible) return true;
          const rect = el.getBoundingClientRect();
          const style = getComputedStyle(el);
          return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
        })(${JSON.stringify(selector)}, ${opts?.visible === true})`,
      );
      if (found) return true;
      await this.wait(0.1);
    }
  };

  /**
   * Wait until no Network requests have been in flight for idle_sec
   * (default 0.5s), using buffered Network domain events.
   */
  waitForNetworkIdle = async (idleSec = 0.5, timeoutSec = 30): Promise<void> => {
    const { deadline, clamped } = this.waitDeadline(timeoutSec * 1000);
    for (;;) {
      const { inFlight, lastActivity } = this.client.networkIdleState();
      const now = Date.now();
      if (inFlight === 0 && now - lastActivity >= idleSec * 1000) {
        return;
      }
      if (now > deadline) {
        throw new Error(
          `timed out waiting for network idle (${inFlight} requests in flight)` +
            (clamped ? ' (bounded by the execution timeout)' : ''),
        );
      }
      await this.wait(0.1);
    }
  };

  // -----------------------------------------------------------------------
  // JavaScript evaluation + uploads
  // -----------------------------------------------------------------------

  /**
   * Evaluate JavaScript in the attached page, or in the page/iframe target
   * identified by target_id (see iframe_target / list_tabs). Returns the
   * JSON-compatible value of the expression.
   */
  js = async (code: string, targetId?: string): Promise<unknown> => {
    if (targetId !== undefined && targetId !== null && typeof targetId !== 'string') {
      // A natural mistake is passing an options object ({target: id}) given
      // other helpers take opts objects; that used to surface a raw CDP
      // 'Invalid parameters' error from Target.attachToTarget.
      throw new Error('js: target must be a target id string (see iframe_target/list_tabs)');
    }
    if (targetId) {
      return this.client.evaluateOnTarget(targetId, code);
    }
    return this.evaluateInPage(code);
  };

  /** Set files on an <input type=file> using VM-local paths. */
  uploadFile = async (selector: string, paths: string[]): Promise<void> => {
    if (!Array.isArray(paths) || paths.length === 0) {
      throw new Error('upload_file requires a non-empty array of VM-local file paths');
    }
    const doc = await this.client.sessionCommand<any>('DOM.getDocument', { depth: 0 });
    const queried = await this.client.sessionCommand<any>('DOM.querySelector', {
      nodeId: doc.root.nodeId,
      selector,
    });
    if (!queried.nodeId) {
      throw new Error(`no element matches selector: ${selector}`);
    }
    await this.client.sessionCommand('DOM.setFileInputFiles', {
      nodeId: queried.nodeId,
      files: paths,
    });
  };

  // -----------------------------------------------------------------------
  // HTTP
  // -----------------------------------------------------------------------

  /** Perform an HTTP GET from inside the VM and return the response body. */
  httpGet = async (url: string): Promise<string> => {
    const res = await fetch(url);
    if (!res.ok) {
      throw new Error(`GET ${url} failed with status ${res.status}`);
    }
    return res.text();
  };

  // -----------------------------------------------------------------------
  // Recording (delegates to the Kernel recording API)
  // -----------------------------------------------------------------------

  /** Start a Kernel browser recording; returns its identifier. */
  startRecording = async (opts?: Record<string, unknown>): Promise<Record<string, unknown>> => {
    const body = (opts ?? {}) as Record<string, unknown>;
    const res = await fetch(`${API_BASE}/recording/start`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (res.status === 409) {
      throw new Error('a recording is already in progress');
    }
    if (!res.ok) {
      const text = await res.text().catch(() => '');
      throw new Error(`failed to start recording (status ${res.status}): ${text}`);
    }
    const recorderId = typeof body.id === 'string' && body.id !== '' ? body.id : 'default';
    this.activeRecording = { recorderId, dir: process.env.OUTPUT_DIR ?? null };
    return { recorder_id: recorderId };
  };

  /** Stop the active Kernel browser recording. */
  stopRecording = async (opts?: Record<string, unknown>): Promise<Record<string, unknown>> => {
    const body = (opts ?? {}) as Record<string, unknown>;
    const res = await fetch(`${API_BASE}/recording/stop`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const text = await res.text().catch(() => '');
      throw new Error(`failed to stop recording (status ${res.status}): ${text}`);
    }
    const recorderId = this.activeRecording?.recorderId ?? 'default';
    this.activeRecording = null;
    return { recorder_id: recorderId };
  };

  /** The active recording directory, or null when no recording is active. */
  recordingDir = async (): Promise<string | null> => {
    return this.activeRecording ? this.activeRecording.dir : null;
  };

  // -----------------------------------------------------------------------
  // Internals
  // -----------------------------------------------------------------------

  private async evaluateInPage(expression: string): Promise<any> {
    const res = await this.client.sessionCommand<any>('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
    });
    if (res.exceptionDetails) {
      const desc =
        res.exceptionDetails.exception?.description ?? res.exceptionDetails.text ?? 'evaluation failed';
      throw new Error(desc);
    }
    return res.result?.value;
  }
}

/**
 * Build the seeded globals for a REPL context: the frozen `browser`
 * namespace plus bare aliases for concise agent-authored code.
 */
export function buildBrowserGlobals(helpers: BrowserHelpers): Record<string, unknown> {
  const namespace = {
    cdp: helpers.cdp,
    drain_events: helpers.drainEvents,
    goto_url: helpers.gotoUrl,
    page_info: helpers.pageInfo,
    click_at_xy: helpers.clickAtXy,
    type_text: helpers.typeText,
    fill_input: helpers.fillInput,
    press_key: helpers.pressKey,
    scroll: helpers.scroll,
    capture_screenshot: helpers.captureScreenshot,
    list_tabs: helpers.listTabs,
    current_tab: helpers.currentTab,
    switch_tab: helpers.switchTab,
    new_tab: helpers.newTab,
    close_tab: helpers.closeTab,
    ensure_real_tab: helpers.ensureRealTab,
    iframe_target: helpers.iframeTarget,
    wait: helpers.wait,
    wait_for_load: helpers.waitForLoad,
    wait_for_element: helpers.waitForElement,
    wait_for_network_idle: helpers.waitForNetworkIdle,
    js: helpers.js,
    dispatch_key: helpers.dispatchKey,
    upload_file: helpers.uploadFile,
    http_get: helpers.httpGet,
    start_recording: helpers.startRecording,
    stop_recording: helpers.stopRecording,
    recording_dir: helpers.recordingDir,
  };
  const browser = Object.freeze({ ...namespace });
  return { browser, ...namespace };
}
