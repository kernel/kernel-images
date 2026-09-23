import { ToolSchema } from '@modelcontextprotocol/core';
import { AjvJsonSchemaValidator } from '@modelcontextprotocol/server/validators/ajv';

export const MAX_OUTPUT_BYTES = 1 << 20;
const CUSTOM_TOOL_NAME_PREFIX = 'custom.';
const CUSTOM_TOOL_NAMESPACE_PATTERN = /^[A-Za-z0-9_.-]{1,128}$/;

type JsonSchema = Record<string, unknown>;
type Validator = ReturnType<AjvJsonSchemaValidator['getValidator']>;
export type ToolKind = 'page' | 'cdp';

export interface CustomToolMatch {
  url_patterns: string[];
}

export interface CustomToolMetadata {
  name: string;
  title?: string;
  description: string;
  inputSchema: JsonSchema;
  outputSchema?: JsonSchema;
  annotations?: Record<string, boolean>;
}

export interface CustomToolFrameMatch {
  frame_id: string;
  session_id: string;
  target_id: string | null;
  top_target_id: string;
  url: string;
}

export interface CustomToolExecutionContext {
  signal: AbortSignal;
  matches: CustomToolFrameMatch[];
}

export interface CustomToolDefinitionInput {
  kind: ToolKind;
  match: CustomToolMatch;
  tool: CustomToolMetadata;
  execute: (
    input: Record<string, unknown>,
    context: CustomToolExecutionContext,
  ) => unknown | Promise<unknown>;
}

export interface CustomToolDefinition extends CustomToolDefinitionInput {
  id: string;
  namespace: string;
  registeredName: string;
  revision: number;
  inputValidator: Validator;
  outputValidator?: Validator;
  pageExecuteSource?: string;
}

export interface CustomToolSummary {
  id: string;
  namespace: string;
  kind: ToolKind;
  match: CustomToolMatch;
  tool: CustomToolMetadata;
}

export interface AddCustomToolsInput {
  namespace: string;
  tools: CustomToolDefinitionInput[];
  forceOverwriteNamespace?: boolean;
}

export function clone<T>(value: T): T {
  return structuredClone(value);
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function registeredToolName(id: string, name: string): string {
  const registeredName = `${CUSTOM_TOOL_NAME_PREFIX}${id}.${name}`;
  if (registeredName.length > 128) {
    throw new Error(`tool.name is too long after custom namespacing: ${name}`);
  }
  return registeredName;
}

class CustomToolConflictError extends Error {
  readonly code = 'custom_tool_conflict';

  constructor(message: string) {
    super(message);
    this.name = 'CustomToolConflictError';
  }
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

interface ParsedURLPattern {
  scheme: 'http' | 'https' | '*';
  host: string;
  path: RegExp;
}

function wildcardRegexp(pattern: string): RegExp {
  const escaped = pattern.replace(/[|\\{}()[\]^$+?.]/g, '\\$&').replaceAll('*', '.*');
  return new RegExp(`^${escaped}$`);
}

function parseURLPattern(pattern: unknown): ParsedURLPattern {
  if (typeof pattern !== 'string') {
    throw new Error('match.url_patterns entries must be strings');
  }
  const separator = pattern.indexOf('://');
  if (separator === -1) {
    throw new Error('match.url_patterns entries must contain ://');
  }
  const scheme = pattern.slice(0, separator);
  if (scheme !== 'http' && scheme !== 'https' && scheme !== '*') {
    throw new Error(`unsupported URL pattern scheme: ${scheme}`);
  }
  const remainder = pattern.slice(separator + 3);
  const pathStart = remainder.indexOf('/');
  if (pathStart === -1) {
    throw new Error('match.url_patterns entries must include a path');
  }
  const host = remainder.slice(0, pathStart).toLowerCase();
  if (!host || (host.includes('*') && host !== '*' && !host.startsWith('*.')) || host.slice(2).includes('*')) {
    throw new Error(`unsupported URL pattern host: ${host}`);
  }
  return {scheme, host, path: wildcardRegexp(remainder.slice(pathStart))};
}

export function normalizedURL(url: string): string {
  const hash = url.indexOf('#');
  return hash === -1 ? url : url.slice(0, hash);
}

export function matchesURLPattern(url: string, pattern: string): boolean {
  const parsed = parseURLPattern(pattern);
  let candidate: URL;
  try {
    candidate = new URL(normalizedURL(url));
  } catch {
    return false;
  }
  const scheme = candidate.protocol.slice(0, -1);
  if (parsed.scheme === '*' ? scheme !== 'http' && scheme !== 'https' : scheme !== parsed.scheme) return false;

  const host = candidate.host.toLowerCase();
  let hostMatches: boolean;
  if (parsed.host === '*') {
    hostMatches = true;
  } else if (parsed.host.startsWith('*.')) {
    const suffix = parsed.host.slice(2);
    hostMatches = host === suffix || host.endsWith(`.${suffix}`);
  } else {
    hostMatches = host === parsed.host;
  }
  return hostMatches && parsed.path.test(`${candidate.pathname}${candidate.search}`);
}

function definitionPublic(definition: CustomToolDefinition): CustomToolSummary {
  return {
    id: definition.id,
    namespace: definition.namespace,
    kind: definition.kind,
    match: clone(definition.match),
    tool: clone(definition.tool),
  };
}

export class CustomToolDefinitions {
  private definitions = new Map<string, CustomToolDefinition>();
  private revision = 0;
  private readonly publish: (tools: CustomToolSummary[]) => void;

  constructor(publish: (tools: CustomToolSummary[]) => void) {
    this.publish = publish;
  }

  get size(): number { return this.definitions.size; }
  get(id: string): CustomToolDefinition | undefined { return this.definitions.get(id); }
  values(): MapIterator<CustomToolDefinition> { return this.definitions.values(); }
  isCDP(id: string): boolean { return this.definitions.get(id)?.kind === 'cdp'; }
  hasCDP(): boolean {
    for (const definition of this.definitions.values()) {
      if (definition.kind === 'cdp') return true;
    }
    return false;
  }

  list(): CustomToolSummary[] {
    return [...this.definitions.values()]
      .map(definitionPublic)
      .sort((a, b) => a.id.localeCompare(b.id));
  }

  async add(input: AddCustomToolsInput): Promise<CustomToolSummary[]> {
    if (!input || typeof input !== 'object') throw new Error('custom tool batch must be an object');
    if (typeof input.namespace !== 'string' || !CUSTOM_TOOL_NAMESPACE_PATTERN.test(input.namespace)) {
      throw new Error('namespace must be 1-128 ASCII letters, numbers, dots, underscores, or hyphens');
    }
    if (!Array.isArray(input.tools) || input.tools.length === 0) {
      throw new Error('tools must be a non-empty array');
    }
    if (input.forceOverwriteNamespace !== undefined && typeof input.forceOverwriteNamespace !== 'boolean') {
      throw new Error('forceOverwriteNamespace must be a boolean');
    }

    const {createId} = await import('@paralleldrive/cuid2');
    const nextDefinitions = new Map(this.definitions);
    if (input.forceOverwriteNamespace) {
      for (const definition of nextDefinitions.values()) {
        if (definition.namespace === input.namespace) nextDefinitions.delete(definition.id);
      }
    }
    const occupied = new Set(
      [...nextDefinitions.values()].map((definition) => `${definition.namespace}\u0000${definition.tool.name}`),
    );
    const additions: CustomToolDefinition[] = [];
    for (const tool of input.tools) {
      const name = isRecord(tool) && isRecord(tool.tool) && typeof tool.tool.name === 'string'
        ? tool.tool.name
        : '';
      const key = `${input.namespace}\u0000${name}`;
      if (occupied.has(key)) {
        throw new CustomToolConflictError(`custom tool already exists: ${input.namespace}/${name}`);
      }
      occupied.add(key);
      let id: string;
      do id = `ct_${createId()}`; while (this.definitions.has(id) || additions.some((item) => item.id === id));
      additions.push(this.validateDefinition(tool, id, input.namespace, this.revision + 1));
    }

    for (const definition of additions) nextDefinitions.set(definition.id, definition);
    const previousDefinitions = this.definitions;
    this.definitions = nextDefinitions;
    try {
      this.publish(this.list());
    } catch (error) {
      this.definitions = previousDefinitions;
      throw error;
    }
    this.revision++;
    return additions.map(definitionPublic);
  }

  remove(id: string): boolean {
    const definition = this.definitions.get(id);
    if (!definition) return false;
    this.definitions.delete(id);
    try {
      this.publish(this.list());
    } catch (error) {
      this.definitions.set(id, definition);
      throw error;
    }
    this.revision++;
    return true;
  }

  private validateDefinition(input: CustomToolDefinitionInput, id: string, namespace: string, revision: number): CustomToolDefinition {
    if (!input || typeof input !== 'object') throw new Error('definition must be an object');
    if (input.kind !== 'page' && input.kind !== 'cdp') {
      throw new Error('definition.kind must be page or cdp');
    }
    if (!input.match || !Array.isArray(input.match.url_patterns) || input.match.url_patterns.length === 0) {
      throw new Error('definition.match.url_patterns must be a non-empty array');
    }
    for (const pattern of input.match.url_patterns) parseURLPattern(pattern);
    if (typeof input.execute !== 'function') throw new Error('definition.execute must be a function');

    const toolResult = ToolSchema.safeParse(input.tool);
    if (!toolResult.success) {
      throw new Error(`invalid MCP tool definition: ${formatToolSchemaError(toolResult.error)}`);
    }
    if (input.tool.outputSchema !== undefined && !isRecord(input.tool.outputSchema)) {
      throw new Error('tool.outputSchema must be an object');
    }

    const inputValidator = compileSchema(input.tool.inputSchema, 'tool.inputSchema');
    const outputValidator = input.tool.outputSchema === undefined
      ? undefined
      : compileSchema(input.tool.outputSchema, 'tool.outputSchema');
    const pageExecuteSource = input.kind === 'page' ? input.execute.toString() : undefined;
    if (pageExecuteSource) {
      try {
        new Function(`return (${pageExecuteSource})`);
      } catch (error) {
        throw new Error(`definition.execute cannot be installed in the page: ${String(error)}`);
      }
    }

    return {
      id,
      namespace,
      registeredName: registeredToolName(id, input.tool.name),
      kind: input.kind,
      match: clone(input.match),
      tool: clone(input.tool),
      execute: input.execute,
      revision,
      inputValidator,
      outputValidator,
      pageExecuteSource,
    };
  }
}
