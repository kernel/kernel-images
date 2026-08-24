import assert from 'node:assert/strict';
import test from 'node:test';

import { PageTargetIdCache } from './page-target-id-cache.ts';

test('reuses a successfully discovered target ID', async () => {
  const page = {};
  let discoveries = 0;
  const cache = new PageTargetIdCache<object>(async () => {
    discoveries++;
    return 'target-1';
  });

  assert.equal(await cache.get(page), 'target-1');
  assert.equal(await cache.get(page), 'target-1');
  assert.equal(discoveries, 1);
});

test('refreshes a cached target ID', async () => {
  const page = {};
  let targetId = 'stale-target';
  let discoveries = 0;
  const cache = new PageTargetIdCache<object>(async () => {
    discoveries++;
    return targetId;
  });

  assert.equal(await cache.get(page), 'stale-target');
  targetId = 'fresh-target';
  assert.equal(await cache.get(page), 'stale-target');
  assert.equal(await cache.get(page, { refresh: true }), 'fresh-target');
  assert.equal(await cache.get(page), 'fresh-target');
  assert.equal(discoveries, 2);
});

test('does not cache failed discovery', async () => {
  const page = {};
  let discoveries = 0;
  const cache = new PageTargetIdCache<object>(async () => {
    discoveries++;
    if (discoveries === 1) throw new Error('page closed');
    return 'target-1';
  });

  await assert.rejects(cache.get(page), /page closed/);
  assert.equal(await cache.get(page), 'target-1');
  assert.equal(discoveries, 2);
});

test('shares an in-flight discovery between concurrent callers', async () => {
  const page = {};
  const pending = Promise.withResolvers<string>();
  let discoveries = 0;
  const cache = new PageTargetIdCache<object>(() => {
    discoveries++;
    return pending.promise;
  });

  const first = cache.get(page);
  const second = cache.get(page);
  assert.equal(discoveries, 1);

  pending.resolve('target-1');
  assert.deepEqual(await Promise.all([first, second]), ['target-1', 'target-1']);
  assert.equal(discoveries, 1);
});

test('failed refresh evicts the stale target ID', async () => {
  const page = {};
  const results = ['stale-target', new Error('target replaced'), 'fresh-target'];
  let discoveries = 0;
  const cache = new PageTargetIdCache<object>(async () => {
    const result = results[discoveries++];
    if (result instanceof Error) throw result;
    return result;
  });

  assert.equal(await cache.get(page), 'stale-target');
  await assert.rejects(cache.get(page, { refresh: true }), /target replaced/);
  assert.equal(await cache.get(page), 'fresh-target');
  assert.equal(discoveries, 3);
});

test('builds an index while skipping pages that cannot be inspected', async () => {
  const firstPage = {};
  const closedPage = {};
  const secondPage = {};
  const targetIds = new Map<object, string>([
    [firstPage, 'target-1'],
    [secondPage, 'target-2'],
  ]);
  const cache = new PageTargetIdCache<object>(async page => {
    const targetId = targetIds.get(page);
    if (targetId === undefined) throw new Error('page closed');
    return targetId;
  });

  const pageByTargetId = await cache.buildPageByTargetId([firstPage, closedPage, secondPage]);

  assert.deepEqual([...pageByTargetId.entries()], [
    ['target-1', firstPage],
    ['target-2', secondPage],
  ]);
});

test('refreshes cached IDs while rebuilding the index', async () => {
  const page = {};
  let targetId = 'stale-target';
  let discoveries = 0;
  const cache = new PageTargetIdCache<object>(async () => {
    discoveries++;
    return targetId;
  });

  assert.equal((await cache.buildPageByTargetId([page])).get('stale-target'), page);
  targetId = 'fresh-target';
  const refreshed = await cache.buildPageByTargetId([page], { refresh: true });

  assert.equal(refreshed.has('stale-target'), false);
  assert.equal(refreshed.get('fresh-target'), page);
  assert.equal(discoveries, 2);
});

test('reset discards every cached target ID', async () => {
  const firstPage = {};
  const secondPage = {};
  let discoveries = 0;
  const cache = new PageTargetIdCache<object>(async () => `target-${++discoveries}`);

  assert.equal(await cache.get(firstPage), 'target-1');
  assert.equal(await cache.get(secondPage), 'target-2');
  cache.reset();
  assert.equal(await cache.get(firstPage), 'target-3');
  assert.equal(await cache.get(secondPage), 'target-4');
});

test('reset prevents an in-flight discovery from repopulating the cache', async () => {
  const page = {};
  const pending = Promise.withResolvers<string>();
  let discoveries = 0;
  const cache = new PageTargetIdCache<object>(async () => {
    discoveries++;
    if (discoveries === 1) return pending.promise;
    return 'fresh-target';
  });

  const staleDiscovery = cache.get(page);
  cache.reset();
  const freshDiscovery = cache.get(page);
  assert.equal(discoveries, 2);

  pending.resolve('stale-target');
  assert.equal(await staleDiscovery, 'stale-target');
  assert.equal(await freshDiscovery, 'fresh-target');
  assert.equal(await cache.get(page), 'fresh-target');
  assert.equal(discoveries, 2);
});
