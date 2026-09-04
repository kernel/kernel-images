// Persistent, unrestricted JavaScript daemon owned by the API process.
// Protocol and lifecycle invariants are documented in plans/persistent-browser-repl.md.

import { AsyncLocalStorage } from 'async_hooks';
import { createServer, Socket } from 'net';
import { unlinkSync, existsSync, promises as fsp } from 'fs';
import vm from 'vm';
import util from 'util';
import { CdpClient } from './browser-cdp-client';
import { BrowserHelpers, buildBrowserGlobals } from './browser-helpers';
import { CellRuntime } from './cell-runtime';
import { createWebMCPClient } from './webmcp';

const SOCKET_PATH = process.env.BROWSER_REPL_SOCKET || '/tmp/browser-repl.sock';
const REPL_ID = process.env.BROWSER_REPL_ID || 'unknown';
const CDP_ENDPOINT = process.env.CDP_ENDPOINT || 'ws://127.0.0.1:9222';
const KERNEL_API_ENDPOINT =
  process.env.KERNEL_API_ENDPOINT || `http://127.0.0.1:${process.env.PORT || '10001'}`;

// Output limits (decoded bytes unless noted).
const MAX_TEXT_BYTES = 256 * 1024; // combined text per response
const MAX_IMAGE_BYTES = 8 * 1024 * 1024; // per emitted image
const MAX_TOTAL_IMAGE_BYTES = 16 * 1024 * 1024; // aggregate image data per response
const MAX_REQUEST_BYTES = 8 * 1024 * 1024; // incoming request line
const MAX_STRAY_ITEMS = 1000; // buffered output produced outside an execution

// Private references retained before any user code runs so global/prototype
// modification inside the context cannot corrupt protocol framing or result
// serialization.
const safeStringify = JSON.stringify;
const safeInspect = util.inspect;
const safeFormat = util.format;

// Content collection

type TextChannel = 'write' | 'stdout' | 'stderr';

interface TextItem {
  type: 'text';
  channel: TextChannel;
  text: string;
}

interface ImageItem {
  type: 'image';
  mime_type: string;
  data_b64: string;
}

type ContentItem = TextItem | ImageItem;

class Collector {
  items: ContentItem[] = [];
  truncated = false;
  private textBytes = 0;
  private imageBytes = 0;

  constructor(private readonly maxItems?: number) {}

  addText(channel: TextChannel, text: string): void {
    const bytes = Buffer.byteLength(text);
    if (this.textBytes + bytes > MAX_TEXT_BYTES) {
      const remaining = MAX_TEXT_BYTES - this.textBytes;
      if (remaining > 0) {
        this.items.push({
          type: 'text',
          channel,
          text: Buffer.from(text, 'utf8').subarray(0, remaining).toString('utf8'),
        });
        this.textBytes = MAX_TEXT_BYTES;
        this.enforceItemLimit();
      }
      this.truncated = true;
      return;
    }
    this.textBytes += bytes;
    this.items.push({ type: 'text', channel, text });
    this.enforceItemLimit();
  }

  addImage(mimeType: string, bytes: Buffer): boolean {
    if (this.imageBytes + bytes.length > MAX_TOTAL_IMAGE_BYTES) {
      this.truncated = true;
      return false;
    }
    this.imageBytes += bytes.length;
    this.items.push({ type: 'image', mime_type: mimeType, data_b64: bytes.toString('base64') });
    this.enforceItemLimit();
    return true;
  }

  adopt(item: ContentItem): void {
    if (item.type === 'text') {
      this.addText(item.channel, item.text);
    } else {
      this.addImage(item.mime_type, Buffer.from(item.data_b64, 'base64'));
    }
  }

  drainInto(target: Collector): void {
    for (const item of this.items) target.adopt(item);
    target.truncated ||= this.truncated;
  }

  private enforceItemLimit(): void {
    if (this.maxItems === undefined) return;
    while (this.items.length > this.maxItems) {
      const removed = this.items.shift();
      if (!removed) return;
      this.truncated = true;
      if (removed.type === 'text') this.textBytes -= Buffer.byteLength(removed.text);
      else this.imageBytes -= Buffer.from(removed.data_b64, 'base64').length;
    }
  }
}

let activeCollector: Collector | null = null;
let strayCollector = new Collector(MAX_STRAY_ITEMS);

function currentCollector(): Collector {
  return activeCollector ?? strayCollector;
}

function boundedInspect(value: unknown): string {
  return safeInspect(value, {
    depth: 4,
    maxArrayLength: 100,
    maxStringLength: 8192,
    breakLength: 120,
    compact: true,
  });
}

function writeOutput(channel: TextChannel, text: string): void {
  currentCollector().addText(channel, text);
}

// repl namespace + console capture

const IMAGE_MAGIC: Array<{ mime: string; matches: (b: Buffer) => boolean }> = [
  { mime: 'image/png', matches: (b) => b.length > 8 && b.readUInt32BE(0) === 0x89504e47 },
  { mime: 'image/jpeg', matches: (b) => b.length > 3 && b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff },
  {
    mime: 'image/webp',
    matches: (b) => b.length > 12 && b.subarray(0, 4).toString('ascii') === 'RIFF' && b.subarray(8, 12).toString('ascii') === 'WEBP',
  },
];

function sniffImageMime(bytes: Buffer): string | null {
  for (const candidate of IMAGE_MAGIC) {
    if (candidate.matches(bytes)) return candidate.mime;
  }
  return null;
}

function isImageMime(mime: unknown): mime is string {
  return typeof mime === 'string' && /^image\//.test(mime);
}

// ArrayBuffer slot checks work across the VM and daemon realms.
function bytesToBuffer(raw: unknown): Buffer {
  if (Buffer.isBuffer(raw)) {
    return raw;
  }
  if (ArrayBuffer.isView(raw)) {
    return Buffer.from(raw.buffer, raw.byteOffset, raw.byteLength);
  }
  if (util.types.isArrayBuffer(raw)) {
    return Buffer.from(new Uint8Array(raw));
  }
  throw new Error('repl.emitImage: bytes must be a Buffer, Uint8Array, or ArrayBuffer');
}

async function normalizeImageInput(input: unknown): Promise<{ bytes: Buffer; mime: string }> {
  if (typeof input === 'string') {
    const match = /^data:([^;,]+);base64,(.*)$/s.exec(input);
    if (!match) {
      throw new Error('repl.emitImage: string input must be an image/* data URL');
    }
    const mime = match[1];
    if (!isImageMime(mime)) {
      throw new Error(`repl.emitImage: data URL MIME type must be image/*, got ${mime}`);
    }
    return { bytes: Buffer.from(match[2], 'base64'), mime };
  }

  if (Buffer.isBuffer(input) || ArrayBuffer.isView(input) || util.types.isArrayBuffer(input)) {
    const bytes = bytesToBuffer(input);
    const mime = sniffImageMime(bytes);
    if (!mime) throw new Error('repl.emitImage: unrecognized image data (expected PNG, JPEG, or WebP)');
    return { bytes, mime };
  }

  if (input && typeof input === 'object') {
    const obj = input as Record<string, unknown>;
    const explicitMime = obj.mimeType ?? (obj as any).mime_type;
    if (explicitMime !== undefined && !isImageMime(explicitMime)) {
      throw new Error(`repl.emitImage: MIME type must be image/*, got ${String(explicitMime)}`);
    }
    if (obj.bytes !== undefined) {
      const bytes = bytesToBuffer(obj.bytes);
      const mime = (explicitMime as string | undefined) ?? sniffImageMime(bytes);
      if (!mime) throw new Error('repl.emitImage: unrecognized image data (expected PNG, JPEG, or WebP)');
      return { bytes, mime };
    }
    if (typeof obj.path === 'string') {
      const bytes = await fsp.readFile(obj.path);
      const mime = (explicitMime as string | undefined) ?? sniffImageMime(bytes);
      if (!mime) {
        throw new Error(`repl.emitImage: ${obj.path} is not a recognized image (expected PNG, JPEG, or WebP)`);
      }
      return { bytes, mime };
    }
  }

  throw new Error(
    'repl.emitImage: unsupported input (expected a data URL, Buffer, Uint8Array, ArrayBuffer, { bytes, mimeType? }, or { path, mimeType? })',
  );
}

const repl = Object.freeze({
  id: REPL_ID,
  write(value: unknown): void {
    writeOutput('write', typeof value === 'string' ? value : boundedInspect(value));
  },
  async emitImage(input: unknown): Promise<void> {
    const { bytes, mime } = await normalizeImageInput(input);
    if (bytes.length > MAX_IMAGE_BYTES) {
      throw new Error(
        `repl.emitImage: image is ${bytes.length} bytes, exceeding the ${MAX_IMAGE_BYTES} byte per-image limit`,
      );
    }
    const added = currentCollector().addImage(mime, bytes);
    if (!added) {
      writeOutput(
        'stderr',
        `repl.emitImage: dropped a ${bytes.length} byte image; aggregate response image limit reached`,
      );
    }
  },
});

const consoleCapture = {
  log: (...args: unknown[]) => writeOutput('stdout', safeFormat(...args)),
  info: (...args: unknown[]) => writeOutput('stdout', safeFormat(...args)),
  debug: (...args: unknown[]) => writeOutput('stdout', safeFormat(...args)),
  warn: (...args: unknown[]) => writeOutput('stderr', safeFormat(...args)),
  error: (...args: unknown[]) => writeOutput('stderr', safeFormat(...args)),
  dir: (...args: unknown[]) => writeOutput('stdout', safeFormat(...args)),
  trace: (...args: unknown[]) => writeOutput('stderr', safeFormat(...args)),
  table: (...args: unknown[]) => writeOutput('stdout', safeFormat(...args)),
};

// Persistent evaluation context

const cdpClient = new CdpClient(CDP_ENDPOINT);
const helpers = new BrowserHelpers(cdpClient);
const webmcpExecution = new AsyncLocalStorage<AbortSignal>();
const webmcp = createWebMCPClient({
  apiBaseUrl: KERNEL_API_ENDPOINT,
  signalProvider: () => {
    const signal = webmcpExecution.getStore();
    if (!signal) {
      throw new Error('webmcp calls require an active Browser REPL execution');
    }
    return signal;
  },
});
const browserGlobals = buildBrowserGlobals(helpers);
const browserNamespace = Object.freeze({
  ...(browserGlobals.browser as Record<string, unknown>),
  webmcp,
});

// Operational notes from helpers (e.g. a fallback activating) surface as
// stderr content items in the active (or next) execution.
helpers.onLog = (message) => writeOutput('stderr', `browser-repl: ${message}`);

// A dialog dismissed at attach time was left open before the runtime
// attached (typically by a previous REPL that was killed); surface the
// automatic dismissal so it is visible in the execution's output.
cdpClient.onDialogAutoDismissed = (dialog) => {
  const detail = dialog.message ? `, message: ${JSON.stringify(dialog.message)}` : '';
  writeOutput(
    'stderr',
    `browser-repl: dismissed a pre-existing JavaScript dialog (type: ${dialog.type}${detail}) left open before attach`,
  );
};

// Cross-realm global handle captured right after context creation; cell
// bindings are exposed here after each SourceTextModule evaluation.
let contextGlobal: Record<PropertyKey, unknown>;

const context: vm.Context = vm.createContext(
  {
    console: consoleCapture,
    repl,
    ...browserGlobals,
    browser: browserNamespace,
    webmcp,
    // Node conveniences. This endpoint is unrestricted code execution; the
    // context is a state container, not a sandbox.
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    queueMicrotask,
    Buffer,
    process,
    fetch,
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    AbortController,
    AbortSignal,
    structuredClone,
    atob,
    btoa,
    crypto,
  },
  { name: `browser-repl-${REPL_ID}` },
);

contextGlobal = vm.runInContext('globalThis', context) as Record<PropertyKey, unknown>;

const cellRuntime = new CellRuntime(context, contextGlobal);

async function evaluate(code: string): Promise<void> {
  await cellRuntime.evaluate(code);
}

// Request handling

interface ExecuteRequest {
  id: string;
  code: string;
  timeout_ms?: number;
}

interface ExecuteResponse {
  id: string;
  repl_id: string;
  success: boolean;
  error?: string;
  stack?: string;
  content: ContentItem[];
  content_truncated: boolean;
  timed_out?: boolean;
  exiting?: boolean;
  duration_ms: number;
}

async function executeRequest(
  request: ExecuteRequest,
  respond: (response: ExecuteResponse) => void,
): Promise<ExecuteResponse> {
  const start = Date.now();
  const collector = new Collector();
  // Track the in-flight execution so the uncaughtException handler can
  // answer it with a deterministic failure (including partial content)
  // before exiting, instead of leaving the caller with a bare EOF.
  activeExecution = { request, collector, respond, start };

  // Swap the buffer before adopting it so output produced after this point
  // belongs to the next execution, never to a stale drained collector. The
  // collector owns its counters and truncation bit, so draining cannot leave
  // cumulative limits behind or hide dropped output.
  const drainedStray = strayCollector;
  strayCollector = new Collector(MAX_STRAY_ITEMS);
  drainedStray.drainInto(collector);

  activeCollector = collector;
  const executionAbortController = new AbortController();
  const timeoutMs = request.timeout_ms ?? 60_000;
  // Let wait-style helpers and the CDP client clamp their internal
  // deadlines to just below this execution's deadline, so a routine helper
  // timeout (or a renderer frozen behind a modal dialog) surfaces as a
  // clean error instead of tying the destructive execution timeout.
  helpers.executionDeadlineMs = start + timeoutMs;
  cdpClient.executionDeadlineMs = start + timeoutMs;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let timedOut = false;

  try {
    const timeoutPromise = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        timedOut = true;
        const error = new Error(`execution timed out after ${timeoutMs}ms`);
        executionAbortController.abort(error);
        reject(error);
      }, timeoutMs);
      if (typeof timer.unref === 'function') timer.unref();
    });
    const evaluation = webmcpExecution.run(
      executionAbortController.signal,
      () => evaluate(request.code),
    );
    await Promise.race([evaluation, timeoutPromise]);
    return {
      id: request.id,
      repl_id: REPL_ID,
      success: true,
      content: collector.items,
      content_truncated: collector.truncated,
      duration_ms: Date.now() - start,
    };
  } catch (err: any) {
    return {
      id: request.id,
      repl_id: REPL_ID,
      success: false,
      error: String(err?.message ?? err),
      stack: typeof err?.stack === 'string' ? err.stack : undefined,
      content: collector.items,
      content_truncated: collector.truncated,
      // A timed-out execution is merely abandoned, not interrupted: its code
      // is still running. The API parent must kill this process (it does,
      // destructively, per the spec's timeout semantics) before serving
      // another execution.
      timed_out: timedOut || undefined,
      duration_ms: Date.now() - start,
    };
  } finally {
    if (timer) clearTimeout(timer);
    if (!executionAbortController.signal.aborted) {
      executionAbortController.abort(new Error('Browser REPL execution finished'));
    }
    helpers.executionDeadlineMs = null;
    cdpClient.executionDeadlineMs = null;
    activeCollector = null;
    activeExecution = null;
  }
}

// Serialize executions as defense in depth; the Go handler already holds a
// mutex, but the daemon must never interleave two executions.
let executionChain: Promise<void> = Promise.resolve();

// The execution currently running, so the uncaughtException handler can
// deliver a deterministic failure response (with partial content) before
// exiting instead of leaving the caller with a bare EOF.
let activeExecution: {
  request: ExecuteRequest;
  collector: Collector;
  respond: (response: ExecuteResponse) => void;
  start: number;
} | null = null;

// Set once the daemon has decided to exit (uncaughtException): queued
// execution continuations must not write further responses.
let processExiting = false;

function enqueueExecution(request: ExecuteRequest, respond: (response: ExecuteResponse) => void): void {
  executionChain = executionChain.then(async () => {
    let response: ExecuteResponse;
    try {
      response = await executeRequest(request, respond);
    } catch (err: any) {
      response = {
        id: request.id,
        repl_id: REPL_ID,
        success: false,
        error: `internal daemon error: ${String(err?.message ?? err)}`,
        content: [],
        content_truncated: false,
        duration_ms: 0,
      };
    }
    if (!processExiting) {
      respond(response);
    }
  });
}

function handleConnection(socket: Socket): void {
  let buffer = '';
  // The server sets allowHalfOpen, so a client that half-closes (SHUT_WR)
  // after sending its request still receives the execution response. The
  // daemon ends its own side once the client has ended and every queued
  // response has been flushed.
  let clientEnded = false;
  let pendingWrites = 0;
  // Requests accepted but whose response has not been flushed yet. The
  // client's FIN arrives while its execution is still queued, so the
  // socket must stay open until that response is written.
  let pendingRequests = 0;

  const maybeEnd = () => {
    if (clientEnded && pendingWrites === 0 && pendingRequests === 0) {
      socket.end();
    }
  };

  const respond = (response: ExecuteResponse, onFlushed?: () => void) => {
    pendingWrites++;
    try {
      socket.write(safeStringify(response) + '\n', () => {
        pendingWrites--;
        onFlushed?.();
        maybeEnd();
      });
    } catch (err: any) {
      pendingWrites--;
      onFlushed?.();
      process.stderr.write(`[browser-repl] failed to write response: ${err?.message ?? err}\n`);
    }
  };

  const rejectOversized = () => {
    // end() flushes the rejection before closing (unlike destroy()).
    respond({
      id: 'unknown',
      repl_id: REPL_ID,
      success: false,
      error: `request exceeds the ${MAX_REQUEST_BYTES} byte limit`,
      content: [],
      content_truncated: false,
      duration_ms: 0,
    });
    socket.end();
  };

  socket.on('data', (data) => {
    buffer += data.toString();

    let newlineIndex: number;
    while ((newlineIndex = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, newlineIndex);
      buffer = buffer.slice(newlineIndex + 1);
      // The size cap applies per accumulated line, independent of how the
      // request was chunked: a single write containing the newline is
      // rejected exactly like a slow flood that never sends one.
      if (Buffer.byteLength(line) > MAX_REQUEST_BYTES) {
        rejectOversized();
        return;
      }
      if (!line.trim()) continue;

      let request: ExecuteRequest;
      try {
        request = JSON.parse(line);
      } catch {
        respond({
          id: 'unknown',
          repl_id: REPL_ID,
          success: false,
          error: 'invalid JSON request',
          content: [],
          content_truncated: false,
          duration_ms: 0,
        });
        continue;
      }

      if (!request.id || typeof request.code !== 'string') {
        respond({
          id: (request as any)?.id || 'unknown',
          repl_id: REPL_ID,
          success: false,
          error: 'invalid request: missing id or code',
          content: [],
          content_truncated: false,
          duration_ms: 0,
        });
        continue;
      }

      pendingRequests++;
      enqueueExecution(request, (response) => respond(response, () => pendingRequests--));
    }

    if (Buffer.byteLength(buffer) > MAX_REQUEST_BYTES) {
      rejectOversized();
      return;
    }
  });

  socket.on('end', () => {
    clientEnded = true;
    maybeEnd();
  });

  socket.on('error', (err) => {
    process.stderr.write(`[browser-repl] socket error: ${err.message}\n`);
  });
}

// Lifecycle

// Settled rejections are reportable without invalidating process state.
function onUnhandledRejection(reason: unknown): void {
  let detail: string;
  try {
    const stack = (reason as any)?.stack;
    detail = typeof stack === 'string' ? stack : boundedInspect(reason);
  } catch {
    try {
      detail = String(reason);
    } catch {
      detail = '<unprintable rejection reason>';
    }
  }
  writeOutput(
    'stderr',
    'browser-repl: unhandled promise rejection (the REPL survives; only the rejected promise is settled):\n' +
      detail,
  );
}

// Continuing after an uncaught exception is unsafe; preserve evidence and exit.
function onUncaughtException(err: unknown): void {
  processExiting = true;
  const stack = (err as any)?.stack;
  const message = String((err as any)?.message ?? err);
  process.stderr.write(
    `[browser-repl] uncaught exception; terminating deterministically (repl_id=${REPL_ID}): ${
      typeof stack === 'string' ? stack : message
    }\n`,
  );
  const inFlight = activeExecution;
  activeExecution = null;
  if (inFlight) {
    try {
      inFlight.respond({
        id: inFlight.request.id,
        repl_id: REPL_ID,
        success: false,
        error: `uncaught exception in browser REPL process: ${message}`,
        stack: typeof stack === 'string' ? stack : undefined,
        content: inFlight.collector.items,
        content_truncated: inFlight.collector.truncated,
        exiting: true,
        duration_ms: Date.now() - inFlight.start,
      });
    } catch {
      // The socket is gone; the API reports the child exit instead.
    }
  }
  // Give the stderr log and the in-flight response a bounded window to
  // flush, then exit non-zero. The socket server keeps the event loop
  // alive, so the unref'd timer always fires.
  setTimeout(() => process.exit(1), 100).unref();
}

function shutdown(signal: string): void {
  process.stderr.write(`[browser-repl] received ${signal}, shutting down (repl_id=${REPL_ID})\n`);
  try {
    cdpClient.close();
  } catch {
    // ignore
  }
  try {
    if (existsSync(SOCKET_PATH)) {
      unlinkSync(SOCKET_PATH);
    }
  } catch {
    // ignore
  }
  process.exit(0);
}

async function main(): Promise<void> {
  try {
    if (existsSync(SOCKET_PATH)) {
      unlinkSync(SOCKET_PATH);
    }
  } catch {
    // ignore
  }

  process.on('SIGTERM', () => shutdown('SIGTERM'));
  process.on('SIGINT', () => shutdown('SIGINT'));
  process.on('unhandledRejection', onUnhandledRejection);
  process.on('uncaughtException', onUncaughtException);

  // allowHalfOpen: a client that half-closes (SHUT_WR) after sending its
  // request still receives the execution response; handleConnection ends
  // the server side once the final queued response is flushed.
  const server = createServer({ allowHalfOpen: true }, handleConnection);
  server.on('error', (err) => {
    process.stderr.write(`[browser-repl] server error: ${err.message}\n`);
    process.exit(1);
  });

  server.listen(SOCKET_PATH, () => {
    process.stderr.write(`[browser-repl] listening on ${SOCKET_PATH} (repl_id=${REPL_ID})\n`);
  });
}

main().catch((err) => {
  process.stderr.write(`[browser-repl] fatal error: ${err?.stack ?? err}\n`);
  process.exit(1);
});
