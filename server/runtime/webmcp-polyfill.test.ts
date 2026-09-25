import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// The page-side bridge that lib/webmcpclient calls on a frame's Window.
const source = readFileSync(new URL('../lib/webmcpclient/polyfill_bridge.js', import.meta.url), 'utf8');

interface Bridge {
  sync(): Promise<boolean>;
}

function bridge(window: unknown): Bridge {
  const create = vm.runInNewContext(`(${source})`, {AbortController}) as (this: unknown) => Bridge;
  return create.call(window);
}

type Tool = {name: string; execute?: (input: unknown) => unknown} & Record<string, unknown>;

// Stands in for Chromium's document.modelContext: duplicate names reject and
// aborting the registration signal removes the tool.
class ModelContext {
  readonly tools = new Map<string, Tool>();

  get [Symbol.toStringTag]() {
    return 'ModelContext';
  }

  async registerTool(tool: Tool, options: {signal?: AbortSignal} = {}) {
    if (this.tools.has(tool.name)) throw new Error(`Duplicate tool name: ${tool.name}`);
    this.tools.set(tool.name, tool);
    options.signal?.addEventListener('abort', () => this.tools.delete(tool.name), {once: true});
  }

  metadata(name: string) {
    const {execute: _execute, ...metadata} = this.tools.get(name)!;
    return JSON.parse(JSON.stringify(metadata));
  }

  async invoke(name: string, input: unknown) {
    return JSON.parse(JSON.stringify(await this.tools.get(name)!.execute!(input) ?? null));
  }
}

// Mirrors the polyfill shape that sites ship before Chromium exposed
// document.modelContext: a registry object plus listTools/callTool helpers.
function sitePolyfill() {
  const registry: Record<string, Tool> = {};
  return {
    _registeredTools: registry,
    registerTool(tool: Tool) {
      registry[tool.name] = {
        name: tool.name,
        title: tool.title,
        description: tool.description,
        inputSchema: tool.inputSchema ?? tool.parameters,
        execute: tool.execute,
      };
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

function fakeWindow(modelContext: unknown, native = new ModelContext()) {
  return {navigator: {modelContext}, document: {modelContext: native}, native};
}

test('bridges a site polyfill into the native registry and follows its changes', async () => {
  const polyfill = sitePolyfill();
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
  const {native} = window;
  const b = bridge(window);

  assert.equal(await b.sync(), true);
  assert.deepEqual([...native.tools.keys()], ['search_items', 'legacy_params', 'broken']);
  assert.deepEqual(native.metadata('search_items'), {
    name: 'search_items',
    description: 'Search the catalog.',
    inputSchema: {type: 'object', properties: {query: {type: 'string'}}, required: ['query']},
  });
  assert.deepEqual(native.metadata('legacy_params').inputSchema, {type: 'object', properties: {}});
  assert.deepEqual(await native.invoke('search_items', {query: 'lamp'}), {results: ['match for lamp']});
  assert.equal(await native.invoke('legacy_params', {}), null);
  await assert.rejects(native.invoke('broken', {}), /page rejected the call/);

  assert.equal(await b.sync(), false);

  polyfill.unregisterTool('search_items');
  polyfill.registerTool({name: 'broken', description: 'Fixed.', inputSchema: {type: 'object'}, execute: async () => 'ok'});
  assert.equal(await b.sync(), true);
  assert.deepEqual([...native.tools.keys()].sort(), ['broken', 'legacy_params']);
  assert.equal(native.metadata('broken').description, 'Fixed.');
  assert.equal(await native.invoke('broken', {}), 'ok');
});

test('supports MCP-style polyfills that take callTool({name, arguments})', async () => {
  const tools = new Map<string, Tool>();
  tools.set('add_to_cart', {
    name: 'add_to_cart',
    title: 'Add to cart',
    description: 'Add an item.',
    inputSchema: {type: 'object', properties: {sku: {type: 'string'}}},
    outputSchema: {type: 'object'},
    annotations: {readOnlyHint: false},
    execute: async () => ({content: [{type: 'text', text: 'added'}]}),
  });
  const polyfill = {
    listTools: () => ({tools: [...tools.values()].map(({execute: _execute, ...metadata}) => metadata)}),
    async callTool(params: {name: string; arguments: unknown}) {
      return tools.get(params.name)?.execute?.(params.arguments);
    },
  };
  const window = fakeWindow(polyfill);
  await bridge(window).sync();

  assert.deepEqual(window.native.metadata('add_to_cart'), {
    name: 'add_to_cart',
    title: 'Add to cart',
    description: 'Add an item.',
    inputSchema: {type: 'object', properties: {sku: {type: 'string'}}},
    outputSchema: {type: 'object'},
    annotations: {readOnlyHint: false},
  });
  assert.deepEqual(await window.native.invoke('add_to_cart', {sku: '1'}), {content: [{type: 'text', text: 'added'}]});
});

test('falls back to the registry when the polyfill has no list or call helpers', async () => {
  const registry = new Map<string, Tool>();
  registry.set('get_context', {name: 'get_context', description: 'Context.', inputSchema: {type: 'object'}, execute: async () => 'ctx'});
  const polyfill = {registerTool() {}, provideContext() {}};
  const window = {...fakeWindow(polyfill), __webmcp: {tools: registry}};
  await bridge(window).sync();

  assert.deepEqual([...window.native.tools.keys()], ['get_context']);
  assert.equal(await window.native.invoke('get_context', {}), 'ctx');
});

test('keeps native tools and ignores pages without a polyfill', async () => {
  const native = new ModelContext();
  await native.registerTool({name: 'shared', description: 'Native copy.', execute: async () => 'native'});
  const polyfill = sitePolyfill();
  polyfill.registerTool({name: 'shared', description: 'Polyfill copy.', inputSchema: {type: 'object'}, execute: async () => 'polyfill'});
  const window = fakeWindow(polyfill, native);
  assert.equal(await bridge(window).sync(), false);
  assert.equal(await native.invoke('shared', {}), 'native');

  // navigator.modelContext that is the native registry, or no polyfill at all.
  const self = new ModelContext();
  assert.equal(await bridge(fakeWindow(self, self)).sync(), false);
  assert.equal(await bridge(fakeWindow(undefined)).sync(), false);
  assert.equal(await bridge({navigator: {get modelContext() {
    throw new Error('blocked');
  }}, document: {modelContext: new ModelContext()}}).sync(), false);
  // A document whose modelContext is not the native registry.
  assert.equal(await bridge({navigator: {modelContext: sitePolyfill()}, document: {modelContext: {}}}).sync(), false);
});

test('drops malformed entries, bounds output, and resets for a new document', async () => {
  const polyfill = {
    listTools: () => [
      null,
      {name: 42},
      {name: '', description: 'empty'},
      {name: 'dup', description: 'first', inputSchema: '{"type":"object"}'},
      {name: 'dup', description: 'second'},
      {name: 'bad_schema', inputSchema: 'not json', description: 7},
      {name: 'huge', description: 'x', inputSchema: {type: 'object', enum: ['x'.repeat(300 * 1024)]}},
    ],
    async callTool(name: string, _input: unknown) {
      if (name === 'dup') return {payload: 'x'.repeat(1024 * 1024)};
      const value: Record<string, unknown> = {};
      value.self = value;
      return value;
    },
  };
  const window = fakeWindow(polyfill);
  const b = bridge(window);
  await b.sync();

  assert.deepEqual([...window.native.tools.keys()], ['dup', 'bad_schema']);
  assert.deepEqual(window.native.metadata('dup'), {name: 'dup', description: 'first', inputSchema: {type: 'object'}});
  assert.deepEqual(window.native.metadata('bad_schema'), {name: 'bad_schema', description: '', inputSchema: {type: 'object'}});
  await assert.rejects(window.native.invoke('dup', {}), /exceeds 1 MiB/);
  await assert.rejects(window.native.invoke('bad_schema', {}), /not JSON-serializable/);

  // A same-origin child frame navigated: its registry is new and empty.
  window.document = {modelContext: new ModelContext()};
  assert.equal(await b.sync(), true);
  assert.deepEqual([...(window.document.modelContext as ModelContext).tools.keys()], ['dup', 'bad_schema']);
});
