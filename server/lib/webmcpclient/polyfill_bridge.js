// Copies tools that a page registers through a JavaScript
// navigator.modelContext polyfill into the frame's native document.modelContext
// registry, where the CDP WebMCP domain lists and invokes them like any other
// page tool. Called with a frame's Window as `this`; the returned bridge is
// reachable only through the caller's remote object handle, so nothing is
// stored on the page.
function () {
  const MAX_TOOLS = 256;
  const MAX_TEXT = 64 * 1024;
  const MAX_TOOL_BYTES = 256 * 1024;
  const MAX_LIST_BYTES = 1024 * 1024;
  const MAX_OUTPUT_BYTES = 1024 * 1024;
  const window = this;

  function isObject(value) {
    return typeof value === 'object' && value !== null;
  }

  function isFunction(value) {
    return typeof value === 'function';
  }

  function isNative(context) {
    try {
      return Object.prototype.toString.call(context) === '[object ModelContext]';
    } catch {
      return false;
    }
  }

  function nativeContext() {
    try {
      const context = window.document.modelContext;
      return isNative(context) ? context : null;
    } catch {
      return null;
    }
  }

  // The page-created object on navigator.modelContext, or null when it is
  // absent or is the native registry itself.
  function polyfill() {
    let context;
    try {
      context = window.navigator.modelContext;
    } catch {
      return null;
    }
    if (!isObject(context) && !isFunction(context)) return null;
    if (isNative(context) || context === nativeContext()) return null;
    return context;
  }

  function currentDocument() {
    try {
      return window.document;
    } catch {
      return null;
    }
  }

  // Tool registries that polyfills keep next to their public methods.
  function registryEntries(context) {
    const candidates = [];
    for (const key of ['_registeredTools', '_tools', 'tools']) {
      try {
        candidates.push(context[key]);
      } catch {
        // A throwing accessor is not a registry.
      }
    }
    try {
      candidates.push(window.__webmcp && window.__webmcp.tools);
    } catch {
      // Not a registry either.
    }
    for (const registry of candidates) {
      try {
        if (isObject(registry) && isFunction(registry.values) && isFunction(registry.get)) {
          return Array.from(registry.values());
        }
        if (Array.isArray(registry)) return registry;
        if (isObject(registry)) return Object.values(registry);
      } catch {
        continue;
      }
    }
    return null;
  }

  async function listedTools(context) {
    for (const method of ['listTools', 'getTools']) {
      let fn;
      try {
        fn = context[method];
      } catch {
        continue;
      }
      if (!isFunction(fn)) continue;
      try {
        const result = await fn.call(context);
        if (Array.isArray(result)) return result;
        if (isObject(result) && Array.isArray(result.tools)) return result.tools;
      } catch {
        // Fall through to the next source.
      }
    }
    return registryEntries(context);
  }

  function text(value) {
    if (typeof value !== 'string') return undefined;
    return value.length > MAX_TEXT ? value.slice(0, MAX_TEXT) : value;
  }

  function schema(value) {
    if (typeof value === 'string') {
      try {
        value = JSON.parse(value);
      } catch {
        return undefined;
      }
    }
    return isObject(value) && !Array.isArray(value) ? value : undefined;
  }

  // A JSON copy of the tool's metadata, so nothing page-owned reaches the
  // native registry except the data itself.
  function metadata(tool) {
    try {
      if (!isObject(tool)) return null;
      const name = tool.name;
      if (typeof name !== 'string' || name === '' || name.length > 256) return null;
      const entry = {
        name,
        title: text(tool.title),
        description: text(tool.description) ?? '',
        // Some polyfills accept the pre-standard `parameters` alias for inputSchema.
        inputSchema: schema(tool.inputSchema) ?? schema(tool.parameters) ?? {type: 'object'},
        outputSchema: schema(tool.outputSchema),
        annotations: isObject(tool.annotations) ? tool.annotations : undefined,
      };
      const serialized = JSON.stringify(entry);
      if (typeof serialized !== 'string' || serialized.length > MAX_TOOL_BYTES) return null;
      return {entry: JSON.parse(serialized), serialized};
    } catch {
      return null;
    }
  }

  async function desiredTools(context) {
    const tools = await listedTools(context);
    const desired = new Map();
    if (!Array.isArray(tools)) return desired;
    let bytes = 0;
    for (const tool of tools) {
      if (desired.size >= MAX_TOOLS) break;
      const item = metadata(tool);
      if (!item || desired.has(item.entry.name)) continue;
      if (bytes + item.serialized.length > MAX_LIST_BYTES) break;
      bytes += item.serialized.length;
      desired.set(item.entry.name, item);
    }
    return desired;
  }

  function registryEntry(context, name) {
    const entries = registryEntries(context);
    if (!entries) return null;
    for (const entry of entries) {
      try {
        if (isObject(entry) && entry.name === name && isFunction(entry.execute)) return entry;
      } catch {
        continue;
      }
    }
    return null;
  }

  async function run(context, name, input) {
    // The registry entry's own execute is unambiguous; callTool's shape is not.
    const entry = registryEntry(context, name);
    if (entry) return entry.execute(input);
    let callTool;
    try {
      callTool = context.callTool;
    } catch {
      callTool = undefined;
    }
    if (isFunction(callTool)) {
      // Site polyfills take (name, input); MCP-style polyfills take ({name, arguments}).
      if (callTool.length >= 2) return callTool.call(context, name, input);
      return callTool.call(context, {name, arguments: input});
    }
    let executeTool;
    try {
      executeTool = context.executeTool;
    } catch {
      executeTool = undefined;
    }
    if (isFunction(executeTool)) {
      const output = await executeTool.call(context, name, JSON.stringify(input));
      if (typeof output !== 'string') return output;
      try {
        return JSON.parse(output);
      } catch {
        return output;
      }
    }
    throw new Error('the modelContext polyfill does not expose a way to execute tools');
  }

  function plain(output) {
    if (output === undefined) return undefined;
    let serialized;
    try {
      serialized = JSON.stringify(output);
    } catch {
      throw new Error('tool output is not JSON-serializable');
    }
    if (serialized === undefined) return undefined;
    if (typeof serialized !== 'string') throw new Error('tool output is not JSON-serializable');
    if (serialized.length > MAX_OUTPUT_BYTES) throw new Error('tool output exceeds 1 MiB');
    return JSON.parse(serialized);
  }

  function execute(name) {
    return async (input) => {
      const context = polyfill();
      if (!context) throw new Error('the page no longer exposes a modelContext polyfill');
      return plain(await run(context, name, input));
    };
  }

  // name -> {controller, serialized} for tools registered in `bridgedDocument`.
  const bridged = new Map();
  let bridgedDocument = null;

  function unregister(name) {
    bridged.get(name).controller.abort();
    bridged.delete(name);
  }

  return {
    // Registers new polyfill tools, unregisters removed or changed ones, and
    // reports whether the native registry changed.
    async sync() {
      const document = currentDocument();
      if (document !== bridgedDocument) {
        // A same-origin child frame navigated; its registrations went with it.
        bridged.clear();
        bridgedDocument = document;
      }
      const native = nativeContext();
      const context = polyfill();
      const desired = native && context ? await desiredTools(context) : new Map();
      let changed = false;
      for (const [name, registration] of bridged) {
        if (desired.get(name)?.serialized !== registration.serialized) {
          unregister(name);
          changed = true;
        }
      }
      for (const [name, item] of desired) {
        if (bridged.has(name)) continue;
        const controller = new AbortController();
        try {
          // Rejects when the page already registered the name natively, which
          // keeps the native tool.
          await native.registerTool({...item.entry, execute: execute(name)}, {signal: controller.signal});
        } catch {
          controller.abort();
          continue;
        }
        bridged.set(name, {controller, serialized: item.serialized});
        changed = true;
      }
      return changed;
    },
  };
}
