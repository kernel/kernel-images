import assert from 'node:assert/strict';
import test from 'node:test';

import { HeapMonitor } from './heap-monitor.ts';

test('warns once above 90% and re-arms below 80%', () => {
  const limitBytes = 1000;
  let usedBytes = 0;
  const warnings: number[] = [];
  const monitor = new HeapMonitor(() => ({ usedBytes, limitBytes }), (sample) => warnings.push(sample.usedBytes));

  for (const used of [500, 899, 900, 950, 990, 850, 810]) {
    usedBytes = used;
    monitor.check();
  }
  assert.deepEqual(warnings, [900]);

  usedBytes = 799;
  monitor.check();
  usedBytes = 920;
  monitor.check();
  assert.deepEqual(warnings, [900, 920]);
});
