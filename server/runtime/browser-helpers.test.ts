import assert from 'node:assert/strict';
import test from 'node:test';
import vm from 'node:vm';

import type { BrowserReplCdpClient } from './browser-cdp-client.ts';
import { BrowserHelpers } from './browser-helpers.ts';

function fakeElement({ visible }: { visible: boolean }) {
  return {
    isConnected: true,
    tagName: 'INPUT',
    getClientRects: () => (visible ? [{ width: 10, height: 10 }] : []),
    checkVisibility: () => visible,
  };
}

// Evaluates page-side helper expressions against a document whose
// querySelectorAll returns the supplied elements.
function helpersWithMatches(matches: unknown[]) {
  const page = vm.createContext({
    document: { querySelectorAll: () => matches },
    getComputedStyle: () => ({ display: 'block', visibility: 'visible', opacity: '1' }),
  });
  // The selector resolvers use only these client members.
  const client: Pick<BrowserReplCdpClient, 'sessionId' | 'ensureAttached' | 'evaluate'> = {
    sessionId: 'session',
    async ensureAttached() {},
    async evaluate<T>(_sessionId: string, expression: string): Promise<T> {
      return vm.runInContext(expression, page) as T;
    },
  };
  return new BrowserHelpers(client as BrowserReplCdpClient);
}

test('selector click reports not found when nothing matches the selector', async () => {
  const helpers = helpersWithMatches([]);
  await assert.rejects(
    helpers.click('a[href="https://example.com/x"]', { timeoutSec: 0 }),
    /^Error: click: selector "a\[href=\\"https:\/\/example\.com\/x\\"\]" was not actionable within 0s \(not found\)$/,
  );
});

test('selector click reports not visible when every match is hidden', async () => {
  const helpers = helpersWithMatches([fakeElement({ visible: false }), fakeElement({ visible: false })]);
  await assert.rejects(helpers.click('a', { timeoutSec: 0 }), /was not actionable within 0s \(not visible\)$/);
});

test('selector fillInput reports not found when nothing matches the selector', async () => {
  const helpers = helpersWithMatches([]);
  await assert.rejects(
    helpers.fillInput('input[name=missing]', 'text', { timeoutSec: 0 }),
    /^Error: fillInput: selector "input\[name=missing\]" was not editable within 0s \(not found\)$/,
  );
});

test('selector fillInput reports not visible when every match is hidden', async () => {
  const helpers = helpersWithMatches([fakeElement({ visible: false })]);
  await assert.rejects(
    helpers.fillInput('input', 'text', { timeoutSec: 0 }),
    /was not editable within 0s \(not visible\)$/,
  );
});
