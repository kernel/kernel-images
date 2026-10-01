import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium } from 'playwright-core';

const mode = process.env.MODE || 'direct';
const base = 'http://127.0.0.1:9226';
const idle = Number(process.env.IDLE_MS || 0);
const durations = (process.env.SLEEPS_MS || '1000,10000,60000').split(',').map(Number);
for (const name of ['BINARY', 'INSTANCE', 'UPSTREAM', 'API_TOKEN_FILE']) {
  assert(process.env[name], `${name} is required`);
}
const args = ['-mode', mode, '-upstream', process.env.UPSTREAM,
  '-instance', process.env.INSTANCE, '-api', process.env.API || 'http://127.0.0.1:4973',
  '-token-file', process.env.API_TOKEN_FILE, '-idle', `${idle}ms`];
if (process.env.RELAY_TOKEN_FILE) args.push('-relay-token-file', process.env.RELAY_TOKEN_FILE);
if (process.env.NO_TCP_KEEPALIVE) args.push('-no-upstream-keepalive');
const gateway = spawn(process.env.BINARY, args, { stdio: ['ignore', 'inherit', 'inherit'] });
let browser;
const rows = [];
const status = async () => (await fetch(`${base}/status`, { signal: AbortSignal.timeout(125000) })).json();
async function until(fn, timeout = 30000) {
  const start = Date.now();
  while (!await fn()) {
    if (Date.now() - start > timeout) throw new Error('condition timed out');
    await sleep(50);
  }
}
async function within(promise, label) {
  let timer;
  try {
    return await Promise.race([promise, new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${label} timed out after 30s`)), 30000);
    })]);
  } finally { clearTimeout(timer); }
}
function event(emitter, name, matches = () => true) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { emitter.off(name, listener); reject(new Error(`missing ${name}`)); }, 15000);
    const listener = value => {
      if (!matches(value)) return;
      emitter.off(name, listener);
      clearTimeout(timer);
      resolve(value);
    };
    emitter.on(name, listener);
  });
}
try {
  await until(async () => {
    if (gateway.exitCode !== null) throw new Error(`gateway exited: ${gateway.exitCode}`);
    try {
      const current = await status();
      return current.state === 'running' && current.instance === process.env.INSTANCE && current.mode === mode;
    } catch { return false; }
  });
  browser = await chromium.connectOverCDP('ws://127.0.0.1:9226/cdp', { timeout: 30000 });
  let disconnected = false;
  browser.on('disconnected', () => { disconnected = true; });
  const page = await browser.contexts()[0].newPage();
  page.setDefaultTimeout(15000);
  await page.setContent('<title>standby fixture</title><button>unchanged page</button>');
  await page.addScriptTag({ content: 'function checkpoint() { return 42; }\n//# sourceURL=cdp-standby-fixture.js' });
  const handle = await page.evaluateHandle(() => ({ value: 17 }));
  const cdp = await page.context().newCDPSession(page);
  let contextId;
  cdp.on('Runtime.executionContextCreated', ({ context }) => {
    if (context.auxData?.isDefault) contextId = context.id;
  });
  await cdp.send('Runtime.enable');
  await cdp.send('Runtime.addBinding', { name: 'standbyProbe' });
  await cdp.send('Debugger.enable');
  const { breakpointId } = await cdp.send('Debugger.setBreakpointByUrl', { url: 'cdp-standby-fixture.js', lineNumber: 0 });
  const { result: { objectId } } = await cdp.send('Runtime.evaluate', { expression: 'globalThis.persisted = {value: 41}' });
  assert(objectId); assert(contextId);
  const originalContextId = contextId;

  // An unanswered command must prevent both explicit and automatic standby.
  const pending = cdp.send('Runtime.evaluate', { expression: 'new Promise(r => setTimeout(() => r(7), 1500))', awaitPromise: true });
  await until(async () => (await status()).pending > 0);
  const denied = await fetch(`${base}/standby`, { method: 'POST' });
  assert.equal(denied.status, 409);
  await sleep(Math.min(idle + 100, 700));
  assert.equal((await status()).state, 'running');
  assert.equal((await pending).result.value, 7);

  for (const [i, duration] of durations.entries()) {
    await until(async () => (await status()).pending === 0);
    if (idle) {
      await until(async () => (await status()).state === 'standby');
    } else {
      const response = await fetch(`${base}/standby`, { method: 'POST', signal: AbortSignal.timeout(125000) });
      assert.equal(response.status, 200, await response.text());
    }
    await sleep(duration);
    assert.equal((await status()).state, 'standby', 'transport failed while asleep');
    const start = Date.now();
    // The same raw CDP session and object ID must work, not a newly attached session.
    const value = await within(cdp.send('Runtime.callFunctionOn', { objectId, functionDeclaration: 'function(){return ++this.value}', returnByValue: true }), 'pre-sleep CDP session');
    const commandMS = Date.now() - start;
    assert.equal(value.result?.value, 42 + i, JSON.stringify(value));
    assert.equal(await handle.evaluate(x => ++x.value), 18 + i);
    assert.equal((await cdp.send('Runtime.evaluate', { contextId: originalContextId, expression: 'persisted.value', returnByValue: true })).result.value, 42 + i);
    const bindingEvent = event(cdp, 'Runtime.bindingCalled', e => e.name === 'standbyProbe');
    await cdp.send('Runtime.evaluate', { expression: `standbyProbe('${i}')` });
    assert.equal((await bindingEvent).payload, String(i));
    const paused = event(cdp, 'Debugger.paused');
    const evaluating = cdp.send('Runtime.evaluate', { expression: 'checkpoint()' });
    assert((await paused).hitBreakpoints.includes(breakpointId));
    await cdp.send('Debugger.resume');
    assert.equal((await evaluating).result.value, 42);
    assert.equal(await page.title(), 'standby fixture');
    assert.equal(disconnected, false);
    assert.equal(contextId, originalContextId);
    const { instance, mode: gatewayMode, ...metrics } = await status();
    rows.push({ sleep_ms: duration, command_ms: commandMS, ...metrics });
    console.log(JSON.stringify({ cycle: i + 1, ...rows.at(-1) }));
  }
  await cdp.send('Debugger.disable');
  await page.close();
  console.log(JSON.stringify({ mode, idle_ms: idle, result: 'PASS', rows }));
} catch (error) {
  console.log(JSON.stringify({ mode, idle_ms: idle, result: 'FAIL', error: String(error), stack: error.stack, rows }));
  process.exitCode = 1;
} finally {
  gateway.kill();
  if (browser) await browser.close().catch(() => {});
}
