import assert from 'node:assert/strict';
import test from 'node:test';

import {createWebMCPClient, WebMCPRequestError} from './webmcp.ts';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {'content-type': 'application/json'},
  });
}

test('lists browser-wide tools through the image API', async () => {
  const controller = new AbortController();
  const requests: Array<{url: string; init?: RequestInit}> = [];
  const tool = {
    tool_ref: 'wmcp_test',
    name: 'search',
    description: 'Search',
    input_schema: {type: 'object'},
    source: {
      window_id: 1,
      tab_id: 2,
      page_title: 'Travel',
      page_url: 'https://travel.example/',
      frame: null,
    },
  };
  const client = createWebMCPClient({
    apiBaseUrl: 'http://127.0.0.1:10001/',
    signal: controller.signal,
    fetchImpl: async (url, init) => {
      requests.push({url: String(url), init});
      return jsonResponse({tools: [tool]});
    },
  });

  assert.equal(Object.isFrozen(client), true);
  assert.deepEqual(await client.listTools(), [tool]);
  assert.equal(requests[0].url, 'http://127.0.0.1:10001/webmcp/tools');
  assert.equal(requests[0].init?.signal, controller.signal);
});

test('invokes an exact tool reference with input and timeout', async () => {
  let request: {url: string; init?: RequestInit} | undefined;
  const client = createWebMCPClient({
    apiBaseUrl: 'http://127.0.0.1:10001',
    fetchImpl: async (url, init) => {
      request = {url: String(url), init};
      return jsonResponse({
        invocation_id: 'invocation-1',
        status: 'completed',
        output: {ok: true},
      });
    },
  });

  const result = await client.invokeTool('wmcp_test', {query: 'SFO'}, {timeoutSec: 30});
  assert.deepEqual(result.output, {ok: true});
  assert.equal(request?.url, 'http://127.0.0.1:10001/webmcp/invoke');
  assert.equal(request?.init?.method, 'POST');
  assert.deepEqual(JSON.parse(String(request?.init?.body)), {
    tool_ref: 'wmcp_test',
    input: {query: 'SFO'},
    timeout_sec: 30,
  });
});

test('returns non-autosubmit form activation without waiting for submission', async () => {
  const client = createWebMCPClient({
    apiBaseUrl: 'http://127.0.0.1:10001',
    fetchImpl: async () =>
      jsonResponse({
        invocation_id: 'invocation-1',
        status: 'awaiting_submission',
        output: {form_populated: true, submitted: false},
      }),
  });

  const result = await client.invokeTool('wmcp_fill', {email: 'buyer@example.com'});
  assert.equal(result.status, 'awaiting_submission');
  assert.deepEqual(result.output, {
    form_populated: true,
    submitted: false,
  });
});

test('preserves structured WebMCP failures', async () => {
  const client = createWebMCPClient({
    apiBaseUrl: 'http://127.0.0.1:10001',
    fetchImpl: async () =>
      jsonResponse(
        {
          code: 'outcome_unknown',
          message: 'do not retry automatically',
          invocation_id: 'invocation-1',
        },
        504,
      ),
  });

  await assert.rejects(client.invokeTool('wmcp_test'), error => {
    assert.ok(error instanceof WebMCPRequestError);
    assert.equal(error.statusCode, 504);
    assert.equal(error.code, 'outcome_unknown');
    assert.equal(error.invocationId, 'invocation-1');
    assert.equal(
      error.message,
      'WebMCP outcome_unknown, invocation invocation-1: do not retry automatically',
    );
    assert.deepEqual(error.body, {
      code: 'outcome_unknown',
      message: 'do not retry automatically',
      invocation_id: 'invocation-1',
    });
    return true;
  });
});

test('names WebMCP failures without a structured body', async () => {
  const client = createWebMCPClient({
    apiBaseUrl: 'http://127.0.0.1:10001',
    fetchImpl: async () => new Response('gateway timeout', {status: 504}),
  });

  await assert.rejects(client.listTools(), error => {
    assert.ok(error instanceof WebMCPRequestError);
    assert.equal(error.message, 'WebMCP error: request failed with status 504');
    assert.equal(error.body, 'gateway timeout');
    return true;
  });
});
