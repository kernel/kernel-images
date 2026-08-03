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
 *   "timed_out"?: boolean,
 *   "duration_ms": number
 * }
 *
 * `timed_out: true` marks a daemon-side execution timeout. JavaScript cannot
 * be reliably interrupted inside this process, so the abandoned execution
 * would keep running concurrently with later ones and leak its output into
 * their responses. The API parent therefore treats `timed_out` exactly like a
 * transport failure: it kills the process group and reports
 * `repl_terminated: true` with the terminated repl_id. The flag exists so the
 * caller still receives the partial content produced before the deadline
 * instead of a bare kill.
 *
 * `exiting: true` marks a deterministic daemon shutdown after an uncaught
 * exception (see the process-level handlers at the bottom of this file). The
 * in-flight caller receives the exception as a normal execution failure; the
 * API parent treats `exiting` like `timed_out` and reports
 * `repl_terminated: true`, so state loss is explicit rather than silent.
 *
 * Process-level user-code failures are split by Node lifecycle semantics:
 *  - unhandledRejection: the rejected promise has already settled, so no
 *    in-flight operation is left inconsistent. Node sanctions keeping the
 *    process alive when a handler is installed. The rejection is surfaced
 *    as a bounded stderr content item (into the active execution, or the
 *    next one) and the REPL — and all of its state — survives.
 *  - uncaughtException: Node documentation is explicit that resuming after
 *    an uncaught exception is unsafe because the process may be in an
 *    undefined state. The daemon therefore terminates deterministically:
 *    it logs the error, answers the in-flight execution (when one exists)
 *    with `exiting: true`, and exits non-zero. The next request lazily
 *    starts a fresh REPL with a new repl_id — the documented signal that
 *    all prior state was lost.
 */

import { createServer, Socket } from 'net';
import { unlinkSync, existsSync, promises as fsp } from 'fs';
import vm from 'vm';
import util from 'util';
import { transform } from 'esbuild';
import { CdpClient } from './browser-cdp-client';
import { BrowserHelpers, buildBrowserGlobals } from './browser-helpers';
// Acorn is bundled into the daemon at image build time. Keep esbuild external
// because it is also loaded dynamically to transform user TypeScript.
import * as acorn from 'acorn';

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
// modification inside the context cannot corrupt protocol framing or result
// serialization.
const safeStringify = JSON.stringify;
const safeParse = JSON.parse;
const safeInspect = util.inspect;
const safeFormat = util.format;
const safeDateToISOString = Date.prototype.toISOString;
const safeGetPrototypeOf = Object.getPrototypeOf;
const safeKeys = Object.keys;

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

/**
 * Convert a Buffer / TypedArray / DataView / ArrayBuffer to a Buffer.
 * Cross-realm safe: user values are constructed in the vm context's realm,
 * whose intrinsics differ from the daemon's, so `instanceof Uint8Array` /
 * `instanceof ArrayBuffer` reject legitimate inputs. ArrayBuffer.isView and
 * util.types inspect internal slots and work across realms.
 */
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

// ---------------------------------------------------------------------------
// Persistent evaluation context
// ---------------------------------------------------------------------------

const cdpClient = new CdpClient(CDP_ENDPOINT);
const helpers = new BrowserHelpers(cdpClient);

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

// Holder for the promise produced by the top-level-await wrapper. Defined in
// the daemon realm so context code cannot interfere with it.
const evalHolder: { promise: Promise<unknown> | null } = { promise: null };

// Cross-realm handles captured right after context creation; the injected
// __replDefine__ closure reads them lazily when invoked from context code.
let contextGlobal: Record<PropertyKey, unknown>;
let ContextTypeError: new (message: string) => Error;

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
    // Exports a binding declared inside the async wrapper onto the context
    // global with real mutability semantics (see rewriteForAsyncBody):
    // const is backed by an accessor whose setter throws the same TypeError
    // V8 raises for a lexical const, let/class produce a writable
    // non-configurable data property. Redeclaration conflicts are rejected
    // earlier, at rewrite time, via replDeclaredNames.
    __replDefine__: (kind: 'let' | 'const' | 'class', name: string, value: unknown) => {
      if (kind === 'const') {
        Object.defineProperty(contextGlobal, name, {
          enumerable: true,
          configurable: false,
          get: () => value,
          set: () => {
            throw new ContextTypeError('Assignment to constant variable.');
          },
        });
      } else {
        Object.defineProperty(contextGlobal, name, {
          value,
          writable: true,
          enumerable: true,
          configurable: false,
        });
      }
    },
  },
  { name: `browser-repl-${REPL_ID}` },
);

contextGlobal = vm.runInContext('globalThis', context) as Record<PropertyKey, unknown>;
ContextTypeError = vm.runInContext('TypeError', context) as new (message: string) => Error;

// The context realm's Object.prototype. User values are created in the vm
// context's realm, whose intrinsics differ from the daemon's, so plain-object
// detection must compare against the context realm's prototype.
const contextObjectPrototype: object = vm.runInContext('Object.prototype', context) as object;

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

const scriptOptions: vm.ScriptOptions = {
  filename: 'browser-repl-execution.js',
  importModuleDynamically: vm.constants.USE_MAIN_CONTEXT_DEFAULT_LOADER,
};

const STATIC_IMPORT_ERROR =
  'static import/export is not supported in the browser REPL; use dynamic import() instead';

/**
 * Parse transformed JavaScript for the evaluation pipeline. Tolerates
 * top-level await and return (the async-wrapper path compiles those
 * separately).
 *
 * Static import/export is rejected here: the transform runs with
 * preserveValueImports so esbuild's TypeScript loader cannot elide unused
 * imports before this check — every import/export statement survives into
 * the transformed JavaScript, where it is a syntax error in acorn's script
 * mode. That makes the rejection unconditional for every form, used or
 * unused, even when TypeScript-only syntax precedes the import (a
 * raw-source acorn scan cannot parse such input and would miss it).
 * Type-only imports (`import type ...`) are erased by the transform and
 * remain allowed. Other parse failures return null; the vm compiler (fast
 * path) or the async-wrapper rewrite (fallback) reports those.
 */
function parseScript(js: string): any | null {
  try {
    return acorn.parse(js, {
      ecmaVersion: 'latest',
      allowAwaitOutsideFunction: true,
      allowReturnOutsideFunction: true,
    });
  } catch (err: any) {
    if (typeof err?.message === 'string' && err.message.includes('sourceType: module')) {
      throw new Error(STATIC_IMPORT_ERROR);
    }
    return null;
  }
}

async function transformCode(code: string): Promise<{ js: string; ast: any | null }> {
  // No `format`: esm output makes esbuild wrap top-level returns in a
  // CommonJS shim, and cjs output would break dynamic import(). Leaving it
  // unset preserves the input module syntax.
  const result = await transform(code, {
    loader: 'ts',
    target: 'es2022',
    // Preserve unused imports so the static-import check below sees them;
    // esbuild's TypeScript loader would otherwise elide them.
    tsconfigRaw: { compilerOptions: { preserveValueImports: true } },
  });
  // esbuild drops expression statements it considers side-effect-free (e.g.
  // a bare `NaN` or `undefined`). In a REPL those expressions ARE the result,
  // so when the whole program was elided, evaluate the original source (it
  // is guaranteed to be valid JavaScript at this point).
  const js = result.code.trim() === '' && code.trim() !== '' ? code : result.code;
  const ast = parseScript(js);
  return { js, ast };
}

// ---------------------------------------------------------------------------
// Cross-call declaration tracking
// ---------------------------------------------------------------------------

type DeclKind = 'var' | 'function' | 'let' | 'const' | 'class';

interface DeclaredName {
  name: string;
  kind: DeclKind;
}

// replDeclaredNames tracks every top-level binding the REPL has created, on
// either evaluation path, so redeclaration rules hold across calls no matter
// which path declared the name. The vm enforces conflicts among
// classic-script (fast-path) declarations on its own; this registry extends
// the same rules to bindings the async wrapper exports onto the context
// global via __replDefine__, which the vm cannot see. Executions are
// serialized, so no locking is needed.
const replDeclaredNames = new Map<string, DeclKind>();

function isLexicalKind(kind: DeclKind): boolean {
  return kind === 'let' || kind === 'const' || kind === 'class';
}

// canRedeclare mirrors top-level script semantics: var and function
// declarations may redeclare each other freely; let/const/class conflict
// with any prior declaration of the same name (and vice versa).
function canRedeclare(existing: DeclKind, incoming: DeclKind): boolean {
  return !isLexicalKind(existing) && !isLexicalKind(incoming);
}

function alreadyDeclaredError(name: string): SyntaxError {
  return new SyntaxError(`Identifier '${name}' has already been declared`);
}

function collectPatternNames(pattern: any, out: string[]): void {
  if (!pattern) return;
  switch (pattern.type) {
    case 'Identifier':
      out.push(pattern.name);
      return;
    case 'ObjectPattern':
      for (const prop of pattern.properties) {
        if (prop.type === 'RestElement') collectPatternNames(prop.argument, out);
        else collectPatternNames(prop.value, out);
      }
      return;
    case 'ArrayPattern':
      for (const el of pattern.elements) collectPatternNames(el, out);
      return;
    case 'AssignmentPattern':
      collectPatternNames(pattern.left, out);
      return;
    case 'RestElement':
      collectPatternNames(pattern.argument, out);
      return;
    default:
      return;
  }
}

function collectTopLevelDeclarations(ast: any): DeclaredName[] {
  const out: DeclaredName[] = [];
  for (const st of ast?.body ?? []) {
    if (st.type === 'VariableDeclaration') {
      const names: string[] = [];
      for (const decl of st.declarations) {
        collectPatternNames(decl.id, names);
      }
      for (const name of names) {
        out.push({ name, kind: st.kind as DeclKind });
      }
    } else if (st.type === 'FunctionDeclaration' && st.id) {
      out.push({ name: st.id.name, kind: 'function' });
    } else if (st.type === 'ClassDeclaration' && st.id) {
      out.push({ name: st.id.name, kind: 'class' });
    }
  }
  return out;
}

/**
 * Enforce cross-call redeclaration rules before any user code runs.
 * JavaScript reports redeclaration as an early error: no statement of the
 * program executes, so this check must run ahead of both evaluation paths.
 */
function precheckDeclarations(declared: DeclaredName[]): void {
  for (const { name, kind } of declared) {
    const existing = replDeclaredNames.get(name);
    if (existing !== undefined && !canRedeclare(existing, kind)) {
      throw alreadyDeclaredError(name);
    }
  }
}

/**
 * Record bindings a submission is about to instantiate. Returns an undo
 * closure for the case where script execution fails during global
 * declaration instantiation (a redeclaration the registry could not see,
 * e.g. a prior declaration written in syntax acorn could not parse), which
 * leaves no bindings behind.
 */
function registerDeclarations(declared: DeclaredName[]): () => void {
  const previous: DeclaredName[] = [];
  for (const { name, kind } of declared) {
    const existing = replDeclaredNames.get(name);
    if (existing !== undefined) previous.push({ name, kind: existing });
    replDeclaredNames.set(name, kind);
  }
  return () => {
    for (const { name, kind } of [...declared].reverse()) {
      replDeclaredNames.delete(name);
    }
    for (const { name, kind } of previous) {
      replDeclaredNames.set(name, kind);
    }
  };
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
 * top-level return). Parses with acorn using the same options as Node's own
 * REPL.
 *
 * Persistence semantics match the classic-script (fast) path as closely as
 * a function scope allows:
 *
 *  - var and function declarations become ordinary global assignments and
 *    stay freely redeclarable.
 *  - let/const/class bindings are exported via __replDefine__: const is
 *    backed by an accessor whose setter throws TypeError on assignment
 *    (matching V8's "Assignment to constant variable."), let/class produce
 *    writable non-configurable data properties.
 *  - redeclaration is an early error enforced by replDeclaredNames before
 *    any user code runs, so a prior const cannot be redeclared no matter
 *    which evaluation path declared it.
 *
 * Registration happens here, before execution: script-level declarations
 * instantiate before the first statement runs, so the names stay reserved
 * even when an initializer later throws.
 */
function rewriteForAsyncBody(source: string, parsedAst: any | null): string {
  const ast =
    parsedAst ??
    (() => {
      try {
        return acorn.parse(source, {
          ecmaVersion: 'latest',
          allowAwaitOutsideFunction: true,
          allowReturnOutsideFunction: true,
        });
      } catch (err: any) {
        if (typeof err?.message === 'string' && err.message.includes('sourceType: module')) {
          throw new Error(STATIC_IMPORT_ERROR);
        }
        throw err;
      }
    })();
  registerDeclarations(collectTopLevelDeclarations(ast));
  const statements: any[] = ast.body;
  const parts: string[] = [];

  statements.forEach((st, index) => {
    const isLast = index === statements.length - 1;
    const text = source.slice(st.start, st.end);

    if (st.type === 'VariableDeclaration') {
      const kind = st.kind as DeclKind;
      if (kind === 'var') {
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
      // let/const: export each binding to the context global with real
      // mutability semantics. Same-body references resolve through the
      // global, which __replDefine__ populates before the next statement.
      for (const decl of st.declarations) {
        const initText = decl.init ? source.slice(decl.init.start, decl.init.end) : 'undefined';
        if (decl.id.type === 'Identifier') {
          parts.push(`__replDefine__(${JSON.stringify(kind)}, ${JSON.stringify(decl.id.name)}, (${initText}));`);
        } else {
          const nameText = source.slice(decl.id.start, decl.id.end);
          const names: string[] = [];
          collectPatternNames(decl.id, names);
          parts.push(`(${nameText} = (${initText}));`);
          for (const name of names) {
            parts.push(`__replDefine__(${JSON.stringify(kind)}, ${JSON.stringify(name)}, ${name});`);
          }
        }
      }
      return;
    }

    if (st.type === 'FunctionDeclaration' && st.id) {
      parts.push(text);
      // Assign to globalThis explicitly: a plain `Name = Name` assignment
      // would resolve to the function-local binding and never reach the
      // persistent context global.
      parts.push(`globalThis.${st.id.name} = ${st.id.name};`);
      return;
    }

    if (st.type === 'ClassDeclaration' && st.id) {
      parts.push(text);
      // Classes get let-like mutability on the persistent global.
      parts.push(`__replDefine__('class', ${JSON.stringify(st.id.name)}, ${st.id.name});`);
      return;
    }

    if (st.type === 'ImportDeclaration' || st.type.startsWith('Export')) {
      throw new Error(STATIC_IMPORT_ERROR);
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
  const { js, ast } = await transformCode(code);

  // Cross-call redeclaration is an early error on both evaluation paths;
  // check before any user code runs.
  const declared = ast ? collectTopLevelDeclarations(ast) : [];
  precheckDeclarations(declared);

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
    const undoRegistration = registerDeclarations(declared);
    try {
      let value: unknown = script.runInContext(context);
      if (isThenable(value)) {
        value = await value;
      }
      return value;
    } catch (err) {
      // A redeclaration SyntaxError here means global declaration
      // instantiation failed (a conflict the registry could not see, e.g.
      // a prior binding written in syntax acorn could not parse): no
      // bindings were created, so roll the registry back. Runtime errors
      // leave the instantiated bindings — and their registration — in
      // place.
      if (err instanceof SyntaxError && /already been declared/.test(String(err?.message)))
        undoRegistration();
      throw err;
    }
  }

  if (!(compileError instanceof SyntaxError)) {
    throw compileError;
  }

  // Fallback: top-level await or return requires an async wrapper. Rewrite
  // top-level declarations into persistent global bindings (see
  // rewriteForAsyncBody for the semantics). Note that in this mode the
  // implicit result is the value of the final expression statement (or an
  // explicit return); a trailing block statement (try/if/for) yields no
  // implicit result.
  const body = rewriteForAsyncBody(js, ast);
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

/**
 * Serialize a value to JSON text without ever invoking user code.
 * JSON.stringify consults toJSON hooks on the value's (context-realm)
 * prototypes, so user code like `Array.prototype.toJSON = () => 'PWNED'`
 * would otherwise corrupt the reported result payload.
 *
 * Only primitives, plain objects, arrays, and Dates are JSON-compatible;
 * anything else (Map, RegExp, class instances, functions, bigint, circular
 * structures, throwing getters, ...) returns undefined and the caller falls
 * back to the bounded repr. Serialization otherwise follows JSON.stringify:
 * undefined/function/symbol array elements become null and object properties
 * with such values are skipped.
 */
function toJsonText(value: unknown): string | undefined {
  const seen = new Set<object>();

  const write = (v: unknown): string | undefined => {
    if (v === null) return 'null';
    switch (typeof v) {
      case 'boolean':
        return v ? 'true' : 'false';
      case 'number':
        // Nested non-finite numbers follow JSON.stringify and become null;
        // top-level non-finite numbers are routed to the repr by the caller.
        return Number.isFinite(v) ? safeStringify(v) : 'null';
      case 'string':
        return safeStringify(v);
      case 'bigint':
        // JSON.stringify throws on bigint anywhere; route the whole value
        // to the repr.
        throw new Error('bigint is not JSON-compatible');
      default:
        // undefined, function, symbol: not representable; the caller
        // decides between skip (object property), null (array element),
        // and the repr fallback (top level).
        if (typeof v !== 'object') return undefined;
        break;
    }

    const obj = v as object;
    // Dates serialize through the pristine toISOString. Cross-realm safe:
    // the method reads internal slots, not context-realm prototypes. Throws
    // for non-Dates and invalid Dates, which fall through to the plain
    // object check (a Date's prototype is not Object.prototype, so invalid
    // Dates end up in the repr as "Invalid Date").
    try {
      return safeStringify(safeDateToISOString.call(obj));
    } catch {
      // Not a Date; continue.
    }

    const isArray = Array.isArray(obj);
    const proto = safeGetPrototypeOf(obj);
    const isPlain = proto === null || proto === contextObjectPrototype || proto === Object.prototype;
    if (!isArray && !isPlain) {
      // Map, RegExp, class instances, etc.: JSON.stringify would silently
      // produce {} or lossy output; route the whole value to the repr.
      throw new Error('non-plain object is not JSON-compatible');
    }
    if (seen.has(obj)) {
      // JSON.stringify throws on circular structures; route to the repr.
      throw new Error('circular structure is not JSON-compatible');
    }
    seen.add(obj);
    try {
      if (isArray) {
        const arr = obj as unknown[];
        const parts: string[] = [];
        for (let i = 0; i < arr.length; i++) {
          const text = write(arr[i]);
          // JSON.stringify serializes undefined/function/symbol array
          // elements as null.
          parts.push(text === undefined ? 'null' : text);
        }
        return `[${parts.join(',')}]`;
      }
      const parts: string[] = [];
      for (const key of safeKeys(obj)) {
        const text = write((obj as Record<string, unknown>)[key]);
        // JSON.stringify skips undefined/function/symbol properties.
        if (text === undefined) continue;
        parts.push(`${safeStringify(key)}:${text}`);
      }
      return `{${parts.join(',')}}`;
    } finally {
      seen.delete(obj);
    }
  };

  try {
    return write(value);
  } catch {
    // Proxy traps, throwing getters, etc.
    return undefined;
  }
}

function serializeResult(value: unknown): SerializedResult {
  if (value === undefined) {
    // Surface undefined explicitly so it is distinguishable from null.
    return { result_repr: 'undefined', result_truncated: false };
  }
  if (isErrorLike(value)) {
    return { result_repr: String((value as any).stack ?? (value as any).message), result_truncated: false };
  }
  // JSON.stringify silently converts NaN/Infinity to null and -0 to 0;
  // surface them through the repr instead of dropping the value.
  if (typeof value === 'number' && (!Number.isFinite(value) || Object.is(value, -0))) {
    return { result_repr: boundedInspect(value), result_truncated: false };
  }
  const json = toJsonText(value);
  if (json === undefined) {
    // Not JSON-compatible: functions, symbols, bigint, Map, RegExp, class
    // instances, circular structures, throwing getters, etc.
    return { result_repr: boundedInspect(value), result_truncated: false };
  }
  if (Buffer.byteLength(json) > MAX_RESULT_BYTES) {
    return { result_repr: boundedInspect(value), result_truncated: true };
  }
  // Re-parse with the pristine parser so the framing serializer never sees
  // context-realm objects (and their potentially polluted prototypes).
  return { result: safeParse(json), result_truncated: false };
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

  // Drain output produced outside any execution (e.g. timers scheduled by a
  // completed execution) into this execution, preserving order ahead of new
  // output. Output from a timed-out execution can never reach this buffer:
  // the API parent kills the process on timed_out before the next request.
  for (const item of strayCollector.items) {
    collector.adopt(item);
  }
  strayCollector.items.length = 0;
  strayCollector.truncated = false;

  activeCollector = collector;
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
      // A timed-out execution is merely abandoned, not interrupted: its code
      // is still running. The API parent must kill this process (it does,
      // destructively, per the spec's timeout semantics) before serving
      // another execution.
      timed_out: timedOut || undefined,
      duration_ms: Date.now() - start,
    };
  } finally {
    if (timer) clearTimeout(timer);
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
        result_truncated: false,
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
      result_truncated: false,
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
          result_truncated: false,
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
          result_truncated: false,
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

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

/**
 * An unhandled promise rejection leaves no inconsistent in-flight state:
 * the promise has already settled and Node sanctions keeping the process
 * alive when a handler is installed (without one, Node >= 15 crashes the
 * process). Floating promises are extremely common in agent-authored code
 * (e.g. `js("alert('x')")` without await, whose CDP timeout rejection would
 * otherwise kill the REPL 30s later mid-unrelated-request), so the
 * rejection is surfaced as a bounded stderr content item — into the active
 * execution, or drained into the next one — and the REPL and all of its
 * state survive.
 */
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

/**
 * Node documentation is explicit that resuming after an uncaught exception
 * is unsafe: the throw may have originated anywhere, leaving the process in
 * an undefined state. Rather than blindly keeping a possibly-corrupt
 * state-holding process alive, the daemon terminates deterministically and
 * preserves evidence:
 *
 *  1. the exception is logged to stderr (container logs);
 *  2. the in-flight execution, when one exists, is answered with
 *     success: false, the exception details, partial content, and
 *     exiting: true — the API maps that to repl_terminated: true, so the
 *     caller sees the state loss explicitly instead of a bare EOF;
 *  3. the process exits non-zero; the next request lazily starts a fresh
 *     REPL with a new repl_id, the documented signal that all prior
 *     bindings were lost.
 */
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
        result_truncated: false,
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
