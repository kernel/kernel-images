// Discovers and invokes tools that a page registers through a JavaScript
// navigator.modelContext polyfill instead of the native document.modelContext
// registry that the CDP WebMCP domain reports. Evaluated in the page's main
// world with the frame's Window passed in; it stores nothing on the page.
(() => {
  const MAX_TOOLS = 256;
  const MAX_TEXT = 64 * 1024;
  const MAX_TOOL_BYTES = 256 * 1024;
  const MAX_OUTPUT_BYTES = 1024 * 1024;
  const HINTS = [
    'readOnlyHint',
    'destructiveHint',
    'idempotentHint',
    'openWorldHint',
    'consequentialHint',
    'untrustedContentHint',
    'autosubmit',
  ];

  function isObject(value) {
    return typeof value === 'object' && value !== null;
  }

  function isFunction(value) {
    return typeof value === 'function';
  }

  function message(error) {
    try {
      return isObject(error) && typeof error.message === 'string' ? error.message : String(error);
    } catch {
      return 'unknown error';
    }
  }

  function text(value, fallback) {
    if (typeof value !== 'string') return fallback;
    return value.length > MAX_TEXT ? value.slice(0, MAX_TEXT) : value;
  }

  // Returns the page-created object on navigator.modelContext, or null when the
  // property is absent or is the native registry that CDP already reports.
  function polyfill(window) {
    let context;
    try {
      context = window.navigator.modelContext;
    } catch {
      return null;
    }
    if (!isObject(context) && !isFunction(context)) return null;
    let native;
    try {
      native = window.document.modelContext;
    } catch {
      native = undefined;
    }
    if (context === native) return null;
    try {
      if (Object.prototype.toString.call(context) === '[object ModelContext]') return null;
    } catch {
      return null;
    }
    return context;
  }

  function mapLike(value) {
    return isObject(value) && isFunction(value.values) && isFunction(value.get);
  }

  // Tool registries that polyfills keep next to their public methods.
  function registryEntries(context, window) {
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
      // Same as above.
    }
    for (const registry of candidates) {
      try {
        if (mapLike(registry)) return Array.from(registry.values());
        if (Array.isArray(registry)) return registry;
        if (isObject(registry)) return Object.values(registry);
      } catch {
        continue;
      }
    }
    return null;
  }

  async function listedTools(context, window) {
    for (const method of ['listTools', 'getTools']) {
      let fn;
      try {
        fn = context[method];
      } catch {
        continue;
      }
      if (!isFunction(fn)) continue;
      try {
        const tools = await fn.call(context);
        if (Array.isArray(tools)) return tools;
      } catch {
        // Fall through to the next source.
      }
    }
    return registryEntries(context, window);
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

  function metadata(tool) {
    try {
      if (!isObject(tool)) return null;
      const name = tool.name;
      if (typeof name !== 'string' || name === '' || name.length > 256) return null;
      const entry = {name, description: text(tool.description, '')};
      const title = text(tool.title, undefined);
      if (title !== undefined) entry.title = title;
      // Some polyfills accept the pre-standard `parameters` alias for inputSchema.
      entry.inputSchema = schema(tool.inputSchema) ?? schema(tool.parameters) ?? {};
      const outputSchema = schema(tool.outputSchema);
      if (outputSchema) entry.outputSchema = outputSchema;
      if (isObject(tool.annotations)) {
        const annotations = {};
        for (const hint of HINTS) {
          if (typeof tool.annotations[hint] === 'boolean') annotations[hint] = tool.annotations[hint];
        }
        if (Object.keys(annotations).length) entry.annotations = annotations;
      }
      const serialized = JSON.stringify(entry);
      if (serialized.length > MAX_TOOL_BYTES) return null;
      return JSON.parse(serialized);
    } catch {
      return null;
    }
  }

  async function list(window) {
    const context = polyfill(window);
    if (!context) return null;
    const tools = await listedTools(context, window);
    if (!Array.isArray(tools)) return null;
    const seen = new Set();
    const result = [];
    for (const tool of tools) {
      if (result.length >= MAX_TOOLS) break;
      const entry = metadata(tool);
      if (!entry || seen.has(entry.name)) continue;
      seen.add(entry.name);
      result.push(entry);
    }
    return {tools: result};
  }

  function registryEntry(context, window, name) {
    const entries = registryEntries(context, window);
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

  async function execute(context, window, name, input) {
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
    const entry = registryEntry(context, window, name);
    if (entry) return entry.execute(input);
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

  function serialize(output) {
    if (output === undefined) return null;
    let serialized;
    try {
      serialized = JSON.stringify(output);
    } catch (error) {
      throw new Error(`tool output is not JSON-serializable: ${message(error)}`);
    }
    if (serialized === undefined) return null;
    if (serialized.length > MAX_OUTPUT_BYTES) throw new Error('tool output exceeds 1 MiB');
    return serialized;
  }

  async function invoke(window, name, input) {
    const context = polyfill(window);
    if (!context) return {ok: false, error: 'the page no longer exposes a modelContext polyfill'};
    try {
      return {ok: true, output: serialize(await execute(context, window, name, input))};
    } catch (error) {
      return {ok: false, error: text(message(error), 'tool execution failed')};
    }
  }

  return {list, invoke};
})()
