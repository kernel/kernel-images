import type { BrowserReplCdpClient, CdpEvent } from './browser-cdp-client';
import { clone, isRecord, MAX_OUTPUT_BYTES } from './custom-webmcp-definitions.ts';
import type { CustomToolDefinition, CustomToolFrameMatch } from './custom-webmcp-definitions.ts';

export const PAGE_RUNTIME_KEY = '__kernelCustomWebMCP';
export const MAIN_PAGE_RUNTIME_KEY = '__kernelCustomWebMCPPage';
export const BINDING_NAME = '__kernelCustomWebMCPInvoke';
const RECONCILE_COMMAND_TIMEOUT_MS = 5_000;
const MAX_ERROR_BYTES = 64 << 10;

interface RuntimeEvaluateResult {
  exceptionDetails?: {
    exception?: {description?: string};
    text?: string;
  };
  result?: {value?: unknown};
}

export function errorMessage(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error);
  if (Buffer.byteLength(message) <= MAX_ERROR_BYTES) return message;
  return `${Buffer.from(message).subarray(0, MAX_ERROR_BYTES).toString('utf8')}...[truncated]`;
}

export class CustomWebMCPPageRuntime {
  private activeInvocations = new Map<string, AbortController>();
  private readonly client: BrowserReplCdpClient;
  private readonly runInvocation: <T>(signal: AbortSignal, callback: () => Promise<T>) => Promise<T>;
  private readonly resolve: (sessionId: string, id: string, revision: number) =>
    {definition: CustomToolDefinition; matches: CustomToolFrameMatch[]} | undefined;

  constructor(
    client: BrowserReplCdpClient,
    runInvocation: <T>(signal: AbortSignal, callback: () => Promise<T>) => Promise<T>,
    resolve: (sessionId: string, id: string, revision: number) =>
      {definition: CustomToolDefinition; matches: CustomToolFrameMatch[]} | undefined,
  ) {
    this.client = client;
    this.runInvocation = runInvocation;
    this.resolve = resolve;
  }

  abortAll(): void {
    for (const controller of this.activeInvocations.values()) controller.abort();
    this.activeInvocations.clear();
  }

  async invokeCDP(
    definition: CustomToolDefinition,
    matches: CustomToolFrameMatch[],
    input: Record<string, unknown>,
    signal?: AbortSignal,
  ): Promise<unknown> {
    const inputResult = definition.inputValidator(input);
    if (!inputResult.valid) throw new Error(`input failed JSON Schema validation: ${inputResult.errorMessage}`);
    const executionSignal = signal ?? new AbortController().signal;
    const output = await this.runInvocation(executionSignal, async () => definition.execute(
      inputResult.data as Record<string, unknown>,
      {signal: executionSignal, matches: clone(matches)},
    ));
    if (definition.outputValidator) {
      const outputResult = definition.outputValidator(output);
      if (!outputResult.valid) throw new Error(`output failed JSON Schema validation: ${outputResult.errorMessage}`);
    }
    const result = output ?? null;
    if (Buffer.byteLength(JSON.stringify(result)) > MAX_OUTPUT_BYTES) {
      throw new Error('custom WebMCP output exceeds 1 MiB');
    }
    return result;
  }

  async dispose(sessionId: string, runtimeKey: string, contextId?: number): Promise<boolean> {
    const params: Record<string, unknown> = {
      expression: `globalThis[${JSON.stringify(runtimeKey)}]?.dispose?.()`,
      awaitPromise: true,
    };
    if (contextId !== undefined) params.contextId = contextId;
    try {
      await this.client.send('Runtime.evaluate', params, sessionId, 1_000);
      return true;
    } catch (error) {
      const message = errorMessage(error);
      return message.includes('Cannot find context') ||
        message.includes('No target with given id') ||
        message.includes('Session with given id not found');
    }
  }

  async install(
    sessionId: string,
    definitions: CustomToolDefinition[],
    runtimeKey: string,
    contextId?: number,
  ): Promise<string[]> {
    const definitionsSource = definitions.map((definition) => {
      const metadata = JSON.stringify({
        id: definition.id,
        kind: definition.kind,
        tool: {...definition.tool, name: definition.registeredName},
        revision: definition.revision,
      });
      if (definition.kind === 'cdp') return metadata;
      return `${metadata.slice(0, -1)},"execute":(${definition.pageExecuteSource})}`;
    }).join(',');
    const expression = `
      (async () => {
        const key = ${JSON.stringify(runtimeKey)};
        const definitions = [${definitionsSource}];
        let runtime = globalThis[key];
        if (!runtime) {
          const controllers = new Map();
          const pending = new Map();
          const revisions = new Map();
          runtime = {
            controllers,
            pending,
            revisions,
            resolveInvocation(id, value) {
              const invocation = pending.get(id);
              if (!invocation) return;
              pending.delete(id);
              invocation.resolve(value);
            },
            rejectInvocation(id, message) {
              const invocation = pending.get(id);
              if (!invocation) return;
              pending.delete(id);
              invocation.reject(new Error(message));
            },
            dispose() {
              for (const controller of controllers.values()) controller.abort();
              controllers.clear();
              revisions.clear();
            },
          };
          globalThis[key] = runtime;
        }
        const {controllers, pending, revisions} = runtime;
        const desiredRevisions = new Map(definitions.map(definition => [definition.id, definition.revision]));
        for (const [id, controller] of controllers) {
          if (desiredRevisions.get(id) !== revisions.get(id)) {
            controller.abort();
            controllers.delete(id);
            revisions.delete(id);
          }
        }
        const errors = [];
        for (const definition of definitions) {
          if (controllers.has(definition.id)) continue;
          const controller = new AbortController();
          controllers.set(definition.id, controller);
          revisions.set(definition.id, definition.revision);
          let execute;
          if (definition.kind === 'page') {
            execute = definition.execute;
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
            revisions.delete(definition.id);
            errors.push(\`\${definition.id}: \${error instanceof Error ? error.message : String(error)}\`);
          }
        }
        return errors;
      })()
    `;
    const params: Record<string, unknown> = { expression, awaitPromise: true, returnByValue: true };
    if (contextId !== undefined) params.contextId = contextId;
    const result = await this.client.send<RuntimeEvaluateResult>(
      'Runtime.evaluate',
      params,
      sessionId,
      RECONCILE_COMMAND_TIMEOUT_MS,
    );
    if (result.exceptionDetails) {
      throw new Error(result.exceptionDetails.exception?.description ?? result.exceptionDetails.text ?? 'tool installation failed');
    }
    if (!Array.isArray(result.result?.value)) throw new Error('tool installation returned no result');
    return result.result.value;
  }

  async handleBinding(event: CdpEvent): Promise<void> {
    const params = event.params as { name?: string; payload?: string; executionContextId?: number };
    if (params.name !== BINDING_NAME || !event.sessionId || params.executionContextId === undefined) return;
    let payload: unknown;
    try {
      payload = JSON.parse(params.payload ?? '');
    } catch {
      return;
    }
    if (!isRecord(payload)) return;
    const message = payload;
    if (message.type === 'cancel' && typeof message.invocation_id === 'string') {
      this.activeInvocations.get(message.invocation_id)?.abort();
      return;
    }
    if (
      message.type !== 'invoke' ||
      typeof message.invocation_id !== 'string' ||
      typeof message.definition_id !== 'string' ||
      typeof message.revision !== 'number' ||
      !isRecord(message.input)
    ) {
      return;
    }

    const invocation = this.resolve(event.sessionId, message.definition_id, message.revision);
    if (!invocation) {
      await this.respond(event.sessionId, params.executionContextId, message.invocation_id, undefined, 'custom tool is no longer registered');
      return;
    }

    const controller = new AbortController();
    this.activeInvocations.set(message.invocation_id, controller);
    try {
      const output = await this.invokeCDP(invocation.definition, invocation.matches, message.input, controller.signal);
      await this.respond(event.sessionId, params.executionContextId, message.invocation_id, output);
    } catch (error) {
      await this.respond(event.sessionId, params.executionContextId, message.invocation_id, undefined, errorMessage(error));
    } finally {
      this.activeInvocations.delete(message.invocation_id);
    }
  }

  private async respond(sessionId: string, contextId: number, invocationId: string, output?: unknown, error?: string): Promise<void> {
    let expression: string;
    if (error !== undefined) {
      expression = `globalThis[${JSON.stringify(PAGE_RUNTIME_KEY)}]?.rejectInvocation(${JSON.stringify(invocationId)}, ${JSON.stringify(error)})`;
    } else {
      const serialized = JSON.stringify(output ?? null);
      if (Buffer.byteLength(serialized) > MAX_OUTPUT_BYTES) {
        throw new Error('custom WebMCP output exceeds 1 MiB');
      }
      expression = `globalThis[${JSON.stringify(PAGE_RUNTIME_KEY)}]?.resolveInvocation(${JSON.stringify(invocationId)}, ${serialized})`;
    }
    try {
      await this.client.send('Runtime.evaluate', { expression, contextId, awaitPromise: true }, sessionId);
    } catch {
      // The registration document may have navigated after starting the tool.
    }
  }
}
