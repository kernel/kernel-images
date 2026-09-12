import assert from 'node:assert/strict';
import test from 'node:test';

import {
  browserReplBrowserMethodNames,
  formatBrowserReplHelp,
  listBrowserReplHelpEntries,
} from './browser-repl-help.ts';

test('lists every registered method', () => {
  const output = formatBrowserReplHelp();
  for (const entry of listBrowserReplHelpEntries()) {
    assert.match(output, new RegExp(`\\b${entry.qualifiedName.replace('.', '\\.')}\\b`));
  }
});

test('resolves qualified and unqualified browser method names', () => {
  const direct = formatBrowserReplHelp('click');
  assert.equal(direct, formatBrowserReplHelp('browser.click'));
  assert.match(direct, /^click\(target, options\?\)/);
  assert.match(direct, /Available as click\(\.\.\.\) and browser\.click\(\.\.\.\)\./);
});

test('rejects invalid and unknown method names', () => {
  assert.throws(() => formatBrowserReplHelp(''), /non-empty string/);
  assert.throws(() => formatBrowserReplHelp(42), /non-empty string/);
  assert.throws(() => formatBrowserReplHelp('noSuchMethod'), /call repl\.help\(\)/);
});

test('browser helper names are unique and have detailed help', () => {
  assert.equal(new Set(browserReplBrowserMethodNames).size, browserReplBrowserMethodNames.length);
  for (const name of browserReplBrowserMethodNames) {
    const output = formatBrowserReplHelp(name);
    assert.match(output, new RegExp(`^${name}\\(`));
    assert.ok(output.length > name.length + 40, `${name} help is unexpectedly sparse`);
  }
});
