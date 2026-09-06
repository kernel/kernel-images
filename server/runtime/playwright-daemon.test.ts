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

test('socket readiness does not wait for cold browser-engine imports', { timeout: 10000 }, async t => {
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
  while ((!existsSync(started) || !existsSync(socketPath)) && performance.now() < deadline && child.exitCode === null) {
    await setTimeout(20);
  }
  assert.ok(existsSync(started), `engine import was not exercised: ${stderr}`);
  assert.ok(existsSync(socketPath), `socket blocked by engine initialization: ${stderr}`);
  const socket = createConnection(socketPath);
  try {
    await once(socket, 'connect');
  } finally {
    socket.destroy();
  }
  await writeFile(release, '');
  const connectedDeadline = performance.now() + 2000;
  while (!stderr.includes('CDP connection established') && performance.now() < connectedDeadline) {
    await setTimeout(20);
  }
  assert.match(stderr, /CDP connection established/);
});
