import assert from 'node:assert/strict';
import test from 'node:test';
import vm from 'node:vm';

import { CellRuntime } from './cell-runtime.ts';

function fixture() {
  const helpers: Record<string, unknown> = {
    repl: Object.freeze({}),
    webmcp: Object.freeze({}),
    pageInfo: async () => ({ url: 'https://example.com/' }),
    iframeTarget: async (urlSubstring: string) => ({ targetId: `frame-${urlSubstring}` }),
    js: async (expression: string) => `evaluated ${expression}`,
  };
  const context = vm.createContext({ ...helpers, crypto, fetch, console });
  const contextGlobal = vm.runInContext('globalThis', context) as Record<PropertyKey, unknown>;
  const runtime = new CellRuntime(context, contextGlobal, Object.keys(helpers));
  return { runtime, contextGlobal };
}

const builtinError = (name: string) =>
  new RegExp(`^SyntaxError: \`${name}\` is a built-in Browser REPL helper; use a different variable name$`);

test('rejects declaring a built-in helper name and keeps the helper usable', async () => {
  const { runtime, contextGlobal } = fixture();

  await assert.rejects(runtime.evaluate('var pageInfo = await pageInfo();'), builtinError('pageInfo'));
  await assert.rejects(runtime.evaluate('var iframeTarget = await iframeTarget("x");'), builtinError('iframeTarget'));
  await assert.rejects(runtime.evaluate('var js = `document.title`;'), builtinError('js'));

  await runtime.evaluate(
    'globalThis.info = await pageInfo(); globalThis.frame = await iframeTarget("x"); globalThis.title = await js("document.title");',
  );
  assert.deepEqual(contextGlobal.info, { url: 'https://example.com/' });
  assert.deepEqual(contextGlobal.frame, { targetId: 'frame-x' });
  assert.equal(contextGlobal.title, 'evaluated document.title');
});

test('rejects every persistent declaration form that names a built-in', async () => {
  const { runtime } = fixture();
  const cells: Array<[string, string]> = [
    ['let pageInfo = 1;', 'pageInfo'],
    ['const repl = {};', 'repl'],
    ['let webmcp = null;', 'webmcp'],
    ['function js() {}', 'js'],
    ['class iframeTarget {}', 'iframeTarget'],
    ['const { url: pageInfo } = { url: "x" };', 'pageInfo'],
    ['var [first, js] = [1, 2];', 'js'],
    ['if (true) { var pageInfo = 1; }', 'pageInfo'],
    ['for (var js of [1]) {}', 'js'],
  ];
  for (const [cell, name] of cells) {
    await assert.rejects(runtime.evaluate(cell), builtinError(name), cell);
  }
});

test('a rejected cell does not run or register any of its bindings', async () => {
  const { runtime, contextGlobal } = fixture();

  await assert.rejects(
    runtime.evaluate('globalThis.ran = true; const info = 1; var js = 2;'),
    builtinError('js'),
  );
  assert.equal(contextGlobal.ran, undefined);
  assert.equal(Object.getOwnPropertyDescriptor(contextGlobal, 'info'), undefined);

  // `info` was never registered, so it can still be declared lexically.
  await runtime.evaluate('let info = await pageInfo();');
  await runtime.evaluate('globalThis.url = info.url;');
  assert.equal(contextGlobal.url, 'https://example.com/');
});

test('ordinary names, local scopes, and redeclaration rules are unchanged', async () => {
  const { runtime, contextGlobal } = fixture();

  await runtime.evaluate('var info = await pageInfo(); var info = await pageInfo();');
  await runtime.evaluate('function helper(js) { let pageInfo = js; return pageInfo; }');
  await runtime.evaluate('{ let js = 1; const repl = 2; } globalThis.local = helper("arg");');
  assert.equal(contextGlobal.local, 'arg');

  await runtime.evaluate('let count = 1;');
  await assert.rejects(runtime.evaluate('let count = 2;'), /Identifier 'count' has already been declared/);
  await assert.rejects(runtime.evaluate('var count = 2;'), /Identifier 'count' has already been declared/);
  await runtime.evaluate('count = 3; globalThis.count_ = count;');
  assert.equal(contextGlobal.count_, 3);
});

test('Node globals that are not REPL helpers may still be declared', async () => {
  const { runtime, contextGlobal } = fixture();

  await runtime.evaluate('const crypto = 1; var fetch = "custom"; let console = { log() {} };');
  await runtime.evaluate('globalThis.cryptoValue = crypto; globalThis.fetchValue = fetch; globalThis.logType = typeof console.log;');
  assert.equal(contextGlobal.cryptoValue, 1);
  assert.equal(contextGlobal.fetchValue, 'custom');
  assert.equal(contextGlobal.logType, 'function');
});
