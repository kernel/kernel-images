import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { existsSync } from 'node:fs';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createConnection } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';
import test from 'node:test';

test('the socket binds without attaching to the browser, which the first request does', { timeout: 10000 }, async t => {
  const dir = await mkdtemp(join(tmpdir(), 'playwright-startup-'));
  const socketPath = join(dir, 'daemon.sock');
  const started = join(dir, 'import-started');
  const release = join(dir, 'release-import');
  const loader = join(dir, 'loader.mjs');
  // Hold package evaluation until released, rather than depending on CPU load
  // or the speed of installed Playwright packages to reproduce cold startup.
  const engine = `
    import { existsSync, writeFileSync } from 'node:fs';
    import { setTimeout } from 'node:timers/promises';
    writeFileSync(${JSON.stringify(started)}, '');
    while (!existsSync(${JSON.stringify(release)})) await setTimeout(20);
    export const chromium = { connectOverCDP: async () => ({
      on() {}, isConnected() { return true; }, async close() {}
    }) };
    export const transform = async code => ({ code });
    export const Browser = null, CDPSession = null, Page = null;
  `;
  await writeFile(loader, `
    export async function resolve(specifier, context, nextResolve) {
      if (['playwright-core', 'patchright', 'esbuild'].includes(specifier)) {
        return { url: 'data:text/javascript,' + encodeURIComponent(${JSON.stringify(engine)}), shortCircuit: true };
      }
      if (['./page-target-id-cache', './webmcp'].includes(specifier)) specifier += '.ts';
      return nextResolve(specifier, context);
    }
  `);
  const child = spawn(process.execPath, [
    '--experimental-loader', pathToFileURL(loader).href,
    new URL('./playwright-daemon.ts', import.meta.url).pathname,
  ], { env: { ...process.env, PLAYWRIGHT_DAEMON_SOCKET: socketPath }, stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '';
  child.stderr.on('data', chunk => { stderr += chunk; });
  const exited = once(child, 'exit');
  t.after(async () => {
    child.kill('SIGKILL');
    await exited;
    await rm(dir, { recursive: true, force: true });
  });
  const deadline = performance.now() + 4000;
  while (!existsSync(socketPath) && performance.now() < deadline && child.exitCode === null) {
    await setTimeout(20);
  }
  assert.ok(existsSync(socketPath), `socket blocked by engine initialization: ${stderr}`);

  // The daemon is started at boot, so attaching here would attach to every
  // session. A Playwright page with no `dialog` listener dismisses JavaScript
  // dialogs, which is not a thing to do to a browser nobody has asked to
  // automate yet.
  await setTimeout(300);
  assert.ok(!existsSync(started), `engine was imported before any request: ${stderr}`);

  const socket = createConnection(socketPath);
  try {
    await once(socket, 'connect');
    socket.write(JSON.stringify({ id: 'req-1', code: 'return 1;', timeout_ms: 2000 }) + '\n');
    const importDeadline = performance.now() + 3000;
    while (!existsSync(started) && performance.now() < importDeadline) {
      await setTimeout(20);
    }
    assert.ok(existsSync(started), `a request did not trigger the engine import: ${stderr}`);
    await writeFile(release, '');
    const connectedDeadline = performance.now() + 3000;
    while (!stderr.includes('CDP connection established') && performance.now() < connectedDeadline) {
      await setTimeout(20);
    }
    assert.match(stderr, /CDP connection established/);
  } finally {
    socket.destroy();
  }
});
