#!/usr/bin/env bun

/*
Benchmark the readiness contract of POST /configure on the kernel-images
container by:
  1. Starting one of the kernel-images-headless docker images.
  2. Waiting for the initial chromium to be fully CDP-responsive.
  3. Issuing a single POST /configure with both viewport (1280x800@60) and
     --kiosk chromium_flags so the VM has to stop chromium and start it
     back up (the "stop/start path" inside ChromiumConfigure).
  4. Concurrently probing:
       (a) when /json/version starts reporting a NEW browser UUID
           (i.e. the new chromium has emitted "DevTools listening on ...")
       (b) when a real Browser.getVersion CDP round-trip succeeds against
           that new UUID (i.e. chromium is actually serving CDP).
     and timing the gap between the api returning 200 and (a)/(b) being
     true.

This isolates the bug fixed in fix/chromium-readiness-false-positive:
the BEFORE image returns 200 several seconds before chromium can actually
serve requests; the AFTER image holds 200 until chromium is real.

Usage:
  bun bench/bench-configure-readiness.ts \
    --image kernel-images-headless:before --label before
  bun bench/bench-configure-readiness.ts \
    --image kernel-images-headless:after  --label after
*/

interface Args {
  image: string;
  label: string;
  apiPort: number;
  devToolsPort: number;
  containerName: string;
  iterations: number;
}

function parseArgs(): Args {
  const argv = process.argv.slice(2);
  const get = (flag: string, def?: string) => {
    const i = argv.indexOf(flag);
    if (i === -1) return def;
    return argv[i + 1];
  };
  const image = get('--image');
  if (!image) {
    console.error('--image is required');
    process.exit(1);
  }
  return {
    image,
    label: get('--label', image) ?? image,
    apiPort: Number(get('--api-port', '19001')),
    devToolsPort: Number(get('--devtools-port', '19222')),
    containerName: get('--name', `kibench-${Date.now()}`)!,
    iterations: Number(get('--iterations', '3')),
  };
}

const args = parseArgs();

async function sh(cmd: string[], opts?: { quiet?: boolean }): Promise<string> {
  const proc = Bun.spawn(cmd, {
    stdout: 'pipe',
    stderr: opts?.quiet ? 'ignore' : 'pipe',
  });
  const out = await new Response(proc.stdout).text();
  await proc.exited;
  if (proc.exitCode !== 0) {
    throw new Error(`${cmd.join(' ')} exited ${proc.exitCode}: ${out}`);
  }
  return out;
}

async function dockerRm(name: string): Promise<void> {
  try {
    await sh(['docker', 'rm', '-f', name], { quiet: true });
  } catch {
    // ignore
  }
}

async function startContainer(): Promise<void> {
  await dockerRm(args.containerName);
  console.log(`[start] docker run ${args.image}`);
  // Detached run; we'll exec on it later if we need anything from inside.
  await sh([
    'docker',
    'run',
    '-d',
    '--rm',
    '--name',
    args.containerName,
    '--platform',
    'linux/amd64',
    '--privileged',
    '--tmpfs',
    '/dev/shm:size=2g',
    '-p',
    `${args.devToolsPort}:9222`,
    '-p',
    `${args.apiPort}:10001`,
    args.image,
  ]);
}

async function getBrowserUUID(): Promise<string | null> {
  try {
    const res = await fetch(`http://localhost:${args.devToolsPort}/json/version`, {
      signal: AbortSignal.timeout(1500),
    });
    if (!res.ok) return null;
    const data = (await res.json()) as { webSocketDebuggerUrl?: string };
    const m = data.webSocketDebuggerUrl?.match(/\/devtools\/browser\/([^/?]+)/);
    return m?.[1] ?? null;
  } catch {
    return null;
  }
}

async function browserGetVersionCDP(uuid: string, timeoutMs = 2000): Promise<boolean> {
  // Send a single Browser.getVersion message on a fresh WS.
  return new Promise<boolean>((resolve) => {
    let settled = false;
    const settle = (ok: boolean) => {
      if (!settled) {
        settled = true;
        try {
          ws.close();
        } catch {
          // ignore
        }
        resolve(ok);
      }
    };
    let ws: WebSocket;
    try {
      ws = new WebSocket(`ws://localhost:${args.devToolsPort}/devtools/browser/${uuid}`);
    } catch {
      resolve(false);
      return;
    }
    const t = setTimeout(() => settle(false), timeoutMs);
    ws.addEventListener('open', () => {
      try {
        ws.send(JSON.stringify({ id: 1, method: 'Browser.getVersion' }));
      } catch {
        clearTimeout(t);
        settle(false);
      }
    });
    ws.addEventListener('message', (ev) => {
      try {
        const msg = JSON.parse(String(ev.data));
        if (msg.id === 1 && msg.result?.product) {
          clearTimeout(t);
          settle(true);
        }
      } catch {
        // ignore
      }
    });
    ws.addEventListener('error', () => {
      clearTimeout(t);
      settle(false);
    });
    ws.addEventListener('close', () => {
      clearTimeout(t);
      settle(false);
    });
  });
}

async function waitUntil(
  fn: () => Promise<boolean | string | null>,
  timeoutMs: number,
  intervalMs = 100,
): Promise<{ ok: boolean; ms: number; value: boolean | string | null }> {
  const t0 = Date.now();
  while (Date.now() - t0 < timeoutMs) {
    const v = await fn();
    if (v) {
      return { ok: true, ms: Date.now() - t0, value: v };
    }
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  return { ok: false, ms: Date.now() - t0, value: null };
}

function makeConfigureBody(width: number, height: number, refresh: number): { body: Blob; contentType: string } {
  const fd = new FormData();
  fd.append('display', JSON.stringify({ width, height, refresh_rate: refresh, restart_chromium: true, require_idle: false }));
  fd.append('chromium_flags', JSON.stringify({ flags: ['--kiosk'] }));
  // Bun's FormData via Request handles boundary automatically; we capture both
  // body and content-type via a dummy Request.
  const req = new Request('http://x/', { method: 'POST', body: fd });
  return { body: req.body as unknown as Blob, contentType: req.headers.get('content-type')! };
}

async function postConfigure(): Promise<{ status: number; ms: number; body: string }> {
  const { body, contentType } = makeConfigureBody(1280, 800, 60);
  const t0 = Date.now();
  let res: Response;
  try {
    res = await fetch(`http://localhost:${args.apiPort}/configure`, {
      method: 'POST',
      headers: { 'content-type': contentType },
      body,
      signal: AbortSignal.timeout(60_000),
    });
  } catch (e) {
    return { status: -1, ms: Date.now() - t0, body: `fetch error: ${(e as Error).message}` };
  }
  const ms = Date.now() - t0;
  const txt = await res.text();
  return { status: res.status, ms, body: txt };
}

interface Sample {
  iter: number;
  initialUUID: string;
  configureMs: number;
  configureStatus: number;
  newUUIDMs: number | null;
  newUUID: string | null;
  cdpReadyMs: number | null;
  gapMs: number | null; // cdpReadyMs - configureMs
}

async function runOneIteration(iter: number): Promise<Sample> {
  // Wait for the initial chromium to be fully ready (UUID present + CDP responsive).
  console.log(`[iter ${iter}] waiting for initial chromium ready...`);
  const initialResult = await waitUntil(
    async () => {
      const u = await getBrowserUUID();
      if (!u) return null;
      const ok = await browserGetVersionCDP(u, 1500);
      return ok ? u : null;
    },
    90_000,
    250,
  );
  if (!initialResult.ok || typeof initialResult.value !== 'string') {
    throw new Error('initial chromium never became ready');
  }
  const initialUUID = initialResult.value;
  console.log(`[iter ${iter}] initial chromium ready in ${initialResult.ms}ms  uuid=${initialUUID.slice(0, 8)}…`);

  // Fire POST /configure and concurrently start probing for the new UUID.
  console.log(`[iter ${iter}] POST /configure (viewport 1280x800@60 + --kiosk)...`);
  const configureStart = Date.now();
  const configurePromise = postConfigure();

  // Poll fast for the new UUID to appear on /json/version.
  let newUUID: string | null = null;
  let newUUIDMs: number | null = null;
  const newUUIDProbe = (async () => {
    while (Date.now() - configureStart < 60_000) {
      const u = await getBrowserUUID();
      if (u && u !== initialUUID) {
        newUUID = u;
        newUUIDMs = Date.now() - configureStart;
        return;
      }
      await new Promise((r) => setTimeout(r, 25));
    }
  })();

  // Wait for /configure to return.
  const configResult = await configurePromise;
  const configureMs = configResult.ms;
  console.log(
    `[iter ${iter}] /configure returned ${configResult.status} in ${configureMs}ms${configResult.status !== 200 ? `  body=${configResult.body.slice(0, 200)}` : ''}`,
  );

  // Wait for the new UUID probe to land (or time out).
  await newUUIDProbe;
  if (newUUID === null) {
    return {
      iter,
      initialUUID,
      configureMs,
      configureStatus: configResult.status,
      newUUIDMs: null,
      newUUID: null,
      cdpReadyMs: null,
      gapMs: null,
    };
  }
  console.log(`[iter ${iter}] new UUID observed at +${newUUIDMs}ms  uuid=${newUUID!.slice(0, 8)}…`);

  // Now wait for a real CDP round-trip to succeed against the new UUID.
  let cdpReadyMs: number | null = null;
  const cdpDeadline = configureStart + 60_000;
  while (Date.now() < cdpDeadline) {
    const ok = await browserGetVersionCDP(newUUID!, 1500);
    if (ok) {
      cdpReadyMs = Date.now() - configureStart;
      break;
    }
    await new Promise((r) => setTimeout(r, 25));
  }
  console.log(`[iter ${iter}] Browser.getVersion succeeded at +${cdpReadyMs}ms`);

  const gapMs = cdpReadyMs !== null ? cdpReadyMs - configureMs : null;
  return {
    iter,
    initialUUID,
    configureMs,
    configureStatus: configResult.status,
    newUUIDMs,
    newUUID,
    cdpReadyMs,
    gapMs,
  };
}

async function main(): Promise<void> {
  console.log(`=== benchmark ${args.label} (image=${args.image}) ===`);
  console.log(`apiPort=${args.apiPort}  devToolsPort=${args.devToolsPort}  iters=${args.iterations}`);

  await startContainer();

  const samples: Sample[] = [];
  try {
    for (let i = 1; i <= args.iterations; i++) {
      const s = await runOneIteration(i);
      samples.push(s);
    }
  } finally {
    console.log('\n[cleanup] tearing down container');
    await dockerRm(args.containerName);
  }

  console.log(`\n=== ${args.label} summary ===`);
  console.log(
    `${'iter'.padEnd(5)}${'configMs'.padEnd(11)}${'newUUID@'.padEnd(11)}${'cdpReady@'.padEnd(11)}${'GAP'.padEnd(8)}`,
  );
  console.log('-'.repeat(46));
  for (const s of samples) {
    console.log(
      String(s.iter).padEnd(5) +
        String(s.configureMs).padEnd(11) +
        String(s.newUUIDMs ?? '?').padEnd(11) +
        String(s.cdpReadyMs ?? '?').padEnd(11) +
        String(s.gapMs ?? '?').padEnd(8),
    );
  }
  console.log();
  const gaps = samples.map((s) => s.gapMs).filter((v): v is number => v !== null);
  if (gaps.length) {
    const sum = gaps.reduce((a, b) => a + b, 0);
    const min = Math.min(...gaps);
    const max = Math.max(...gaps);
    const sorted = [...gaps].sort((a, b) => a - b);
    const median = sorted[Math.floor(sorted.length / 2)]!;
    console.log(`GAP stats (ms):  n=${gaps.length}  min=${min}  median=${median}  mean=${Math.round(sum / gaps.length)}  max=${max}`);
    console.log('(GAP = cdpReadyMs − configureMs.  Positive = api returned 200 before chromium was really ready.)');
  } else {
    console.log('no gap measurements collected');
  }
}

void main();
