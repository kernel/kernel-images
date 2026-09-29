import assert from 'node:assert/strict';
import test from 'node:test';
import { BINDING_NAME, CustomWebMCPPageRuntime } from './custom-webmcp-page-runtime.ts';
import type { BrowserReplCdpClient, CdpEvent } from './browser-cdp-client.ts';
import type { CustomToolDefinition } from './custom-webmcp-definitions.ts';

function fixture({closedPrevious = false} = {}) {
  const attached: string[] = [];
  const errors: string[] = [];
  const activated: boolean[] = [];
  const responses: string[] = [];
  const client = {
    targetId: 'unrelated-tab',
    async attach(targetId: string, options?: {activate?: boolean}) {
      if (closedPrevious && targetId === 'unrelated-tab') throw new Error('previous tab closed');
      attached.push(targetId);
      activated.push(options?.activate ?? true);
      this.targetId = targetId;
    },
    async detach() { this.targetId = null; },
    async send(_method: string, params: {expression: string}) {
      responses.push(params.expression);
      return {};
    },
    close() {},
  } as unknown as BrowserReplCdpClient;
  const definition = {
    inputValidator: () => ({valid: true, data: {}}),
    execute: () => ({targetId: client.targetId}),
  } as unknown as CustomToolDefinition;
  const runtime = new CustomWebMCPPageRuntime(
    client,
    async (_signal, callback) => callback(),
    (sessionId) => sessionId === 'registered-session'
      ? {definition, matches: [], targetId: 'registered-tab'}
      : undefined,
    (error) => errors.push(error instanceof Error ? error.message : String(error)),
  );
  return {attached, activated, errors, responses, client, definition, runtime};
}

test('API invocation attaches the registering tab before executing the body', async () => {
  const {attached, activated, client, definition, runtime} = fixture();
  assert.deepEqual(await runtime.invokeCDP(definition, [], 'registered-tab', {}), {targetId: 'registered-tab'});
  assert.deepEqual(attached, ['registered-tab', 'unrelated-tab']);
  assert.deepEqual(activated, [false, false]);
  assert.equal(client.targetId, 'unrelated-tab');
});

test('a closed previous tab does not hide a successful tool result', async () => {
  const {client, definition, errors, runtime} = fixture({closedPrevious: true});
  assert.deepEqual(await runtime.invokeCDP(definition, [], 'registered-tab', {}), {targetId: 'registered-tab'});
  assert.equal(client.targetId, null);
  assert.deepEqual(errors, ['previous tab closed']);
});

test('invocation without a previous attachment leaves the client detached', async () => {
  const {attached, client, definition, runtime} = fixture();
  client.targetId = null;
  assert.deepEqual(await runtime.invokeCDP(definition, [], 'registered-tab', {}), {targetId: 'registered-tab'});
  assert.deepEqual(attached, ['registered-tab']);
  assert.equal(client.targetId, null);
});

test('concurrent calls wait but nested calls fail without switching the active tab', async () => {
  const {attached, client, definition, runtime} = fixture();
  let release!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  definition.execute = async () => {
    await assert.rejects(runtime.invokeCDP(definition, [], 'nested-tab', {}), /nested page or custom CDP invocation is not supported/);
    await held;
    return {targetId: client.targetId};
  };
  const first = runtime.invokeCDP(definition, [], 'first-tab', {});
  await new Promise((resolve) => setTimeout(resolve, 0));
  const second = runtime.invokeCDP({...definition, execute: () => ({targetId: client.targetId})}, [], 'second-tab', {});
  assert.deepEqual(attached, ['first-tab']);
  release();
  assert.deepEqual(await first, {targetId: 'first-tab'});
  assert.deepEqual(await second, {targetId: 'second-tab'});
  assert.deepEqual(attached, ['first-tab', 'unrelated-tab', 'second-tab', 'unrelated-tab']);
});

test('concurrent page invocations fail before switching the active tab', async () => {
  const {attached, definition, runtime} = fixture();
  let release!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  definition.execute = async () => { await held; return {}; };
  const first = runtime.invokeCDP(definition, [], 'registered-tab', {}, undefined, true);
  await new Promise((resolve) => setTimeout(resolve, 0));
  await assert.rejects(runtime.invokeCDP(definition, [], 'second-tab', {}, undefined, true), /busy; retry/);
  assert.deepEqual(attached, ['registered-tab']);
  release();
  await first;
});

test('page invocation attaches its own tab and returns the result to its registering document', async () => {
  const {attached, responses, runtime} = fixture();
  const event = {
    method: 'Runtime.bindingCalled',
    sessionId: 'registered-session',
    params: {
      name: BINDING_NAME,
      payload: JSON.stringify({type: 'invoke', invocation_id: 'invocation-1', definition_id: 'tool-1', revision: 1, input: {}}),
      executionContextId: 42,
    },
  } as CdpEvent;
  await runtime.handleBinding(event);
  assert.deepEqual(attached, ['registered-tab', 'unrelated-tab']);
  assert.match(responses[0], /"targetId":"registered-tab"/);
});
