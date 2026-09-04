const functionToString = Function.prototype.toString;
const reflectApply = Reflect.apply;
const FunctionConstructor = Function;
const objectToString = Object.prototype.toString;

export type PageFunction = (arg: any) => unknown;

export interface JsOptions {
  arg?: unknown;
  targetId?: string;
}

type SerializedArgument =
  | { type: 'undefined' }
  | { type: 'null' }
  | { type: 'boolean'; value: boolean }
  | { type: 'string'; value: string }
  | { type: 'number'; value: number | 'NaN' | 'Infinity' | '-Infinity' | '-0' }
  | { type: 'bigint'; value: string }
  | { type: 'array'; value: SerializedArgument[] }
  | { type: 'object'; value: Array<[string, SerializedArgument]> };

function serializeArgument(value: unknown, seen = new Set<object>()): SerializedArgument {
  if (value === undefined) return { type: 'undefined' };
  if (value === null) return { type: 'null' };
  if (typeof value === 'boolean') return { type: 'boolean', value };
  if (typeof value === 'string') return { type: 'string', value };
  if (typeof value === 'number') {
    if (Number.isNaN(value)) return { type: 'number', value: 'NaN' };
    if (value === Infinity) return { type: 'number', value: 'Infinity' };
    if (value === -Infinity) return { type: 'number', value: '-Infinity' };
    if (Object.is(value, -0)) return { type: 'number', value: '-0' };
    return { type: 'number', value };
  }
  if (typeof value === 'bigint') return { type: 'bigint', value: value.toString() };
  if (typeof value !== 'object') {
    throw new Error(`js: arg contains unsupported ${typeof value} value`);
  }
  if (seen.has(value)) {
    throw new Error('js: arg must not contain cycles');
  }
  seen.add(value);
  try {
    if (Array.isArray(value)) {
      return { type: 'array', value: value.map((item) => serializeArgument(item, seen)) };
    }
    if (reflectApply(objectToString, value, []) !== '[object Object]') {
      throw new Error('js: arg must contain only plain objects and arrays');
    }
    const entries: Array<[string, SerializedArgument]> = [];
    for (const key of Object.keys(value)) {
      entries.push([key, serializeArgument((value as Record<string, unknown>)[key], seen)]);
    }
    return { type: 'object', value: entries };
  } finally {
    seen.delete(value);
  }
}

function isFunctionExpression(source: string): boolean {
  try {
    FunctionConstructor(`return (${source}\n)`);
    return true;
  } catch {
    return false;
  }
}

function normalizeFunctionSource(fn: PageFunction): string {
  let source = reflectApply(functionToString, fn, []).trim();
  if (source.includes('[native code]') || source.startsWith('class ')) {
    throw new Error('js: page function is not serializable');
  }
  if (isFunctionExpression(source)) return source;

  source = source.startsWith('async ')
    ? `async function ${source.slice('async '.length)}`
    : `function ${source}`;
  if (!isFunctionExpression(source)) {
    throw new Error('js: page function is not serializable');
  }
  return source;
}

function jsonForExpression(value: unknown): string {
  return JSON.stringify(value).replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029');
}

const reviveArgumentSource = `function revive(node) {
  switch (node.type) {
    case 'undefined': return undefined;
    case 'null': return null;
    case 'boolean':
    case 'string': return node.value;
    case 'number':
      if (node.value === 'NaN') return NaN;
      if (node.value === 'Infinity') return Infinity;
      if (node.value === '-Infinity') return -Infinity;
      if (node.value === '-0') return -0;
      return node.value;
    case 'bigint': return BigInt(node.value);
    case 'array': return node.value.map(revive);
    case 'object': {
      const out = {};
      for (const [key, value] of node.value) {
        Object.defineProperty(out, key, {
          value: revive(value), enumerable: true, configurable: true, writable: true,
        });
      }
      return out;
    }
    default: throw new Error('invalid serialized Browser REPL argument');
  }
}`;

export function buildFunctionCallExpression(fn: PageFunction, arg: unknown): string {
  const source = normalizeFunctionSource(fn);
  const payload = jsonForExpression(serializeArgument(arg));
  return `(function (payload) {
    ${reviveArgumentSource}
    return (${source})(revive(payload));
  })(${payload})`;
}

export function normalizeJsOptions(options: unknown): JsOptions {
  if (options === undefined) return {};
  if (options === null || typeof options !== 'object' || Array.isArray(options)) {
    throw new Error('js: options must be an object with optional arg and targetId fields');
  }
  const keys = Object.keys(options);
  const unknown = keys.find((key) => key !== 'arg' && key !== 'targetId');
  if (unknown) throw new Error(`js: unknown option: ${unknown}`);
  const normalized = options as JsOptions;
  if (normalized.targetId !== undefined && typeof normalized.targetId !== 'string') {
    throw new Error('js: targetId must be a target id string (see iframeTarget/listTabs)');
  }
  return normalized;
}
