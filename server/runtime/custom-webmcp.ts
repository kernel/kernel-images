import { ToolSchema } from '@modelcontextprotocol/core';
import { AjvJsonSchemaValidator } from '@modelcontextprotocol/server/validators/ajv';
import type { BrowserReplCdpClient, CdpEvent, CdpTarget } from './browser-cdp-client';

const WORLD_NAME = 'kernel-custom-webmcp';
const BINDING_NAME = '__kernelCustomWebMCPInvoke';
const PAGE_RUNTIME_KEY = '__kernelCustomWebMCP';
const MAIN_PAGE_RUNTIME_KEY = '__kernelCustomWebMCPPage';
const RECONNECT_DELAY_MS = 250;

type JsonSchema = Record<string, unknown>;
type Validator = ReturnType<AjvJsonSchemaValidator['getValidator']>;

type ToolKind = 'page' | 'cdp';

interface CustomToolMatch {
  url_patterns: string[];
}

interface CustomToolMetadata {
  name: string;
  title?: string;
  description: string;
  inputSchema: JsonSchema;
  annotations?: Record<string, boolean>;
}

interface CustomToolDefinitionInput {
  id: string;
  kind: ToolKind;
  match: CustomToolMatch;
  tool: CustomToolMetadata;
  outputSchema?: JsonSchema;
  execute: (
    input: Record<string, unknown>,
    context: CustomToolExecutionContext,
  ) => unknown | Promise<unknown>;
}

interface CustomToolDefinition extends CustomToolDefinitionInput {
  revision: number;
  inputValidator: Validator;
  outputValidator?: Validator;
  pageExecuteSource?: string;
}

export interface CustomToolFrameMatch {
  frame_id: string;
  target_id: string | null;
  top_target_id: string;
  url: string;
}

export interface CustomToolExecutionContext {
  signal: AbortSignal;
  matches: CustomToolFrameMatch[];
}

interface CdpFrameTree {
  frame: {
    id: string;
    loaderId?: string;
    url?: string;
  };
  childFrames?: CdpFrameTree[];
}

interface RuntimeEvaluateResult {
  exceptionDetails?: {
    exception?: {description?: string};
    text?: string;
  };
  result?: {value?: unknown};
}

interface FrameInfo {
  id: string;
  url: string;
  children?: FrameInfo[];
}

interface PageState {
  targetId: string;
  sessionId: string;
  documentKey: string;
  contextId: number | null;
  signature: string;
  matches: Map<string, CustomToolFrameMatch[]>;
  errors: string[];
}

interface ActiveInvocation {
  controller: AbortController;
}

export interface CustomToolsSnapshot {
  repl_id: string;
  revision: number;
  source: string;
  tools: Array<{
    id: string;
    kind: ToolKind;
    match: CustomToolMatch;
    tool: CustomToolMetadata;
    outputSchema?: JsonSchema;
    revision: number;
  }>;
  installations: Array<{
    target_id: string;
    definition_ids: string[];
    errors: string[];
  }>;
}

function clone<T>(value: T): T {
  return structuredClone(value);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function formatToolSchemaError(error: { issues: Array<{ path: PropertyKey[]; message: string }> }): string {
  return error.issues
    .map((issue) => `${issue.path.length ? issue.path.join('.') : 'tool'}: ${issue.message}`)
    .join('; ');
}

function compileSchema(schema: JsonSchema, field: string): Validator {
  try {
    return new AjvJsonSchemaValidator().getValidator(schema);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new Error(`${field} is not a compilable JSON Schema: ${message}`);
  }
}

function validatePattern(pattern: unknown): asserts pattern is string {
  if (typeof pattern !== 'string' || !pattern.includes('://')) {
    throw new Error('match.url_patterns entries must be URL patterns containing ://');
  }
  const scheme = pattern.slice(0, pattern.indexOf('://'));
  if (scheme !== 'http' && scheme !== 'https' && scheme !== '*') {
    throw new Error(`unsupported URL pattern scheme: ${scheme}`);
  }
}

function patternRegexp(pattern: string): RegExp {
  const escaped = pattern.replace(/[|\\{}()[\]^$+?.]/g, '\\$&').replaceAll('*', '.*');
  return new RegExp(`^${escaped}$`);
}

function normalizedURL(url: string): string {
  const hash = url.indexOf('#');
  return hash === -1 ? url : url.slice(0, hash);
}

export function matchesURLPattern(url: string, pattern: string): boolean {
  return patternRegexp(pattern).test(normalizedURL(url));
}

function flattenFrames(frame: FrameInfo): FrameInfo[] {
  const frames = [frame];
  for (const child of frame.children ?? []) frames.push(...flattenFrames(child));
  return frames;
}

function definitionPublic(definition: CustomToolDefinition) {
  const result: CustomToolsSnapshot['tools'][number] = {
    id: definition.id,
    kind: definition.kind,
    match: clone(definition.match),
    tool: clone(definition.tool),
    revision: definition.revision,
  };
  if (definition.outputSchema) result.outputSchema = clone(definition.outputSchema);
  return result;
}

export class CustomWebMCPRegistry {
  private definitions = new Map<string, CustomToolDefinition>();
  private source = '';
  private revision = 0;
  private staging: Map<string, CustomToolDefinition> | null = null;
  private pages = new Map<string, PageState>();
  private sessions = new Map<string, string>();
  private activeInvocations = new Map<string, ActiveInvocation>();
  private reconciliation: Promise<void> | null = null;
  private reconcileAgain = false;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private disposed = false;
  private readonly unsubscribeEvent: () => void;
  private readonly unsubscribeDisconnect: () => void;
  private onError?: (message: string) => void;
  private readonly client: BrowserReplCdpClient;
  private readonly replID: string;

  constructor(client: BrowserReplCdpClient, replID: string) {
    this.client = client;
    this.replID = replID;
    this.unsubscribeEvent = client.subscribeEvents((event) => this.handleEvent(event));
    this.unsubscribeDisconnect = client.subscribeDisconnect(() => this.handleDisconnect());
    this.scheduleReconcile();
  }

  setErrorHandler(handler: (message: string) => void): void {
    this.onError = handler;
  }

  register = (input: CustomToolDefinitionInput): ReturnType<typeof definitionPublic> => {
    const destination = this.staging ?? this.definitions;
    const prior = destination.get(input.id) ?? (this.staging ? this.definitions.get(input.id) : undefined);
    const definition = this.validateDefinition(input, prior?.revision ?? 0);
    for (const existing of destination.values()) {
      if (existing.id !== definition.id && existing.tool.name === definition.tool.name) {
        throw new Error(`tool name already registered: ${definition.tool.name}`);
      }
    }
    destination.set(definition.id, definition);
    if (!this.staging) {
      this.revision++;
      this.source = '';
      this.scheduleReconcile();
    }
    return definitionPublic(definition);
  };

  remove = (id: string): boolean => {
    const destination = this.staging ?? this.definitions;
    const removed = destination.delete(id);
    if (removed && !this.staging) {
      this.revision++;
      this.source = '';
      this.scheduleReconcile();
    }
    return removed;
  };

  list = (): CustomToolsSnapshot['tools'] => {
    return [...this.definitions.values()]
      .map(definitionPublic)
      .sort((a, b) => a.id.localeCompare(b.id));
  };

  get = (id: string): CustomToolsSnapshot['tools'][number] | null => {
    const definition = this.definitions.get(id);
    return definition ? definitionPublic(definition) : null;
  };

  async replace(source: string, evaluate: (source: string) => Promise<void>): Promise<CustomToolsSnapshot> {
    if (this.staging) throw new Error('custom tool replacement is already in progress');
    this.staging = new Map();
    try {
      await evaluate(`await (async () => {\n${source}\n})()`);
      this.definitions = this.staging;
      this.source = source;
      this.revision++;
    } finally {
      this.staging = null;
    }
    try {
      await this.reconcile();
    } catch (error) {
      this.reportError(error);
      this.retryReconcile();
    }
    return this.snapshot();
  }

  async current(): Promise<CustomToolsSnapshot> {
    try {
      await this.reconcile();
    } catch (error) {
      this.reportError(error);
      this.retryReconcile();
    }
    return this.snapshot();
  }

  snapshot(): CustomToolsSnapshot {
    return {
      repl_id: this.replID,
      revision: this.revision,
      source: this.source,
      tools: this.list(),
      installations: [...this.pages.values()]
        .map((page) => ({
          target_id: page.targetId,
          definition_ids: [...page.matches.keys()].sort(),
          errors: [...page.errors],
        }))
        .sort((a, b) => a.target_id.localeCompare(b.target_id)),
    };
  }

  async dispose(): Promise<void> {
    this.disposed = true;
    this.unsubscribeEvent();
    this.unsubscribeDisconnect();
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    if (this.reconciliation) await this.reconciliation.catch(() => undefined);
    for (const invocation of this.activeInvocations.values()) invocation.controller.abort();
    this.activeInvocations.clear();
    await Promise.all([...this.pages.values()].map(async (page) => {
      await this.disposePageRuntime(page, MAIN_PAGE_RUNTIME_KEY);
      if (page.contextId !== null) await this.disposePageRuntime(page, PAGE_RUNTIME_KEY, page.contextId);
      await this.detachPage(page);
    }));
    this.pages.clear();
    this.sessions.clear();
  }

  private validateDefinition(input: CustomToolDefinitionInput, previousRevision: number): CustomToolDefinition {
    if (!input || typeof input !== 'object') throw new Error('definition must be an object');
    if (typeof input.id !== 'string' || input.id.trim() === '') {
      throw new Error('definition.id must be a non-empty string');
    }
    if (input.kind !== 'page' && input.kind !== 'cdp') {
      throw new Error('definition.kind must be page or cdp');
    }
    if (!input.match || !Array.isArray(input.match.url_patterns) || input.match.url_patterns.length === 0) {
      throw new Error('definition.match.url_patterns must be a non-empty array');
    }
    for (const pattern of input.match.url_patterns) validatePattern(pattern);
    if (typeof input.execute !== 'function') throw new Error('definition.execute must be a function');

    const toolResult = ToolSchema.safeParse({ ...input.tool, outputSchema: input.outputSchema });
    if (!toolResult.success) {
      throw new Error(`invalid MCP tool definition: ${formatToolSchemaError(toolResult.error)}`);
    }

    const inputValidator = compileSchema(input.tool.inputSchema, 'tool.inputSchema');
    const outputValidator = input.outputSchema
      ? compileSchema(input.outputSchema, 'outputSchema')
      : undefined;
    const pageExecuteSource = input.kind === 'page' ? input.execute.toString() : undefined;
    if (pageExecuteSource) {
      try {
        new Function(`return (${pageExecuteSource})`);
      } catch (error) {
        throw new Error(`definition.execute cannot be installed in the page: ${String(error)}`);
      }
    }

    return {
      id: input.id,
      kind: input.kind,
      match: clone(input.match),
      tool: clone(input.tool),
      outputSchema: input.outputSchema ? clone(input.outputSchema) : undefined,
      execute: input.execute,
      revision: previousRevision + 1,
      inputValidator,
      outputValidator,
      pageExecuteSource,
    };
  }

  private handleEvent(event: CdpEvent): void {
    if (event.method === 'Runtime.bindingCalled') {
      void this.handleBinding(event);
      return;
    }
    if (event.method === 'Target.detachedFromTarget') {
      const sessionId = (event.params as {sessionId?: string})?.sessionId;
      const targetId = sessionId ? this.sessions.get(sessionId) : undefined;
      if (sessionId && targetId) {
        this.sessions.delete(sessionId);
        this.pages.delete(targetId);
      }
    }
    if (
      event.method === 'Target.targetCreated' ||
      event.method === 'Target.targetInfoChanged' ||
      event.method === 'Target.targetDestroyed' ||
      event.method === 'Target.detachedFromTarget' ||
      event.method === 'Page.frameAttached' ||
      event.method === 'Page.frameNavigated' ||
      event.method === 'Page.frameDetached'
    ) {
      this.scheduleReconcile();
    }
  }

  private handleDisconnect(): void {
    this.pages.clear();
    this.sessions.clear();
    if (this.disposed || this.reconnectTimer) return;
    this.retryReconcile();
  }

  private scheduleReconcile(): void {
    queueMicrotask(() => void this.reconcile().catch((error) => {
      this.reportError(error);
      this.retryReconcile();
    }));
  }

  private retryReconcile(): void {
    if (this.disposed || this.reconnectTimer) return;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.scheduleReconcile();
    }, RECONNECT_DELAY_MS);
    this.reconnectTimer.unref?.();
  }

  private async reconcile(): Promise<void> {
    if (this.disposed) return;
    if (this.reconciliation) {
      this.reconcileAgain = true;
      return this.reconciliation;
    }
    this.reconciliation = (async () => {
      do {
        this.reconcileAgain = false;
        await this.reconcileOnce();
      } while (this.reconcileAgain && !this.disposed);
    })();
    try {
      await this.reconciliation;
    } finally {
      this.reconciliation = null;
    }
  }

  private async reconcileOnce(): Promise<void> {
    await this.client.ensureConnected();
    await this.client.browserCommand('Target.setDiscoverTargets', { discover: true });
    const targets = await this.client.listTargets();
    const pages = targets.filter(
      (target) => target.type === 'page' && (target.url.startsWith('https://') || target.url.startsWith('http://')),
    );
    const liveTargets = new Set(pages.map((target) => target.targetId));

    for (const [targetId, page] of this.pages) {
      if (!liveTargets.has(targetId)) {
        await this.detachPage(page);
        this.pages.delete(targetId);
        this.sessions.delete(page.sessionId);
      }
    }

    for (const target of pages) {
      try {
        await this.reconcilePage(target, targets);
      } catch (error) {
        const page = this.pages.get(target.targetId);
        if (page) page.errors = [error instanceof Error ? error.message : String(error)];
        else this.reportError(error);
      }
    }
  }

  private async reconcilePage(target: CdpTarget, targets: CdpTarget[]): Promise<void> {
    let page = this.pages.get(target.targetId);
    if (!page) {
      const attached = await this.client.browserCommand<{ sessionId: string }>('Target.attachToTarget', {
        targetId: target.targetId,
        flatten: true,
      });
      page = {
        targetId: target.targetId,
        sessionId: attached.sessionId,
        documentKey: '',
        contextId: null,
        signature: '',
        matches: new Map(),
        errors: [],
      };
      this.pages.set(target.targetId, page);
      this.sessions.set(page.sessionId, target.targetId);
      await this.client.send('Page.enable', undefined, page.sessionId);
      await this.client.send('Runtime.enable', undefined, page.sessionId);
    }

    const result = await this.client.send<{frameTree: CdpFrameTree}>('Page.getFrameTree', undefined, page.sessionId);
    const root = this.decodeFrameTree(result.frameTree);
    const frames = flattenFrames(root);
    const knownFrameIDs = new Set(frames.map((frame) => frame.id));
    const iframeTargetList = targets.filter((candidate) => candidate.type === 'iframe');
    let added = true;
    while (added) {
      added = false;
      for (const iframe of iframeTargetList) {
        if (knownFrameIDs.has(iframe.targetId) || !iframe.parentFrameId || !knownFrameIDs.has(iframe.parentFrameId)) continue;
        frames.push({id: iframe.targetId, url: iframe.url});
        knownFrameIDs.add(iframe.targetId);
        added = true;
      }
    }
    const iframeTargets = new Set(iframeTargetList.map((candidate) => candidate.targetId));
    const matches = new Map<string, CustomToolFrameMatch[]>();
    for (const definition of this.definitions.values()) {
      const matchingFrames = frames
        .filter((frame) => definition.match.url_patterns.some((pattern) => matchesURLPattern(frame.url, pattern)))
        .map((frame) => ({
          frame_id: frame.id,
          target_id: iframeTargets.has(frame.id) ? frame.id : frame.id === root.id ? target.targetId : null,
          top_target_id: target.targetId,
          url: normalizedURL(frame.url),
        }));
      if (matchingFrames.length) matches.set(definition.id, matchingFrames);
    }

    const documentKey = `${root.id}:${result.frameTree.frame.loaderId ?? ''}`;
    const signature = JSON.stringify([...matches.keys()].sort().map((id) => [id, this.definitions.get(id)!.revision]));
    const registrationCurrent = page.documentKey === documentKey && page.signature === signature && page.contextId !== null;
    page.documentKey = documentKey;
    page.matches = matches;
    if (registrationCurrent) return;

    const world = await this.client.send<{ executionContextId: number }>(
      'Page.createIsolatedWorld',
      { frameId: root.id, worldName: WORLD_NAME, grantUniveralAccess: false },
      page.sessionId,
    );
    await this.client.send(
      'Runtime.addBinding',
      { name: BINDING_NAME, executionContextName: WORLD_NAME },
      page.sessionId,
    );
    page.contextId = world.executionContextId;
    await this.disposePageRuntime(page, MAIN_PAGE_RUNTIME_KEY);
    await this.disposePageRuntime(page, PAGE_RUNTIME_KEY, world.executionContextId);

    const definitions = [...page.matches.keys()].map((id) => this.definitions.get(id)!);
    const pageErrors = await this.installPageDefinitions(
      page,
      definitions.filter((definition) => definition.kind === 'page'),
      MAIN_PAGE_RUNTIME_KEY,
    );
    const cdpErrors = await this.installPageDefinitions(
      page,
      definitions.filter((definition) => definition.kind === 'cdp'),
      PAGE_RUNTIME_KEY,
      world.executionContextId,
    );
    page.errors = [...pageErrors, ...cdpErrors];
    page.signature = signature;
  }

  private decodeFrameTree(tree: CdpFrameTree): FrameInfo {
    return {
      id: tree.frame.id,
      url: tree.frame.url ?? '',
      children: (tree.childFrames ?? []).map((child) => this.decodeFrameTree(child)),
    };
  }

  private async disposePageRuntime(page: PageState, runtimeKey: string, contextId?: number): Promise<void> {
    const params: Record<string, unknown> = {
      expression: `globalThis[${JSON.stringify(runtimeKey)}]?.dispose?.()`,
      awaitPromise: true,
    };
    if (contextId !== undefined) params.contextId = contextId;
    try {
      await this.client.send('Runtime.evaluate', params, page.sessionId, 1_000);
    } catch {
      // A navigation may already have destroyed this runtime.
    }
  }

  private async installPageDefinitions(
    page: PageState,
    definitions: CustomToolDefinition[],
    runtimeKey: string,
    contextId?: number,
  ): Promise<string[]> {
    const serialized = definitions.map((definition) => ({
      id: definition.id,
      kind: definition.kind,
      tool: definition.tool,
      revision: definition.revision,
      pageExecuteSource: definition.pageExecuteSource,
    }));
    const expression = `
      (async () => {
        const key = ${JSON.stringify(runtimeKey)};
        globalThis[key]?.dispose?.();
        const definitions = ${JSON.stringify(serialized)};
        const controllers = new Map();
        const pending = new Map();
        const errors = [];
        const resolveInvocation = (id, value) => {
          const invocation = pending.get(id);
          if (!invocation) return;
          pending.delete(id);
          invocation.resolve(value);
        };
        const rejectInvocation = (id, message) => {
          const invocation = pending.get(id);
          if (!invocation) return;
          pending.delete(id);
          invocation.reject(new Error(message));
        };
        globalThis[key] = {
          resolveInvocation,
          rejectInvocation,
          dispose() {
            for (const controller of controllers.values()) controller.abort();
            controllers.clear();
          },
        };
        for (const definition of definitions) {
          const controller = new AbortController();
          controllers.set(definition.id, controller);
          let execute;
          if (definition.kind === 'page') {
            execute = (0, eval)(\`(\${definition.pageExecuteSource})\`);
          } else {
            execute = (input, {signal} = {}) => {
              const invocationId = crypto.randomUUID();
              return new Promise((resolve, reject) => {
                pending.set(invocationId, {resolve, reject});
                signal?.addEventListener('abort', () => {
                  if (!pending.has(invocationId)) return;
                  pending.delete(invocationId);
                  globalThis[${JSON.stringify(BINDING_NAME)}](JSON.stringify({
                    type: 'cancel',
                    invocation_id: invocationId,
                  }));
                  reject(new DOMException('Canceled', 'AbortError'));
                }, {once: true});
                globalThis[${JSON.stringify(BINDING_NAME)}](JSON.stringify({
                  type: 'invoke',
                  invocation_id: invocationId,
                  definition_id: definition.id,
                  revision: definition.revision,
                  input,
                }));
              });
            };
          }
          try {
            await document.modelContext.registerTool({...definition.tool, execute}, {signal: controller.signal});
          } catch (error) {
            controller.abort();
            controllers.delete(definition.id);
            errors.push(\`\${definition.id}: \${error instanceof Error ? error.message : String(error)}\`);
          }
        }
        return errors;
      })()
    `;
    const params: Record<string, unknown> = { expression, awaitPromise: true, returnByValue: true };
    if (contextId !== undefined) params.contextId = contextId;
    const result = await this.client.send<RuntimeEvaluateResult>('Runtime.evaluate', params, page.sessionId);
    if (result.exceptionDetails) {
      throw new Error(result.exceptionDetails.exception?.description ?? result.exceptionDetails.text ?? 'tool installation failed');
    }
    return Array.isArray(result.result?.value) ? result.result.value : [];
  }

  private async handleBinding(event: CdpEvent): Promise<void> {
    const params = event.params as { name?: string; payload?: string; executionContextId?: number };
    if (params.name !== BINDING_NAME || !event.sessionId || params.executionContextId === undefined) return;
    let payload: unknown;
    try {
      payload = JSON.parse(params.payload ?? '');
    } catch {
      return;
    }
    if (!isRecord(payload)) return;
    if (payload.type === 'cancel' && typeof payload.invocation_id === 'string') {
      this.activeInvocations.get(payload.invocation_id)?.controller.abort();
      return;
    }
    if (
      payload.type !== 'invoke' ||
      typeof payload.invocation_id !== 'string' ||
      typeof payload.definition_id !== 'string' ||
      typeof payload.revision !== 'number' ||
      !payload.input ||
      typeof payload.input !== 'object' ||
      Array.isArray(payload.input)
    ) {
      return;
    }

    const targetId = this.sessions.get(event.sessionId);
    const page = targetId ? this.pages.get(targetId) : undefined;
    const definition = this.definitions.get(payload.definition_id);
    if (!targetId || !page || !definition || definition.revision !== payload.revision) {
      await this.respondToInvocation(event.sessionId, params.executionContextId, payload.invocation_id, undefined, 'custom tool is no longer registered');
      return;
    }

    const controller = new AbortController();
    this.activeInvocations.set(payload.invocation_id, {controller});
    try {
      const inputResult = definition.inputValidator(payload.input);
      if (!inputResult.valid) throw new Error(`input failed JSON Schema validation: ${inputResult.errorMessage}`);
      const output = await definition.execute(inputResult.data as Record<string, unknown>, {
        signal: controller.signal,
        matches: clone(page.matches.get(definition.id) ?? []),
      });
      if (definition.outputValidator) {
        const outputResult = definition.outputValidator(output);
        if (!outputResult.valid) throw new Error(`output failed JSON Schema validation: ${outputResult.errorMessage}`);
      }
      await this.respondToInvocation(event.sessionId, params.executionContextId, payload.invocation_id, output);
    } catch (error) {
      await this.respondToInvocation(
        event.sessionId,
        params.executionContextId,
        payload.invocation_id,
        undefined,
        error instanceof Error ? error.message : String(error),
      );
    } finally {
      this.activeInvocations.delete(payload.invocation_id);
    }
  }

  private async respondToInvocation(
    sessionId: string,
    contextId: number,
    invocationId: string,
    output?: unknown,
    error?: string,
  ): Promise<void> {
    let expression: string;
    if (error !== undefined) {
      expression = `globalThis[${JSON.stringify(PAGE_RUNTIME_KEY)}]?.rejectInvocation(${JSON.stringify(invocationId)}, ${JSON.stringify(error)})`;
    } else {
      const serialized = JSON.stringify(output ?? null);
      expression = `globalThis[${JSON.stringify(PAGE_RUNTIME_KEY)}]?.resolveInvocation(${JSON.stringify(invocationId)}, ${serialized})`;
    }
    try {
      await this.client.send('Runtime.evaluate', { expression, contextId, awaitPromise: true }, sessionId);
    } catch {
      // The registration document may have navigated after starting the tool.
    }
  }

  private async detachPage(page: PageState): Promise<void> {
    try {
      await this.client.browserCommand('Target.detachFromTarget', { sessionId: page.sessionId });
    } catch {
      // The target or connection is already gone.
    }
  }

  private reportError(error: unknown): void {
    this.onError?.(error instanceof Error ? error.message : String(error));
  }
}
