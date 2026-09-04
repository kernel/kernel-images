export interface WebMCPToolFrame {
  frame_id: number;
  url: string;
}

export interface WebMCPToolSource {
  window_id: number;
  tab_id: number;
  page_title: string;
  page_url: string;
  frame: WebMCPToolFrame | null;
}

export interface WebMCPTool {
  tool_ref: string;
  name: string;
  description: string;
  input_schema: Record<string, unknown>;
  annotations?: Record<string, boolean>;
  source: WebMCPToolSource;
}

export interface WebMCPInvocationResult {
  invocation_id: string;
  status: 'completed' | 'canceled' | 'error' | 'awaiting_submission';
  output?: unknown;
  error_text?: string;
}

export interface WebMCPInvokeOptions {
  timeoutSec?: number;
}

export interface WebMCPClient {
  listTools(): Promise<WebMCPTool[]>;
  invokeTool(
    toolRef: string,
    input?: Record<string, unknown>,
    options?: WebMCPInvokeOptions,
  ): Promise<WebMCPInvocationResult>;
}

export class WebMCPRequestError extends Error {
  readonly statusCode: number;
  readonly code?: string;
  readonly invocationId?: string;
  readonly body: unknown;

  constructor(statusCode: number, body: unknown) {
    const fields = isRecord(body) ? body : {};
    const code = typeof fields.code === 'string' ? fields.code : undefined;
    const invocationId =
      typeof fields.invocation_id === 'string' ? fields.invocation_id : undefined;
    const detail =
      typeof fields.message === 'string'
        ? fields.message
        : `request failed with status ${statusCode}`;
    // The Playwright daemon only forwards error.message, so the message itself
    // must identify this as a WebMCP failure and carry the code and invocation.
    const parts = [`WebMCP ${code ?? 'error'}`];
    if (invocationId) parts.push(`invocation ${invocationId}`);
    super(`${parts.join(', ')}: ${detail}`);
    this.name = 'WebMCPRequestError';
    this.statusCode = statusCode;
    this.code = code;
    this.invocationId = invocationId;
    this.body = body;
  }
}

interface WebMCPClientOptions {
  apiBaseUrl: string;
  signal?: AbortSignal;
  fetchImpl?: typeof fetch;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

async function responseBody(response: Response): Promise<unknown> {
  const text = await response.text();
  if (text === '') return null;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

export function createWebMCPClient({
  apiBaseUrl,
  signal,
  fetchImpl = fetch,
}: WebMCPClientOptions): WebMCPClient {
  const baseUrl = apiBaseUrl.replace(/\/+$/, '');

  const request = async (path: string, init?: RequestInit): Promise<unknown> => {
    const response = await fetchImpl(`${baseUrl}${path}`, {...init, signal});
    const body = await responseBody(response);
    if (!response.ok) throw new WebMCPRequestError(response.status, body);
    return body;
  };

  const client: WebMCPClient = {
    async listTools() {
      const body = await request('/webmcp/tools');
      if (!isRecord(body) || !Array.isArray(body.tools)) {
        throw new Error('WebMCP tools response is invalid');
      }
      return body.tools as WebMCPTool[];
    },

    async invokeTool(toolRef, input = {}, options = {}) {
      const payload: Record<string, unknown> = {tool_ref: toolRef, input};
      if (options.timeoutSec !== undefined) payload.timeout_sec = options.timeoutSec;
      const body = await request('/webmcp/invoke', {
        method: 'POST',
        headers: {'content-type': 'application/json'},
        body: JSON.stringify(payload),
      });
      if (
        !isRecord(body) ||
        typeof body.invocation_id !== 'string' ||
        typeof body.status !== 'string'
      ) {
        throw new Error('WebMCP invocation response is invalid');
      }
      return body as WebMCPInvocationResult;
    },
  };

  return Object.freeze(client);
}
