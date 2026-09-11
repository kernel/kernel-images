import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, readFile, rm, symlink } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { createConnection } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { setTimeout } from 'node:timers/promises';
import test from 'node:test';

test('daemon vault protocol never returns or logs secret-bearing errors, even with DEBUG enabled', {
  skip: !process.env.VAULT_FILL_BROWSER_TESTS, timeout: 60000,
}, async t => {
  const { chromium } = await import('playwright-core');
  const { build } = await import('esbuild');
  const dir = await mkdtemp(join(tmpdir(), 'vault-daemon-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const context = await chromium.launchPersistentContext(join(dir, 'profile'), {
    executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true,
    args: ['--no-sandbox', '--remote-debugging-port=0'],
  });
  t.after(() => context.close());
  const [port, path] = (await readFile(join(dir, 'profile', 'DevToolsActivePort'), 'utf8')).trim().split('\n');
  const script = join(dir, 'daemon.cjs');
  await symlink(dirname(dirname(createRequire(import.meta.url).resolve('playwright-core/package.json'))), join(dir, 'node_modules'), 'dir');
  await build({ entryPoints: [new URL('./playwright-daemon.ts', import.meta.url).pathname], outfile: script, bundle: true, platform: 'node', format: 'cjs', packages: 'external' });
  const page = context.pages()[0];
  const secret = 'unique-vault-🔐-value-do-not-log';
  const seed = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ';
  const socketPath = join(dir, 'daemon.sock');
  const child = spawn(process.execPath, [script], {
    env: { ...process.env,
      PLAYWRIGHT_DAEMON_SOCKET: socketPath, CDP_ENDPOINT: `ws://127.0.0.1:${port}${path}`,
      PLAYWRIGHT_ENGINE: process.env.VAULT_FILL_ENGINE || 'playwright-core', DEBUG: 'pw:*', PWDEBUG: '1' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let logs = '';
  child.stdout.on('data', chunk => { logs += chunk; });
  child.stderr.on('data', chunk => { logs += chunk; });
  const exited = once(child, 'exit');
  t.after(async () => { child.kill('SIGKILL'); await exited; });
  const deadline = performance.now() + 10000;
  while (!logs.includes('CDP connection established') && performance.now() < deadline && child.exitCode === null) await setTimeout(20);
  assert.match(logs, /CDP connection established/);

  const call = async (payload: object, fragment = false) => {
    const socket = createConnection(socketPath);
    try {
      await once(socket, 'connect');
      const bytes = Buffer.from(JSON.stringify({ id: 'test', ...payload }) + '\n');
      if (fragment) {
        const split = bytes.indexOf(Buffer.from('🔐')) + 1;
        socket.write(bytes.subarray(0, split));
        await setTimeout(20);
        socket.write(bytes.subarray(split));
      } else socket.write(bytes);
      let buffer = '';
      for await (const chunk of socket) {
        buffer += chunk;
        if (buffer.includes('\n')) return JSON.parse(buffer.split('\n')[0]);
      }
      throw new Error('daemon_closed');
    } finally { socket.destroy(); }
  };
  assert.deepEqual(await call({ method: 'vault_fill_capabilities' }), { id: 'test', success: true, result: { version: 1 } });
  await page.setContent('<input id="a"><input id="otp">');
  const success = await call({ method: 'vault_fill', request: { bindings: [
    { selector: '#a', value: secret, type: 'password' }, { selector: '#otp', value: seed, type: 'totp' },
  ] } }, true);
  assert.equal(success.result.status, 'filled');
  assert.equal(await page.locator('#a').inputValue(), secret);
  assert.match(await page.locator('#otp').inputValue(), /^\d{6}$/);
  await page.setContent('<input id="a" type="number"><input id="b">');
  const failed = await call({ method: 'vault_fill', request: { bindings: [
    { selector: '#a', value: secret, type: 'text' }, { selector: '#b', value: seed, type: 'totp' },
  ] } });
  assert.deepEqual(failed, { id: 'test', success: true, result: { status: 'unknown', fields: [
    { index: 0, status: 'unknown' }, { index: 1, status: 'not_attempted' },
  ] } });

  await page.setContent(`<input id="a" oninput="document.querySelector('#b').style.display='none'"><input id="b"><input id="c">`);
  const timeout = await call({ method: 'vault_fill', request: { timeout_ms: 300, bindings: ['#a', '#b', '#c'].map(selector => ({ selector, value: secret, type: 'password' })) } });
  assert.equal(timeout.result.status, 'unknown');
  assert.deepEqual(timeout.result.fields.map((field: { status: string }) => field.status), ['filled', 'unknown', 'not_attempted']);
  await page.locator('#b').evaluate(element => element.style.display = 'block');
  await setTimeout(400);
  assert.equal(await page.locator('#b').inputValue(), '');
  assert.equal(await page.locator('#c').inputValue(), '');
  await page.setContent(`<input id="a" oninput="document.querySelector('#b').style.display='none';setTimeout(()=>location.hash='changed',100)"><input id="b">`);
  const navigation = await call({ method: 'vault_fill', request: { timeout_ms: 3000, bindings: ['#a', '#b'].map(selector => ({ selector, value: secret, type: 'password' })) } });
  assert.equal(navigation.result.status, 'unknown');
  await page.locator('#b').evaluate(element => element.style.display = 'block');
  await setTimeout(300);
  assert.equal(await page.locator('#b').inputValue(), '');
  assert.ok(!JSON.stringify([success, failed, timeout, navigation]).includes(secret));
  assert.ok(!JSON.stringify([success, failed, timeout]).includes(seed));
  assert.ok(!logs.includes(secret));
  assert.ok(!logs.includes(seed));
  assert.ok(!logs.includes('pw:api'));
  assert.ok(!logs.includes('pw:protocol'));
});
