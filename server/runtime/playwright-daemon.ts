/**
 * Persistent Playwright Executor Daemon
 *
 * Listens on a Unix socket for code execution requests, maintains a warm CDP
 * connection to the browser, and uses esbuild for TypeScript transformation.
 *
 * Protocol (newline-delimited JSON):
 * Request:  { "id": string, "code": string, "timeout_ms"?: number }
 * Response: { "id": string, "success": boolean, "result"?: any, "error"?: string, "stack"?: string }
 */

import { createServer, Socket } from 'net';
import { unlinkSync, existsSync } from 'fs';
import { transform } from 'esbuild';
import { chromium as chromiumPW, Browser } from 'playwright-core';
import { chromium as chromiumPR } from 'patchright';

const SOCKET_PATH = process.env.PLAYWRIGHT_DAEMON_SOCKET || '/tmp/playwright-daemon.sock';
const CDP_ENDPOINT = process.env.CDP_ENDPOINT || 'ws://127.0.0.1:9222';
const USE_PATCHRIGHT = process.env.PLAYWRIGHT_ENGINE !== 'playwright-core';
const RECONNECT_DELAY_MS = 1000;
const MAX_RECONNECT_ATTEMPTS = 10;

let browser: Browser | null = null;
let connecting = false;
let reconnectAttempts = 0;

interface ExecuteRequest {
  id: string;
  code: string;
  timeout_ms?: number;
}

interface ExecuteResponse {
  id: string;
  success: boolean;
  result?: unknown;
  error?: string;
  stack?: string;
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
    const chromium = USE_PATCHRIGHT ? chromiumPR : chromiumPW;

    if (browser) {
      try {
        await browser.close();
      } catch {
        // Ignore
      }
      browser = null;
    }

    console.error(`[playwright-daemon] Connecting to CDP: ${CDP_ENDPOINT}`);
    const connectedBrowser = await chromium.connectOverCDP(CDP_ENDPOINT);
    browser = connectedBrowser;
    reconnectAttempts = 0;

    connectedBrowser.on('disconnected', () => {
      console.error('[playwright-daemon] Browser disconnected');
      if (browser === connectedBrowser) {
        browser = null;
      }
    });

    console.error('[playwright-daemon] CDP connection established');
    return connectedBrowser;
  } finally {
    connecting = false;
  }
}

async function executeCode(request: ExecuteRequest, signal: AbortSignal): Promise<ExecuteResponse> {
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
    const contexts = browserInstance.contexts();
    const context = contexts.length > 0 ? contexts[0] : await browserInstance.newContext();
    const pages = context.pages();
    const page = pages.length > 0 ? pages[0] : await context.newPage();

    const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
    const userFunction = new AsyncFunction('page', 'context', 'browser', jsCode);
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
      let response: ExecuteResponse;
      try {
        response = await withTimeout(executeCode(request, signal), signal);
      } catch (error: any) {
        if (signal.aborted) {
          await disconnectBrowser();
        }
        response = {
          id: request.id,
          success: false,
          error: error.message,
          stack: error.stack,
        };
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
