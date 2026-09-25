import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// The page-side reader that lib/webmcpclient evaluates in a frame's main world.
const source = readFileSync(new URL('../lib/webmcpclient/polyfill_page.js', import.meta.url), 'utf8');

interface PageScript {
  list(window: unknown): Promise<{tools: unknown[]} | null>;
  invoke(window: unknown, name: string, input: Record<string, unknown>): Promise<{ok: boolean; output?: string | null; error?: string}>;
}

// Results come from the vm realm; strip prototypes before comparing.
function plain<T>(value: T): T {
  return JSON.parse(JSON.stringify(value));
}

function pageScript(): PageScript {
  const script = vm.runInNewContext(source, {}) as PageScript;
  return {
    list: async (window) => plain(await script.list(window)),
    invoke: async (window, name, input) => plain(await script.invoke(window, name, input)),
  };
}

type Tool = {name: string; description?: string; inputSchema?: unknown; execute?: (input: unknown) => unknown} & Record<string, unknown>;

// Mirrors the polyfill shape that sites ship before Chromium exposed
// document.modelContext: a registry object plus listTools/callTool helpers.
function sitePolyfill(): Record<string, unknown> {
  const registry: Record<string, Tool> = {};
  return {
    _registeredTools: registry,
    registerTool(tool: Tool, options?: {signal?: AbortSignal}) {
      registry[tool.name] = {
        name: tool.name,
        title: tool.title,
        description: tool.description,
        inputSchema: tool.inputSchema ?? tool.parameters,
        execute: tool.execute,
      };
      options?.signal?.addEventListener('abort', () => delete registry[tool.name], {once: true});
    },
    unregisterTool(name: string) {
      delete registry[name];
    },
    async callTool(name: string, input: unknown) {
      const tool = registry[name];
      if (!tool) throw new Error(`Tool not found: ${name}`);
      return tool.execute?.(input);
    },
    listTools: () => Object.values(registry).map((tool) => ({name: tool.name, description: tool.description, inputSchema: tool.inputSchema})),
  };
}

function fakeWindow(modelContext: unknown, documentModelContext?: unknown): Record<string, unknown> {
  return {navigator: {modelContext}, document: {modelContext: documentModelContext}};
}

test('lists and invokes tools from a site polyfill on navigator.modelContext', async () => {
  const polyfill = sitePolyfill() as ReturnType<typeof sitePolyfill> & {registerTool: (tool: Tool) => void};
  polyfill.registerTool({
    name: 'search_items',
    description: 'Search the catalog.',
    inputSchema: {type: 'object', properties: {query: {type: 'string'}}, required: ['query']},
    execute: async (input) => ({results: [`match for ${(input as {query: string}).query}`]}),
  });
  polyfill.registerTool({
    name: 'legacy_params',
    description: 'Uses the pre-standard parameters alias.',
    parameters: {type: 'object', properties: {}},
    execute: async () => undefined,
  });
  polyfill.registerTool({name: 'broken', description: 'Throws.', inputSchema: {type: 'object'}, execute: async () => {
    throw new Error('page rejected the call');
  }});
  const window = fakeWindow(polyfill);
  const script = pageScript();

  const listed = await script.list(window);
  assert.deepEqual(listed, {
    tools: [
      {name: 'search_items', description: 'Search the catalog.', inputSchema: {type: 'object', properties: {query: {type: 'string'}}, required: ['query']}},
      {name: 'legacy_params', description: 'Uses the pre-standard parameters alias.', inputSchema: {type: 'object', properties: {}}},
      {name: 'broken', description: 'Throws.', inputSchema: {type: 'object'}},
    ],
  });

  const invoked = await script.invoke(window, 'search_items', {query: 'lamp'});
  assert.deepEqual(invoked, {ok: true, output: JSON.stringify({results: ['match for lamp']})});
  assert.deepEqual(await script.invoke(window, 'legacy_params', {}), {ok: true, output: null});
  assert.deepEqual(await script.invoke(window, 'broken', {}), {ok: false, error: 'page rejected the call'});
  assert.deepEqual(await script.invoke(window, 'missing', {}), {ok: false, error: 'Tool not found: missing'});

  (polyfill.unregisterTool as (name: string) => void)('search_items');
  assert.equal((await script.list(window))?.tools.length, 2);
});

test('supports MCP-style polyfills that take callTool({name, arguments})', async () => {
  const tools = new Map<string, Tool>();
  tools.set('add_to_cart', {
    name: 'add_to_cart',
    title: 'Add to cart',
    description: 'Add an item.',
    inputSchema: {type: 'object', properties: {sku: {type: 'string'}}},
    outputSchema: {type: 'object'},
    annotations: {readOnlyHint: false, destructiveHint: true, ignored: 'x'},
    execute: async () => ({content: [{type: 'text', text: 'added'}]}),
  });
  const polyfill = {
    _tools: tools,
    listTools: () => [...tools.values()].map(({execute: _execute, ...metadata}) => metadata),
    async callTool(params: {name: string; arguments: unknown}) {
      const tool = tools.get(params.name);
      if (!tool) throw new Error(`unknown tool ${params.name}`);
      return tool.execute?.(params.arguments);
    },
  };
  const script = pageScript();
  const window = fakeWindow(polyfill);

  const listed = await script.list(window);
  assert.deepEqual(listed?.tools, [{
    name: 'add_to_cart',
    title: 'Add to cart',
    description: 'Add an item.',
    inputSchema: {type: 'object', properties: {sku: {type: 'string'}}},
    outputSchema: {type: 'object'},
    annotations: {readOnlyHint: false, destructiveHint: true},
  }]);
  assert.deepEqual(await script.invoke(window, 'add_to_cart', {sku: '1'}), {
    ok: true,
    output: JSON.stringify({content: [{type: 'text', text: 'added'}]}),
  });
});

test('falls back to the registry when the polyfill has no list or call helpers', async () => {
  const registry = new Map<string, Tool>();
  registry.set('get_context', {name: 'get_context', description: 'Context.', inputSchema: {type: 'object'}, execute: async () => 'ctx'});
  const polyfill = {registerTool() {}, unregisterTool() {}, provideContext() {}, clearContext() {}};
  const script = pageScript();
  const window = {...fakeWindow(polyfill), __webmcp: {tools: registry}};

  assert.deepEqual(await script.list(window), {tools: [{name: 'get_context', description: 'Context.', inputSchema: {type: 'object'}}]});
  assert.deepEqual(await script.invoke(window, 'get_context', {}), {ok: true, output: '"ctx"'});
  assert.deepEqual(await script.invoke(fakeWindow(polyfill), 'get_context', {}), {
    ok: false,
    error: 'the modelContext polyfill does not expose a way to execute tools',
  });
});

test('ignores the native registry and pages without a polyfill', async () => {
  const script = pageScript();
  class ModelContext {
    get [Symbol.toStringTag]() {
      return 'ModelContext';
    }
    async getTools() {
      return [{name: 'native_tool', description: 'Native.', inputSchema: {type: 'object'}}];
    }
  }
  const native = new ModelContext();

  assert.equal(await script.list(fakeWindow(undefined)), null);
  assert.equal(await script.list(fakeWindow(native, native)), null);
  assert.equal(await script.list(fakeWindow(native)), null);
  assert.equal(await script.list(fakeWindow('not an object')), null);
  assert.equal(await script.list({navigator: {get modelContext() {
    throw new Error('blocked');
  }}, document: {}}), null);
  assert.deepEqual(await script.invoke(fakeWindow(undefined), 'x', {}), {
    ok: false,
    error: 'the page no longer exposes a modelContext polyfill',
  });
});

test('drops malformed entries and bounds output size', async () => {
  const script = pageScript();
  const polyfill = {
    listTools: () => [
      null,
      {name: 42},
      {name: '', description: 'empty'},
      {name: 'dup', description: 'first', inputSchema: '{"type":"object"}'},
      {name: 'dup', description: 'second'},
      {name: 'bad_schema', inputSchema: 'not json', description: 7},
      {name: 'huge', description: 'x'.repeat(300 * 1024)},
    ],
    async callTool(name: string, _input: unknown) {
      if (name === 'large') return {payload: 'x'.repeat(1024 * 1024)};
      if (name === 'cyclic') {
        const value: Record<string, unknown> = {};
        value.self = value;
        return value;
      }
      return {name};
    },
  };
  const window = fakeWindow(polyfill);

  const listed = await script.list(window);
  assert.deepEqual(listed?.tools.slice(0, 2), [
    {name: 'dup', description: 'first', inputSchema: {type: 'object'}},
    {name: 'bad_schema', description: '', inputSchema: {}},
  ]);
  assert.equal(listed?.tools.length, 3);
  assert.equal((listed?.tools[2] as {description: string}).description.length, 64 * 1024);
  assert.deepEqual(await script.invoke(window, 'large', {}), {ok: false, error: 'tool output exceeds 1 MiB'});
  const cyclic = await script.invoke(window, 'cyclic', {});
  assert.equal(cyclic.ok, false);
  assert.match(cyclic.error ?? '', /not JSON-serializable/);
});
