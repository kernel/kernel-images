/**
 * Minimal raw CDP client for the persistent browser REPL.
 *
 * Maintains a single browser-level WebSocket to the DevTools proxy
 * (ws://127.0.0.1:9222) and one attached page session. The connection is
 * established lazily and re-established automatically after a Chromium
 * restart; a dropped connection only clears browser/session state, never
 * JavaScript bindings in the REPL context.
 *
 * Uses Node 22's built-in WebSocket (undici) so no extra dependencies are
 * required in the image.
 */

export interface CdpEvent {
  method: string;
  params: unknown;
  sessionId?: string;
  time: number;
}

export interface CdpTarget {
  targetId: string;
  type: string;
  title: string;
  url: string;
  attached: boolean;
}

export interface PendingDialog {
  type: string;
  message: string;
  since: number;
}

interface PendingCommand {
  resolve: (value: unknown) => void;
  reject: (error: Error) => void;
  method: string;
  timer: ReturnType<typeof setTimeout>;
}

const EVENT_RING_CAPACITY = 500;
const CONNECT_TIMEOUT_MS = 10_000;
// Individual CDP commands must not hang forever: Chromium occasionally never
// answers a command (e.g. Input.dispatchMouseEvent mouseWheel on a
// non-scrollable page), and an unanswered command would otherwise wedge the
// serialized execution queue until the outer execution timeout kills the
// whole REPL.
const COMMAND_TIMEOUT_MS = 30_000;
// COMMAND_DEADLINE_MARGIN_MS is how far below the executing request's
// deadline a CDP command's effective timeout is clamped, leaving room for
// the error to unwind and the response to be written before the daemon's
// execution timer fires and destructively kills the REPL. A renderer frozen
// behind a modal JavaScript dialog never answers session-routed commands;
// without this clamp every such command burned a whole REPL per attempt.
const COMMAND_DEADLINE_MARGIN_MS = 500;
// ENABLE_DOMAINS_BUDGET_MS bounds the total time attach() spends enabling
// CDP domains, and ENABLE_DOMAIN_COMMAND_TIMEOUT_MS each individual enable.
// A frozen renderer never answers Page.enable, so without a budget attach
// alone could consume the entire execution deadline and the command the
// caller actually wanted (e.g. Page.handleJavaScriptDialog or Page.reload,
// both answered browser-side) would never be sent.
const ENABLE_DOMAINS_BUDGET_MS = 10_000;
const ENABLE_DOMAIN_COMMAND_TIMEOUT_MS = 5_000;
// DIALOG_DISMISS_TIMEOUT_MS bounds the best-effort dismissal of a dialog
// that was already open when the runtime attached.
const DIALOG_DISMISS_TIMEOUT_MS = 5_000;

/** CdpCommandTimeoutError marks a command that exceeded its time budget. */
export class CdpCommandTimeoutError extends Error {
  readonly cdpCommandTimeout = true;
}

function isCdpCommandTimeout(err: unknown): boolean {
  return err instanceof CdpCommandTimeoutError || (err as any)?.cdpCommandTimeout === true;
}

/** URL prefixes considered "internal" browser pages. */
const INTERNAL_URL_PREFIXES = [
  'chrome://',
  'chrome-extension://',
  'chrome-untrusted://',
  'devtools://',
  'edge://',
  'about:srcdoc',
];

export function isInternalUrl(url: string): boolean {
  return INTERNAL_URL_PREFIXES.some((p) => url.startsWith(p));
}

export class CdpClient {
  private readonly endpoint: string;
  private ws: WebSocket | null = null;
  private connecting: Promise<void> | null = null;
  private nextId = 1;
  private pending = new Map<number, PendingCommand>();
  private events: CdpEvent[] = [];

  /** Currently attached page session. */
  sessionId: string | null = null;
  /** Target ID for the attached session. */
  targetId: string | null = null;

  /** Pending JavaScript dialog for the attached session, if any. */
  pendingDialog: PendingDialog | null = null;

  /**
   * Deadline (epoch ms) of the in-flight REPL execution, set by the daemon
   * around each execution. Every command's effective timeout is clamped to
   * just below it, so a renderer frozen behind a modal dialog surfaces a
   * clean error instead of tying the destructive execution timeout.
   */
  executionDeadlineMs: number | null = null;

  /**
   * Whether the attached target's renderer answered domain enables. False
   * after an attach that hit a frozen renderer (e.g. a dialog left open by
   * a previous REPL); sessionCommand retries the enables so the session
   * recovers automatically once the renderer unfreezes.
   */
  private rendererResponsive = true;

  /**
   * Invoked when attach() dismisses a JavaScript dialog that was already
   * open on the target. Wired by the daemon to surface a stderr note.
   */
  onDialogAutoDismissed?: (dialog: PendingDialog) => void;

  /** Network in-flight tracking for wait_for_network_idle. */
  private inFlightRequests = new Set<string>();
  private lastNetworkActivity = 0;

  constructor(endpoint: string) {
    this.endpoint = endpoint;
  }

  /** Whether the browser-level WebSocket is currently open. */
  get connected(): boolean {
    return this.ws !== null && this.ws.readyState === WebSocket.OPEN;
  }

  /**
   * Resolve the browser-level WebSocket URL. The Kernel DevTools proxy
   * routes every WebSocket connection to the browser endpoint regardless of
   * path, but a raw Chromium requires /devtools/browser/<id>; discover it
   * via /json/version when the configured endpoint has no path.
   */
  private async resolveBrowserWsUrl(): Promise<string> {
    let parsed: URL;
    try {
      parsed = new URL(this.endpoint);
    } catch {
      return this.endpoint;
    }
    if (parsed.pathname && parsed.pathname !== '/') {
      return this.endpoint;
    }
    const httpBase = `${parsed.protocol === 'wss:' ? 'https' : 'http'}://${parsed.host}`;
    try {
      const res = await fetch(`${httpBase}/json/version`);
      if (res.ok) {
        const info = (await res.json()) as { webSocketDebuggerUrl?: string };
        if (info.webSocketDebuggerUrl) {
          return info.webSocketDebuggerUrl;
        }
      }
    } catch {
      // Fall through to the raw endpoint.
    }
    return this.endpoint;
  }

  /** Connect the browser-level WebSocket if not already connected. */
  async ensureConnected(): Promise<void> {
    if (this.connected) return;
    if (this.connecting) return this.connecting;

    this.connecting = (async () => {
      const url = await this.resolveBrowserWsUrl();
      await this.openSocket(url);
    })();

    try {
      await this.connecting;
    } finally {
      this.connecting = null;
    }
  }

  private openSocket(url: string): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const ws = new WebSocket(url);
      const timer = setTimeout(() => {
        try {
          ws.close();
        } catch {
          // ignore
        }
        reject(new Error(`timed out connecting to CDP endpoint ${this.endpoint}`));
      }, CONNECT_TIMEOUT_MS);

      ws.addEventListener('open', () => {
        clearTimeout(timer);
        this.ws = ws;
        resolve();
      });
      ws.addEventListener('error', () => {
        clearTimeout(timer);
        reject(new Error(`failed to connect to CDP endpoint ${this.endpoint}`));
      });
      ws.addEventListener('message', (event) => {
        this.onMessage(event.data);
      });
      ws.addEventListener('close', () => {
        this.onClose(ws);
      });
    });
  }

  private onClose(closed: WebSocket): void {
    if (this.ws !== closed) return;
    this.ws = null;
    this.sessionId = null;
    this.targetId = null;
    this.pendingDialog = null;
    this.rendererResponsive = true;
    this.inFlightRequests.clear();
    const err = new Error('CDP connection closed');
    for (const [, p] of this.pending) {
      clearTimeout(p.timer);
      p.reject(err);
    }
    this.pending.clear();
  }

  private onMessage(data: unknown): void {
    if (typeof data !== 'string') return;
    let msg: any;
    try {
      msg = JSON.parse(data);
    } catch {
      return;
    }

    if (typeof msg.id === 'number') {
      const p = this.pending.get(msg.id);
      if (!p) return;
      this.pending.delete(msg.id);
      clearTimeout(p.timer);
      if (msg.error) {
        p.reject(new Error(`CDP ${p.method} failed: ${msg.error.message} (code ${msg.error.code})`));
      } else {
        p.resolve(msg.result);
      }
      return;
    }

    if (typeof msg.method === 'string') {
      this.onEvent(msg.method, msg.params, msg.sessionId);
    }
  }

  private onEvent(method: string, params: any, sessionId?: string): void {
    this.events.push({ method, params, sessionId, time: Date.now() });
    if (this.events.length > EVENT_RING_CAPACITY) {
      this.events.splice(0, this.events.length - EVENT_RING_CAPACITY);
    }

    if (sessionId && sessionId === this.sessionId) {
      if (method === 'Page.javascriptDialogOpening') {
        this.pendingDialog = {
          type: params?.type ?? 'alert',
          message: params?.message ?? '',
          since: Date.now(),
        };
      } else if (method === 'Page.javascriptDialogClosed') {
        this.pendingDialog = null;
      }
    }

    if (method.startsWith('Network.')) {
      this.lastNetworkActivity = Date.now();
      const requestId = params?.requestId;
      if (method === 'Network.requestWillBeSent' && requestId) {
        this.inFlightRequests.add(requestId);
      } else if (
        (method === 'Network.loadingFinished' || method === 'Network.loadingFailed') &&
        requestId
      ) {
        this.inFlightRequests.delete(requestId);
      }
    }

    if (method === 'Target.detachedFromTarget') {
      if (params?.sessionId && params.sessionId === this.sessionId) {
        this.sessionId = null;
        this.targetId = null;
        this.pendingDialog = null;
      }
    }
  }

  /** Send a browser-level command (no session). */
  async browserCommand<T = any>(method: string, params?: unknown): Promise<T> {
    return this.send<T>(method, params, undefined);
  }

  /** Send a command on the attached page session. */
  async sessionCommand<T = any>(method: string, params?: unknown): Promise<T> {
    await this.ensureAttached();
    if (!this.rendererResponsive) {
      // The previous attach hit a frozen renderer (e.g. a dialog left open
      // by a previous REPL). Retry the domain enables: once the renderer
      // unfreezes (dialog dismissed, page reloaded) the session recovers
      // without a reattach.
      this.rendererResponsive = await this.enableDomains(this.sessionId!);
    }
    return this.send<T>(method, params, this.sessionId!);
  }

  /** Send a raw command. sessionId === null forces browser-level routing. */
  async send<T = any>(method: string, params?: unknown, sessionId?: string, timeoutMs?: number): Promise<T> {
    await this.ensureConnected();
    const id = this.nextId++;
    const payload: Record<string, unknown> = { id, method };
    if (params !== undefined) payload.params = params;
    if (sessionId) payload.sessionId = sessionId;

    const { timeout, clampedByDeadline } = this.effectiveCommandTimeout(timeoutMs);
    const rendererHint =
      sessionId && !this.rendererResponsive
        ? ' (the page renderer is unresponsive — a modal JavaScript dialog may be blocking it; ' +
          'recover with cdp("Page.handleJavaScriptDialog", { accept: true }) or cdp("Page.reload"))'
        : '';
    if (timeout <= 0) {
      throw new CdpCommandTimeoutError(
        `CDP ${method} could not run: the execution deadline has already been reached${rendererHint}`,
      );
    }

    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        if (this.pending.delete(id)) {
          let message = `CDP ${method} timed out after ${timeout}ms`;
          if (clampedByDeadline) {
            message += ' (bounded by the execution timeout)';
          }
          message += rendererHint;
          reject(new CdpCommandTimeoutError(message));
        }
      }, timeout);
      if (typeof timer.unref === 'function') timer.unref();
      this.pending.set(id, { resolve: resolve as (v: unknown) => void, reject, method, timer });
      try {
        this.ws!.send(JSON.stringify(payload));
      } catch (err: any) {
        clearTimeout(timer);
        this.pending.delete(id);
        reject(new Error(`failed to send CDP ${method}: ${err?.message ?? err}`));
      }
    });
  }

  /**
   * Effective timeout for one command: the caller's override (or the
   * default), clamped to just below the executing request's deadline so a
   * hung command loses to the daemon's destructive execution timer.
   */
  private effectiveCommandTimeout(overrideMs?: number): { timeout: number; clampedByDeadline: boolean } {
    let timeout = overrideMs ?? COMMAND_TIMEOUT_MS;
    const exec = this.executionDeadlineMs;
    if (exec !== null) {
      const remaining = exec - COMMAND_DEADLINE_MARGIN_MS - Date.now();
      if (remaining < timeout) {
        return { timeout: remaining, clampedByDeadline: true };
      }
    }
    return { timeout, clampedByDeadline: false };
  }

  /** List all targets from the browser. */
  async listTargets(): Promise<CdpTarget[]> {
    await this.ensureConnected();
    const res = await this.browserCommand<{ targetInfos: any[] }>('Target.getTargets');
    return (res.targetInfos ?? []).map((t) => ({
      targetId: t.targetId,
      type: t.type,
      title: t.title ?? '',
      url: t.url ?? '',
      attached: !!t.attached,
    }));
  }

  /** Attach to a page target, replacing any current attachment. */
  async attach(targetId: string): Promise<void> {
    await this.ensureConnected();
    const res = await this.browserCommand<{ sessionId: string }>('Target.attachToTarget', {
      targetId,
      flatten: true,
    });
    this.sessionId = res.sessionId;
    this.targetId = targetId;
    this.pendingDialog = null;
    this.rendererResponsive = await this.enableDomains(this.sessionId);
    await this.dismissStaleDialog();
  }

  /**
   * Enable the CDP domains the runtime relies on. Returns false when a
   * command timed out (or the budget ran out), which means the target's
   * renderer is unresponsive; CDP errors are ignored because some targets
   * (e.g. OOPIFs) do not support every domain.
   */
  private async enableDomains(sessionId: string): Promise<boolean> {
    const start = Date.now();
    for (const method of ['Page.enable', 'DOM.enable', 'Runtime.enable', 'Network.enable']) {
      const remaining = ENABLE_DOMAINS_BUDGET_MS - (Date.now() - start);
      if (remaining <= 0) {
        return false;
      }
      try {
        await this.send(method, undefined, sessionId, Math.min(remaining, ENABLE_DOMAIN_COMMAND_TIMEOUT_MS));
      } catch (err) {
        if (isCdpCommandTimeout(err)) {
          return false;
        }
        // Domain unsupported on this target; ignore.
      }
    }
    return true;
  }

  /**
   * Best-effort dismissal of a JavaScript dialog that was already open when
   * the runtime attached. A modal dialog freezes the renderer main thread
   * and no live execution owns a dialog that predates the attach (the REPL
   * that opened it is gone), so leaving it would brick every session-routed
   * command until the caller recovers manually. Chromium re-emits
   * Page.javascriptDialogOpening on Page.enable in many versions (captured
   * into pendingDialog); when the renderer was unresponsive during domain
   * enable we also probe unconditionally, since Page.handleJavaScriptDialog
   * is answered browser-side and simply errors when no dialog is showing.
   */
  private async dismissStaleDialog(): Promise<void> {
    if (!this.sessionId) return;
    if (this.rendererResponsive && !this.pendingDialog) return;
    let dismissed: PendingDialog | null = null;
    try {
      await this.send(
        'Page.handleJavaScriptDialog',
        { accept: true },
        this.sessionId,
        DIALOG_DISMISS_TIMEOUT_MS,
      );
      dismissed = this.pendingDialog ?? { type: 'unknown', message: '', since: Date.now() };
    } catch {
      // No dialog is showing (or the command could not be answered in
      // time). Leave pendingDialog untouched so page_info still reports a
      // detected dialog and the caller can retry the dismissal explicitly.
    }
    if (dismissed) {
      this.pendingDialog = null;
      this.onDialogAutoDismissed?.(dismissed);
      if (!this.rendererResponsive) {
        // The dismissed dialog may have been what froze the renderer.
        this.rendererResponsive = await this.enableDomains(this.sessionId);
      }
    }
  }

  /**
   * Ensure a page session is attached. Prefers the existing target, then any
   * non-internal page, then any page; creates a new tab if no page exists.
   */
  async ensureAttached(): Promise<void> {
    await this.ensureConnected();
    if (this.sessionId && this.targetId) {
      return;
    }
    // A target can be destroyed between listing and attaching (target swap
    // during navigation); re-list and retry once instead of surfacing the
    // raw CDP error to the caller.
    for (let attempt = 0; attempt < 2; attempt++) {
      const targets = await this.listTargets();
      const pages = targets.filter((t) => t.type === 'page');
      const pick =
        pages.find((t) => t.targetId === this.targetId) ??
        pages.find((t) => !isInternalUrl(t.url)) ??
        pages[0];
      if (!pick) {
        break;
      }
      try {
        await this.attach(pick.targetId);
        return;
      } catch (err: any) {
        if (attempt === 0 && String(err?.message ?? err).includes('No target with given id found')) {
          continue;
        }
        throw err;
      }
    }
    const created = await this.browserCommand<{ targetId: string }>('Target.createTarget', {
      url: 'about:blank',
    });
    await this.attach(created.targetId);
  }

  /**
   * Best-effort wait until a target's URL has committed away from
   * about:blank (or the target disappeared). Used by new_tab so immediate
   * follow-up calls observe the requested URL. Never throws.
   */
  async waitForNavigationCommit(targetId: string, timeoutMs: number): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      try {
        const targets = await this.listTargets();
        const target = targets.find((t) => t.targetId === targetId);
        if (!target || (target.url !== '' && target.url !== 'about:blank')) {
          return;
        }
      } catch {
        return;
      }
      if (Date.now() > deadline) {
        return;
      }
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
  }

  /**
   * Best-effort wait until a target no longer appears in the target list.
   * Used by close_tab so immediate follow-up calls observe the closure.
   * Never throws.
   */
  async waitForTargetGone(targetId: string, timeoutMs: number): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      try {
        const targets = await this.listTargets();
        if (!targets.some((t) => t.targetId === targetId)) {
          return;
        }
      } catch {
        return;
      }
      if (Date.now() > deadline) {
        return;
      }
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
  }

  /**
   * Ensure the attached target is a non-internal page, switching away from
   * internal pages (e.g. chrome://newtab) if necessary.
   */
  async ensureRealTab(): Promise<CdpTarget> {
    await this.ensureAttached();
    const targets = await this.listTargets();
    const current = targets.find((t) => t.targetId === this.targetId);
    if (current && current.type === 'page' && !isInternalUrl(current.url)) {
      return current;
    }
    const real = targets.find((t) => t.type === 'page' && !isInternalUrl(t.url));
    if (real) {
      await this.attach(real.targetId);
      return real;
    }
    const created = await this.browserCommand<{ targetId: string }>('Target.createTarget', {
      url: 'about:blank',
    });
    await this.attach(created.targetId);
    const after = await this.listTargets();
    return (
      after.find((t) => t.targetId === created.targetId) ?? {
        targetId: created.targetId,
        type: 'page',
        title: '',
        url: 'about:blank',
        attached: true,
      }
    );
  }

  /** Evaluate an expression against an arbitrary target (e.g. an OOPIF). */
  async evaluateOnTarget<T = any>(targetId: string, expression: string): Promise<T> {
    await this.ensureConnected();
    const res = await this.browserCommand<{ sessionId: string }>('Target.attachToTarget', {
      targetId,
      flatten: true,
    });
    const sessionId = res.sessionId;
    try {
      const evalRes = await this.send<any>(
        'Runtime.evaluate',
        { expression, awaitPromise: true, returnByValue: true },
        sessionId,
      );
      if (evalRes.exceptionDetails) {
        const desc =
          evalRes.exceptionDetails.exception?.description ??
          evalRes.exceptionDetails.text ??
          'evaluation failed';
        throw new Error(desc);
      }
      return evalRes.result?.value as T;
    } finally {
      try {
        await this.browserCommand('Target.detachFromTarget', { sessionId });
      } catch {
        // ignore
      }
    }
  }

  /** Return and clear buffered events for the attached session. */
  drainEvents(): CdpEvent[] {
    const mine: CdpEvent[] = [];
    const rest: CdpEvent[] = [];
    for (const ev of this.events) {
      if (ev.sessionId && ev.sessionId === this.sessionId) {
        mine.push(ev);
      } else {
        rest.push(ev);
      }
    }
    this.events = rest;
    return mine;
  }

  /** Network idle state for wait_for_network_idle. */
  networkIdleState(): { inFlight: number; lastActivity: number } {
    return { inFlight: this.inFlightRequests.size, lastActivity: this.lastNetworkActivity };
  }

  /** Close the browser connection. Session state is cleared. */
  close(): void {
    if (this.ws) {
      try {
        this.ws.close();
      } catch {
        // ignore
      }
    }
    this.onClose(this.ws as WebSocket);
    this.ws = null;
  }
}
