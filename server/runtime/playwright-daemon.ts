/**
 * Persistent Playwright Executor Daemon
 *
 * Listens on a Unix socket for code execution requests, maintains a warm CDP
 * connection to the browser, and uses esbuild for TypeScript transformation.
 *
 * Protocol (newline-delimited JSON):
 * Request:  { "id": string, "code": string, "timeout_ms"?: number, "executor"?: string, "target_id"?: string }
 * Response: { "id": string, "success": boolean, "result"?: any, "error"?: string, "stack"?: string,
 *             "target_id"?: string, "tab_created"?: boolean, "timed_out"?: boolean }
 *
 * When "executor" is set, `page` is bound to the executor's own tab: the page
 * whose CDP target ID is "target_id", or a new background tab when there is
 * none. The response reports the tab's target ID so the API can pass it back
 * on the executor's next call, including after this process is replaced. A new
 * tab is also reported as soon as it exists, before user code runs, with a
 * preliminary line: { "id": string, "tab": { "target_id": string, "tab_created": true } }
 */

import { createServer, Socket } from 'net';
import { unlinkSync, existsSync } from 'fs';
import type { Browser, CDPSession, Page } from 'playwright-core';

import { PageTargetIdCache } from './page-target-id-cache';
import { createWebMCPClient } from './webmcp';

const SOCKET_PATH = process.env.PLAYWRIGHT_DAEMON_SOCKET || '/tmp/playwright-daemon.sock';
const CDP_ENDPOINT = process.env.CDP_ENDPOINT || 'ws://127.0.0.1:9222';
const KERNEL_API_ENDPOINT =
  process.env.KERNEL_API_ENDPOINT || `http://127.0.0.1:${process.env.PORT || '10001'}`;
const USE_PATCHRIGHT = process.env.PLAYWRIGHT_ENGINE !== 'playwright-core';
const RECONNECT_DELAY_MS = 1000;
const MAX_RECONNECT_ATTEMPTS = 10;

let browser: Browser | null = null;
let connecting = false;
let reconnectAttempts = 0;

const pageTargetIdCache = new PageTargetIdCache<Page>(async page => {
  const session = await page.context().newCDPSession(page);
  try {
    const { targetInfo } = await session.send('Target.getTargetInfo');
    return targetInfo.targetId;
  } finally {
    await session.detach().catch(() => {});
  }
});

interface ExecuteRequest {
  id: string;
  code: string;
  timeout_ms?: number;
  executor?: string;
  target_id?: string;
}

interface ExecuteResponse {
  id: string;
  success: boolean;
  result?: unknown;
  error?: string;
  stack?: string;
  target_id?: string;
  tab_created?: boolean;
  timed_out?: boolean;
}

// The executor tab bound by a call. It is recorded as soon as it is known so a
// timed-out call still reports it.
interface ExecutorTab {
  targetId?: string;
  created?: boolean;
}

function withTimeout<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) {
    return Promise.reject(signal.reason);
  }

  return Promise.race([
    promise,
    new Promise<never>((_, reject) => {
      signal.addEventListener('abort', () => reject(signal.reason), { once: true });
    }),
  ]);
}

async function transformCode(code: string): Promise<string> {
  // Wrap in async function so top-level await/return are valid for esbuild
  const wrapped = `async function __userCode__() {\n${code}\n}`;

  const { transform } = await import('esbuild');
  const result = await transform(wrapped, {
    loader: 'ts',
    target: 'es2022',
  });

  // Extract the function body
  const transformed = result.code;
  const bodyStart = transformed.indexOf('{') + 1;
  const bodyEnd = transformed.lastIndexOf('}');

  if (bodyStart <= 0 || bodyEnd <= bodyStart) {
    return code;
  }

  return transformed.slice(bodyStart, bodyEnd).trim();
}

async function disconnectBrowser(): Promise<void> {
  const connectedBrowser = browser;
  browser = null;
  pageTargetIdCache.reset();
  if (!connectedBrowser) return;

  try {
    await connectedBrowser.close();
  } catch {
    // The timeout may have already torn down the transport.
  }
}

async function ensureBrowserConnection(): Promise<Browser> {
  if (browser && browser.isConnected()) {
    return browser;
  }

  if (connecting) {
    while (connecting) {
      await new Promise(resolve => setTimeout(resolve, 50));
    }
    if (browser && browser.isConnected()) {
      return browser;
    }
  }

  connecting = true;
  try {
    // Load the engine after socket binding, outside the API's short
    // socket-ready deadline. Only initialize the selected engine.
    const { chromium } = USE_PATCHRIGHT
      ? await import('patchright')
      : await import('playwright-core');

    if (browser) {
      try {
        await browser.close();
      } catch {
        // Ignore
      }
      browser = null;
      pageTargetIdCache.reset();
    }

    console.error(`[playwright-daemon] Connecting to CDP: ${CDP_ENDPOINT}`);
    const connectedBrowser = await chromium.connectOverCDP(CDP_ENDPOINT);
    browser = connectedBrowser;
    reconnectAttempts = 0;

    connectedBrowser.on('disconnected', () => {
      console.error('[playwright-daemon] Browser disconnected');
      if (browser === connectedBrowser) {
        browser = null;
        pageTargetIdCache.reset();
      }
    });

    console.error('[playwright-daemon] CDP connection established');
    return connectedBrowser;
  } finally {
    connecting = false;
  }
}

async function activeTabTargetIds(root: CDPSession): Promise<string[]> {
  const { targetInfos } = await root.send('Target.getTargets', {
    filter: [{ type: 'tab', exclude: false }, { exclude: true }],
  });

  return targetInfos
    .filter(target => (target.embedderData as any)?.tabActive === true)
    .map(target => target.targetId);
}

async function pageForTabTarget(
  root: CDPSession,
  targetId: string,
  pageByTargetId: Map<string, Page>,
): Promise<Page | null> {
  // The listener is scoped to this call so page targets attached for one tab
  // never leak into another tab's candidate set.
  const relatedPageIds = new Set<string>();
  const collectRelatedPage = (event: { targetInfo: { type: string; subtype?: string; targetId: string } }) => {
    if (event.targetInfo.type === 'page' && !event.targetInfo.subtype) {
      relatedPageIds.add(event.targetInfo.targetId);
    }
  };
  root.on('Target.attachedToTarget', collectRelatedPage);

  try {
    await root.send('Target.autoAttachRelated', {
      targetId,
      waitForDebuggerOnStart: false,
      filter: [{ type: 'page', exclude: false }, { exclude: true }],
    });

    for (const relatedPageId of relatedPageIds) {
      const page = pageByTargetId.get(relatedPageId);
      if (page && !page.isClosed()) return page;
    }

    return null;
  } finally {
    root.off('Target.attachedToTarget', collectRelatedPage);
  }
}

// Chrome reports one active tab per window. Try every reported target against
// every Playwright context's pages.
//
// A pass can legitimately find an active tab with no matching page. The known
// cause is a cross-origin navigation in a freshly opened tab (e.g. a site
// handing the user off to a sibling product on another origin): Chrome swaps
// the tab's page target to a new renderer process and marks the tab active
// before Playwright has attached to the replacement target. That gap is
// transient but has been observed to outlast back-to-back daemon calls, so an
// immediate retry lands inside the same gap; the retries are spaced to give
// Playwright time to attach. The delay is only paid when a pass fails, and
// callers fall back to an open page if every attempt misses.
const ACTIVE_PAGE_RESOLUTION_ATTEMPTS = 3;
const ACTIVE_PAGE_RESOLUTION_RETRY_DELAY_MS = 150;

async function resolveActivePage(browser: Browser): Promise<Page | null> {
  let activeTabIds: string[] = [];

  for (let attempt = 0; attempt < ACTIVE_PAGE_RESOLUTION_ATTEMPTS; attempt++) {
    if (attempt > 0) {
      await new Promise(resolve => setTimeout(resolve, ACTIVE_PAGE_RESOLUTION_RETRY_DELAY_MS));
    }

    try {
      const pages = browser
        .contexts()
        .flatMap(context => context.pages())
        .filter(page => !page.isClosed());
      const pageByTargetId = await pageTargetIdCache.buildPageByTargetId(pages, { refresh: attempt > 0 });

      // One browser-level session serves the whole attempt: the active-tab
      // listing and every tab-to-page join. Detaching it also cleans up the
      // page sessions auto-attached by pageForTabTarget.
      const root = await browser.newBrowserCDPSession();
      try {
        activeTabIds = await activeTabTargetIds(root);
        for (const targetId of activeTabIds) {
          try {
            const page = await pageForTabTarget(root, targetId, pageByTargetId);
            if (page) return page;
          } catch {
            // This tab may have changed while it was inspected. Keep trying the
            // other active tabs reported by the same browser snapshot.
          }
        }
      } finally {
        await root.detach().catch(() => {});
      }

      // No active tab matched this page snapshot. Retry with fresh snapshots.
    } catch {
      // The browser-wide lookup raced with a target change. Discard this pass;
      // the next attempt, if any, snapshots both pages and active tabs again.
    }
  }

  console.error(
    `[playwright-daemon] active-tab resolution failed after ${ACTIVE_PAGE_RESOLUTION_ATTEMPTS} attempts; ` +
      `falling back (unmatched active tabs: ${activeTabIds.join(', ') || 'none reported'})`,
  );
  return null;
}

const OWNED_PAGE_RESOLUTION_ATTEMPTS = 3;
const OWNED_PAGE_RESOLUTION_RETRY_DELAY_MS = 150;

async function findPage(browser: Browser, targetId: string, refresh: boolean): Promise<Page | undefined> {
  const pages = browser
    .contexts()
    .flatMap(context => context.pages())
    .filter(page => !page.isClosed());
  return (await pageTargetIdCache.buildPageByTargetId(pages, { refresh })).get(targetId);
}

async function targetExists(browser: Browser, targetId: string): Promise<boolean> {
  const root = await browser.newBrowserCDPSession();
  try {
    await root.send('Target.getTargetInfo', { targetId });
    return true;
  } catch {
    return false;
  } finally {
    await root.detach().catch(() => {});
  }
}

const NEW_TAB_ATTACH_ATTEMPTS = 50;
const NEW_TAB_ATTACH_RETRY_DELAY_MS = 100;

// Binds an executor's `page` to the tab it owns. A tab that still exists but
// has no Playwright page yet (e.g. this connection is still attaching to it)
// is an error rather than a reason to open a duplicate tab.
async function resolveExecutorPage(
  browser: Browser,
  tab: ExecutorTab,
  onTabCreated: (targetId: string) => void,
  targetId?: string,
): Promise<Page> {
  if (targetId) {
    for (let attempt = 0; attempt < OWNED_PAGE_RESOLUTION_ATTEMPTS; attempt++) {
      if (attempt > 0) {
        await new Promise(resolve => setTimeout(resolve, OWNED_PAGE_RESOLUTION_RETRY_DELAY_MS));
      }
      const page = await findPage(browser, targetId, attempt > 0);
      if (page) {
        tab.targetId = targetId;
        tab.created = false;
        return page;
      }
      if (!(await targetExists(browser, targetId))) break;
      if (attempt === OWNED_PAGE_RESOLUTION_ATTEMPTS - 1) {
        throw new Error(`executor tab ${targetId} is open but not available to Playwright yet; retry the call`);
      }
    }
  }

  // Open the tab in the background so it does not take focus from the live
  // view or change the active tab that calls without an executor bind to.
  // Without a browserContextId the tab lands in the default context.
  const root = await browser.newBrowserCDPSession();
  let newTargetId: string;
  try {
    ({ targetId: newTargetId } = await root.send('Target.createTarget', { url: 'about:blank', background: true }));
  } finally {
    await root.detach().catch(() => {});
  }
  tab.targetId = newTargetId;
  tab.created = true;
  onTabCreated(newTargetId);

  for (let attempt = 0; attempt < NEW_TAB_ATTACH_ATTEMPTS; attempt++) {
    const page = await findPage(browser, newTargetId, attempt > 0);
    if (page) return page;
    await new Promise(resolve => setTimeout(resolve, NEW_TAB_ATTACH_RETRY_DELAY_MS));
  }
  throw new Error(`executor tab ${newTargetId} was opened but is not available to Playwright yet; retry the call`);
}

async function executeCode(
  request: ExecuteRequest,
  signal: AbortSignal,
  tab: ExecutorTab,
  onTabCreated: (targetId: string) => void,
): Promise<ExecuteResponse> {
  const { id, code } = request;

  try {
    let jsCode: string;
    try {
      jsCode = await transformCode(code);
    } catch (transformError: any) {
      return {
        id,
        success: false,
        error: `TypeScript transform error: ${transformError.message}`,
        stack: transformError.stack,
      };
    }
    let browserInstance: Browser;
    try {
      browserInstance = await ensureBrowserConnection();
    } catch (connError: any) {
      reconnectAttempts++;
      if (reconnectAttempts >= MAX_RECONNECT_ATTEMPTS) {
        return {
          id,
          success: false,
          error: `Failed to connect to browser after ${MAX_RECONNECT_ATTEMPTS} attempts: ${connError.message}`,
        };
      }
      await new Promise(resolve => setTimeout(resolve, RECONNECT_DELAY_MS));
      try {
        browserInstance = await ensureBrowserConnection();
      } catch (retryError: any) {
        return {
          id,
          success: false,
          error: `Failed to connect to browser: ${retryError.message}`,
        };
      }
    }
    let page: Page;
    if (request.executor) {
      page = await resolveExecutorPage(browserInstance, tab, onTabCreated, request.target_id);
    } else {
      const contexts = browserInstance.contexts();
      const defaultContext = contexts.length > 0 ? contexts[0] : await browserInstance.newContext();
      const pages = contexts.flatMap(context => context.pages());
      // Bind `page` to the actual foreground tab (see resolveActivePage). Using
      // pages[0] bound `page` to the oldest tab regardless of which was active, so
      // calls like page.pdf() operated on the wrong tab whenever more than one was
      // open.
      page =
        (pages.length > 0 ? await resolveActivePage(browserInstance) : null) ??
        pages.findLast(candidate => !candidate.isClosed()) ??
        (await defaultContext.newPage());
    }
    const context = page.context();

    const webmcp = createWebMCPClient({apiBaseUrl: KERNEL_API_ENDPOINT, signal});
    const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
    const createUserFunction = new AsyncFunction(
      'webmcp',
      `return async function(page, context, browser) {\n${jsCode}\n};`,
    );
    const userFunction = await createUserFunction(webmcp);
    signal.throwIfAborted();

    const result = await userFunction(page, context, browserInstance);

    return {
      id,
      success: true,
      result: result !== undefined ? result : null,
    };
  } catch (error: any) {
    return {
      id,
      success: false,
      error: error.message,
      stack: error.stack,
    };
  }
}

function handleConnection(socket: Socket): void {
  let buffer = '';

  socket.on('data', async (data) => {
    buffer += data.toString();

    let newlineIndex: number;
    while ((newlineIndex = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, newlineIndex);
      buffer = buffer.slice(newlineIndex + 1);

      if (!line.trim()) continue;

      let request: ExecuteRequest;
      try {
        request = JSON.parse(line);
      } catch {
        socket.write(JSON.stringify({ id: 'unknown', success: false, error: 'Invalid JSON request' }) + '\n');
        continue;
      }

      if (!request.id || typeof request.code !== 'string') {
        socket.write(JSON.stringify({ id: request.id || 'unknown', success: false, error: 'Invalid request: missing id or code' }) + '\n');
        continue;
      }

      const signal = AbortSignal.timeout(request.timeout_ms ?? 60000);
      const tab: ExecutorTab = {};
      let response: ExecuteResponse;
      try {
        const onTabCreated = (targetId: string) => {
          socket.write(JSON.stringify({ id: request.id, tab: { target_id: targetId, tab_created: true } }) + '\n');
        };
        response = await withTimeout(executeCode(request, signal, tab, onTabCreated), signal);
      } catch (error: any) {
        if (signal.aborted) {
          await disconnectBrowser();
        }
        response = {
          id: request.id,
          success: false,
          error: error.message,
          stack: error.stack,
          ...(signal.aborted && { timed_out: true }),
        };
      }
      if (request.executor && tab.targetId) {
        response.target_id = tab.targetId;
        response.tab_created = tab.created;
      }
      socket.write(JSON.stringify(response) + '\n');
    }
  });

  socket.on('error', (err) => {
    console.error('[playwright-daemon] Socket error:', err.message);
  });
}

async function shutdown(signal: string): Promise<void> {
  console.error(`[playwright-daemon] Received ${signal}, shutting down...`);

  if (browser) {
    try {
      await browser.close();
    } catch {
      // Ignore
    }
  }

  try {
    if (existsSync(SOCKET_PATH)) {
      unlinkSync(SOCKET_PATH);
    }
  } catch {
    // Ignore
  }

  process.exit(0);
}

async function main(): Promise<void> {
  try {
    if (existsSync(SOCKET_PATH)) {
      unlinkSync(SOCKET_PATH);
    }
  } catch {
    // Ignore
  }

  process.on('SIGTERM', () => shutdown('SIGTERM'));
  process.on('SIGINT', () => shutdown('SIGINT'));

  const server = createServer(handleConnection);

  server.on('error', (err) => {
    console.error('[playwright-daemon] Server error:', err);
    process.exit(1);
  });

  server.listen(SOCKET_PATH, () => {
    console.error(`[playwright-daemon] Listening on ${SOCKET_PATH}`);
    ensureBrowserConnection().catch((err) => {
      console.error('[playwright-daemon] Initial connection failed:', err.message);
    });
  });
}

main().catch((err) => {
  console.error('[playwright-daemon] Fatal error:', err);
  process.exit(1);
});
