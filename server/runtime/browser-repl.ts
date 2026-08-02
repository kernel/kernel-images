/**
 * Persistent Browser REPL Daemon
 *
 * Owned and supervised directly by the kernel-images API process. Listens on
 * a Unix socket for code execution requests and evaluates them in a
 * persistent Node.js `vm` context preloaded with browser-control helpers.
 *
 * IMPORTANT: the vm context is a state container, not a security boundary.
 * User code is unrestricted (Node built-ins, filesystem, network, processes).
 *
 * Protocol (newline-delimited JSON, one connection per request):
 * Request:  { "id": string, "code": string, "timeout_ms"?: number }
 * Response: {
 *   "id": string, "repl_id": string, "success": boolean,
 *   "result"?: any, "result_repr"?: string,
 *   "error"?: string, "stack"?: string,
 *   "content": Array<text|image item>,
 *   "result_truncated": boolean, "content_truncated": boolean,
 *   "duration_ms": number
 * }
 */

import { createServer, Socket } from 'net';
import { unlinkSync, existsSync, promises as fsp } from 'fs';
import vm from 'vm';
import util from 'util';
import { transform } from 'esbuild';
import { CdpClient } from './browser-cdp-client';
import { BrowserHelpers, buildBrowserGlobals } from './browser-helpers';
// Vendored acorn (MIT); used to parse user code for the top-level-await
// rewrite. Bundled into the daemon at image build time.
// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore - no type declarations for the vendored single-file build
import * as acorn from './vendor/acorn.mjs';

const SOCKET_PATH = process.env.BROWSER_REPL_SOCKET || '/tmp/browser-repl.sock';
const REPL_ID = process.env.BROWSER_REPL_ID || 'unknown';
const CDP_ENDPOINT = process.env.CDP_ENDPOINT || 'ws://127.0.0.1:9222';

// Output limits (decoded bytes unless noted).
const MAX_TEXT_BYTES = 256 * 1024; // combined text per response
const MAX_IMAGE_BYTES = 8 * 1024 * 1024; // per emitted image
const MAX_TOTAL_IMAGE_BYTES = 16 * 1024 * 1024; // aggregate image data per response
const MAX_RESULT_BYTES = 256 * 1024; // serialized result
const MAX_REQUEST_BYTES = 8 * 1024 * 1024; // incoming request line
const MAX_STRAY_ITEMS = 1000; // buffered output produced outside an execution

// Private references retained before any user code runs so global/prototype
// modification inside the context cannot corrupt protocol framing.
const safeStringify = JSON.stringify;
const safeInspect = util.inspect;
const safeFormat = util.format;

// ---------------------------------------------------------------------------
// Content collection
// ---------------------------------------------------------------------------

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
      }
      this.truncated = true;
      return;
    }
    this.textBytes += bytes;
    this.items.push({ type: 'text', channel, text });
  }

  /** Returns false when the image was dropped due to the aggregate limit. */
  addImage(mimeType: string, bytes: Buffer): boolean {
    if (this.imageBytes + bytes.length > MAX_TOTAL_IMAGE_BYTES) {
      this.truncated = true;
      return false;
    }
    this.imageBytes += bytes.length;
    this.items.push({ type: 'image', mime_type: mimeType, data_b64: bytes.toString('base64') });
    return true;
  }

  adopt(item: ContentItem): void {
    if (item.type === 'text') {
      this.addText(item.channel, item.text);
    } else {
      this.addImage(item.mime_type, Buffer.from(item.data_b64, 'base64'));
    }
  }
}

let activeCollector: Collector | null = null;
const strayCollector = new Collector();

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
  const collector = currentCollector();
  collector.addText(channel, text);
  if (collector === strayCollector && strayCollector.items.length > MAX_STRAY_ITEMS) {
    strayCollector.items.splice(0, strayCollector.items.length - MAX_STRAY_ITEMS);
  }
}

// ---------------------------------------------------------------------------
// repl namespace + console capture
// ---------------------------------------------------------------------------

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

  if (Buffer.isBuffer(input)) {
    const mime = sniffImageMime(input);
    if (!mime) throw new Error('repl.emitImage: unrecognized image data (expected PNG, JPEG, or WebP)');
    return { bytes: input, mime };
  }

  if (input instanceof Uint8Array || input instanceof ArrayBuffer) {
    const bytes = Buffer.from(input instanceof ArrayBuffer ? new Uint8Array(input) : input);
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
      const raw = obj.bytes as Buffer | Uint8Array | ArrayBuffer;
      const bytes = Buffer.isBuffer(raw)
        ? raw
        : Buffer.from(raw instanceof ArrayBuffer ? new Uint8Array(raw) : raw);
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

// ---------------------------------------------------------------------------
// Persistent evaluation context
// ---------------------------------------------------------------------------

const cdpClient = new CdpClient(CDP_ENDPOINT);
const helpers = new BrowserHelpers(cdpClient);

// Holder for the promise produced by the top-level-await wrapper. Defined in
// the daemon realm so context code cannot interfere with it.
const evalHolder: { promise: Promise<unknown> | null } = { promise: null };

const context: vm.Context = vm.createContext(
  {
    console: consoleCapture,
    repl,
    ...buildBrowserGlobals(helpers),
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
    __replRun__: (fn: () => Promise<unknown>) => {
      evalHolder.promise = fn();
    },
  },
  { name: `browser-repl-${REPL_ID}` },
);

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

const scriptOptions: vm.ScriptOptions = {
  filename: 'browser-repl-execution.js',
  importModuleDynamically: vm.constants.USE_MAIN_CONTEXT_DEFAULT_LOADER,
};

async function transformCode(code: string): Promise<string> {
  // No `format`: esm output makes esbuild wrap top-level returns in a
  // CommonJS shim, and cjs output would break dynamic import(). Leaving it
  // unset preserves the input module syntax.
  const result = await transform(code, {
    loader: 'ts',
    target: 'es2022',
  });
  // esbuild drops expression statements it considers side-effect-free (e.g.
  // a bare `NaN` or `undefined`). In a REPL those expressions ARE the result,
  // so when the whole program was elided, evaluate the original source (it
  // is guaranteed to be valid JavaScript at this point).
  if (result.code.trim() === '' && code.trim() !== '') {
    return code;
  }
  return result.code;
}

function isThenable(value: unknown): value is Promise<unknown> {
  return (
    value !== null &&
    (typeof value === 'object' || typeof value === 'function') &&
    typeof (value as any).then === 'function'
  );
}

/**
 * Rewrite top-level declarations so bindings persist on the context global
 * when the code must run inside an async wrapper (top-level await or
 * top-level return). Mirrors the approach of Node's own REPL, which uses
 * acorn with the same parser options.
 */
function rewriteForAsyncBody(source: string): string {
  let ast: any;
  try {
    ast = acorn.parse(source, {
      ecmaVersion: 'latest',
      allowAwaitOutsideFunction: true,
      allowReturnOutsideFunction: true,
    });
  } catch (err: any) {
    if (typeof err?.message === 'string' && err.message.includes('sourceType: module')) {
      throw new Error('static import/export is not supported in the browser REPL; use dynamic import() instead');
    }
    throw err;
  }
  const statements: any[] = ast.body;
  const parts: string[] = [];

  statements.forEach((st, index) => {
    const isLast = index === statements.length - 1;
    const text = source.slice(st.start, st.end);

    if (st.type === 'VariableDeclaration') {
      const assignments: string[] = [];
      for (const decl of st.declarations) {
        if (!decl.init) continue;
        const nameText = source.slice(decl.id.start, decl.id.end);
        const initText = source.slice(decl.init.start, decl.init.end);
        if (decl.id.type === 'Identifier') {
          assignments.push(`${nameText} = ${initText}`);
        } else {
          // Destructuring patterns need parentheses as expression statements.
          assignments.push(`(${nameText} = ${initText})`);
        }
      }
      if (assignments.length > 0) {
        parts.push(assignments.join(',\n  ') + ';');
      }
      return;
    }

    if ((st.type === 'FunctionDeclaration' || st.type === 'ClassDeclaration') && st.id) {
      parts.push(text);
      // Assign to globalThis explicitly: a plain `Name = Name` assignment
      // would resolve to the function-local binding and never reach the
      // persistent context global.
      parts.push(`globalThis.${st.id.name} = ${st.id.name};`);
      return;
    }

    if (st.type === 'ImportDeclaration' || st.type.startsWith('Export')) {
      throw new Error('static import/export is not supported in the browser REPL; use dynamic import() instead');
    }

    if (isLast && st.type === 'ExpressionStatement') {
      parts.push(`return (${source.slice(st.expression.start, st.expression.end)});`);
      return;
    }

    parts.push(text);
  });

  return parts.join('\n');
}

async function evaluate(code: string): Promise<unknown> {
  const js = await transformCode(code);

  // Fast path: run as a classic script in the persistent context. Top-level
  // let/const/class bindings live in the context's global lexical scope, so
  // they persist across calls with true JavaScript semantics (a prior const
  // cannot be redeclared).
  let script: vm.Script | null = null;
  let compileError: unknown = null;
  try {
    script = new vm.Script(js, scriptOptions);
  } catch (err) {
    compileError = err;
  }

  if (script) {
    let value: unknown = script.runInContext(context);
    if (isThenable(value)) {
      value = await value;
    }
    return value;
  }

  if (!(compileError instanceof SyntaxError)) {
    throw compileError;
  }

  // Fallback: top-level await or return requires an async wrapper. Rewrite
  // top-level declarations into global assignments so bindings still
  // persist. Note that in this mode the implicit result is the value of the
  // final expression statement (or an explicit return); a trailing block
  // statement (try/if/for) yields no implicit result.
  const body = rewriteForAsyncBody(js);
  const wrapped = `__replRun__(async () => {\n${body}\n})`;
  const wrappedScript = new vm.Script(wrapped, scriptOptions);
  evalHolder.promise = null;
  wrappedScript.runInContext(context);
  const promise = evalHolder.promise;
  evalHolder.promise = null;
  if (!promise) {
    throw new Error('internal error: async evaluation did not start');
  }
  return await promise;
}

// ---------------------------------------------------------------------------
// Result serialization
// ---------------------------------------------------------------------------

interface SerializedResult {
  result?: unknown;
  result_repr?: string;
  result_truncated: boolean;
}

function isErrorLike(value: unknown): value is Error {
  return (
    value !== null &&
    typeof value === 'object' &&
    typeof (value as any).message === 'string' &&
    typeof (value as any).stack === 'string' &&
    /Error$/.test((value as any).constructor?.name ?? '')
  );
}

function serializeResult(value: unknown): SerializedResult {
  if (value === undefined) {
    return { result_truncated: false };
  }
  if (isErrorLike(value)) {
    return { result_repr: String((value as any).stack ?? (value as any).message), result_truncated: false };
  }
  try {
    const json = safeStringify(value);
    if (json === undefined) {
      // Functions, symbols, etc.
      return { result_repr: boundedInspect(value), result_truncated: false };
    }
    if (Buffer.byteLength(json) > MAX_RESULT_BYTES) {
      return { result_repr: boundedInspect(value), result_truncated: true };
    }
    return { result: value, result_truncated: false };
  } catch {
    // BigInt, circular structures, throwing toJSON, etc.
    return { result_repr: boundedInspect(value), result_truncated: false };
  }
}

// ---------------------------------------------------------------------------
// Request handling
// ---------------------------------------------------------------------------

interface ExecuteRequest {
  id: string;
  code: string;
  timeout_ms?: number;
}

interface ExecuteResponse {
  id: string;
  repl_id: string;
  success: boolean;
  result?: unknown;
  result_repr?: string;
  error?: string;
  stack?: string;
  content: ContentItem[];
  result_truncated: boolean;
  content_truncated: boolean;
  duration_ms: number;
}

async function executeRequest(request: ExecuteRequest): Promise<ExecuteResponse> {
  const start = Date.now();
  const collector = new Collector();

  // Drain output produced outside any execution (e.g. late console callbacks)
  // into this execution, preserving order ahead of new output.
  for (const item of strayCollector.items) {
    collector.adopt(item);
  }
  strayCollector.items.length = 0;
  strayCollector.truncated = false;

  activeCollector = collector;
  const timeoutMs = request.timeout_ms ?? 60_000;
  let timer: ReturnType<typeof setTimeout> | undefined;

  try {
    const timeoutPromise = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        reject(new Error(`execution timed out after ${timeoutMs}ms`));
      }, timeoutMs);
      if (typeof timer.unref === 'function') timer.unref();
    });
    const value = await Promise.race([evaluate(request.code), timeoutPromise]);
    const serialized = serializeResult(value);
    return {
      id: request.id,
      repl_id: REPL_ID,
      success: true,
      ...serialized,
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
      result_truncated: false,
      content_truncated: collector.truncated,
      duration_ms: Date.now() - start,
    };
  } finally {
    if (timer) clearTimeout(timer);
    activeCollector = null;
  }
}

// Serialize executions as defense in depth; the Go handler already holds a
// mutex, but the daemon must never interleave two executions.
let executionChain: Promise<void> = Promise.resolve();

function enqueueExecution(request: ExecuteRequest, socket: Socket): void {
  executionChain = executionChain.then(async () => {
    let response: ExecuteResponse;
    try {
      response = await executeRequest(request);
    } catch (err: any) {
      response = {
        id: request.id,
        repl_id: REPL_ID,
        success: false,
        error: `internal daemon error: ${String(err?.message ?? err)}`,
        content: [],
        result_truncated: false,
        content_truncated: false,
        duration_ms: 0,
      };
    }
    try {
      socket.write(safeStringify(response) + '\n');
    } catch (err: any) {
      process.stderr.write(`[browser-repl] failed to write response: ${err?.message ?? err}\n`);
    }
  });
}

function handleConnection(socket: Socket): void {
  let buffer = '';

  socket.on('data', (data) => {
    buffer += data.toString();
    if (buffer.length > MAX_REQUEST_BYTES && buffer.indexOf('\n') === -1) {
      socket.write(
        safeStringify({
          id: 'unknown',
          repl_id: REPL_ID,
          success: false,
          error: `request exceeds the ${MAX_REQUEST_BYTES} byte limit`,
          content: [],
          result_truncated: false,
          content_truncated: false,
          duration_ms: 0,
        }) + '\n',
      );
      socket.destroy();
      return;
    }

    let newlineIndex: number;
    while ((newlineIndex = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, newlineIndex);
      buffer = buffer.slice(newlineIndex + 1);
      if (!line.trim()) continue;

      let request: ExecuteRequest;
      try {
        request = JSON.parse(line);
      } catch {
        socket.write(
          safeStringify({
            id: 'unknown',
            repl_id: REPL_ID,
            success: false,
            error: 'invalid JSON request',
            content: [],
            result_truncated: false,
            content_truncated: false,
            duration_ms: 0,
          }) + '\n',
        );
        continue;
      }

      if (!request.id || typeof request.code !== 'string') {
        socket.write(
          safeStringify({
            id: (request as any)?.id || 'unknown',
            repl_id: REPL_ID,
            success: false,
            error: 'invalid request: missing id or code',
            content: [],
            result_truncated: false,
            content_truncated: false,
            duration_ms: 0,
          }) + '\n',
        );
        continue;
      }

      enqueueExecution(request, socket);
    }
  });

  socket.on('error', (err) => {
    process.stderr.write(`[browser-repl] socket error: ${err.message}\n`);
  });
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

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

  const server = createServer(handleConnection);
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
