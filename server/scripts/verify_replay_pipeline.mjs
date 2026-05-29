#!/usr/bin/env node

import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const DEFAULT_GOLDEN = 'Kernel-Explainer/Kernel-Explainer-May-29-11-17-45.mp4';
const DEFAULT_APP_DIST = 'Kernel-Explainer/artifacts/kernel-pitch-video/dist/public';
const DEFAULT_OUT_DIR = 'artifacts/replay-pipeline-verifier';
const DEFAULT_IMAGE = 'kernel-headful-test:latest';

function usage() {
  console.log(`Usage: node server/scripts/verify_replay_pipeline.mjs [options]

Runs the replay rendering pipeline against a headful browser image:
  1. start container
  2. /configure display + kiosk flags
  3. upload built Kernel-Explainer app
  4. serve app via /process/spawn
  5. drive manual app start/end with raw CDP
  6. /recording/start, /recording/stop, /recording/download
  7. compare downloaded replay against the golden MP4

Options:
  --image <tag>             Docker image to run (default: ${DEFAULT_IMAGE})
  --golden <path>           Golden MP4 path (default: ${DEFAULT_GOLDEN})
  --app-dist <path>         Built static app directory (default: ${DEFAULT_APP_DIST})
  --golden-audio <path>     Audio reference path. Defaults to golden MP4 audio, or first app audio asset if golden has no audio.
  --out-dir <path>          Output directory for replay + reports (default: ${DEFAULT_OUT_DIR})
  --app-port <port>         Port for the in-container static app (default: 4173)
  --recording-framerate <n> Recording framerate to request from /recording/start (default: 10)
  --duration-tolerance <s>  Max absolute duration delta in seconds. Defaults to max(2.5, golden*0.08)
  --min-ssim <value>        Optional average SSIM threshold for sampled frame comparison
  --min-audio-correlation <value>
                            Minimum Pearson correlation for audio RMS-envelope comparison (default: 0.45)
  --keep-container          Leave the Docker container running after the verifier exits
  --skip-visual-compare     Skip sampled frame SSIM comparison
  --skip-audio-compare      Skip audio stream/reference comparison
  --help                    Show this help
`);
}

function parseArgs(argv) {
  const opts = {
    image: DEFAULT_IMAGE,
    golden: DEFAULT_GOLDEN,
    appDist: DEFAULT_APP_DIST,
    goldenAudio: null,
    outDir: DEFAULT_OUT_DIR,
    appPort: 4173,
    recordingFramerate: 10,
    durationTolerance: null,
    minSsim: null,
    minAudioCorrelation: 0.45,
    keepContainer: false,
    skipVisualCompare: false,
    skipAudioCompare: false,
  };

  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const next = () => {
      if (i + 1 >= argv.length) throw new Error(`${arg} requires a value`);
      return argv[++i];
    };
    switch (arg) {
      case '--image':
        opts.image = next();
        break;
      case '--golden':
        opts.golden = next();
        break;
      case '--app-dist':
        opts.appDist = next();
        break;
      case '--golden-audio':
        opts.goldenAudio = next();
        break;
      case '--out-dir':
        opts.outDir = next();
        break;
      case '--app-port':
        opts.appPort = Number(next());
        if (!Number.isInteger(opts.appPort) || opts.appPort <= 0) throw new Error('invalid --app-port');
        break;
      case '--recording-framerate':
        opts.recordingFramerate = Number(next());
        if (!Number.isInteger(opts.recordingFramerate) || opts.recordingFramerate < 1 || opts.recordingFramerate > 20) throw new Error('invalid --recording-framerate');
        break;
      case '--duration-tolerance':
        opts.durationTolerance = Number(next());
        if (!Number.isFinite(opts.durationTolerance) || opts.durationTolerance < 0) throw new Error('invalid --duration-tolerance');
        break;
      case '--min-ssim':
        opts.minSsim = Number(next());
        if (!Number.isFinite(opts.minSsim) || opts.minSsim < 0 || opts.minSsim > 1) throw new Error('invalid --min-ssim');
        break;
      case '--min-audio-correlation':
        opts.minAudioCorrelation = Number(next());
        if (!Number.isFinite(opts.minAudioCorrelation) || opts.minAudioCorrelation < 0 || opts.minAudioCorrelation > 1) throw new Error('invalid --min-audio-correlation');
        break;
      case '--keep-container':
        opts.keepContainer = true;
        break;
      case '--skip-visual-compare':
        opts.skipVisualCompare = true;
        break;
      case '--skip-audio-compare':
        opts.skipAudioCompare = true;
        break;
      case '--help':
      case '-h':
        usage();
        process.exit(0);
      default:
        throw new Error(`unknown option: ${arg}`);
    }
  }
  return opts;
}

function log(step, message, extra = undefined) {
  const suffix = extra === undefined ? '' : ` ${JSON.stringify(extra)}`;
  console.log(`[verify:${step}] ${message}${suffix}`);
}

function run(cmd, args, options = {}) {
  const res = spawnSync(cmd, args, {
    encoding: options.encoding ?? 'utf8',
    cwd: options.cwd,
    input: options.input,
    timeout: options.timeout,
    maxBuffer: options.maxBuffer ?? 20 * 1024 * 1024,
  });
  if (res.error) throw res.error;
  if (res.status !== 0) {
    const stdout = res.stdout ? String(res.stdout) : '';
    const stderr = res.stderr ? String(res.stderr) : '';
    throw new Error(`${cmd} ${args.join(' ')} failed with exit ${res.status}\nSTDOUT:\n${stdout}\nSTDERR:\n${stderr}`);
  }
  return res.stdout;
}

function commandExists(cmd) {
  const res = spawnSync('bash', ['-lc', `command -v ${cmd}`], { encoding: 'utf8' });
  return res.status === 0;
}

function requireCommand(cmd) {
  if (!commandExists(cmd)) {
    throw new Error(`required command not found: ${cmd}`);
  }
}

function abs(p) {
  return path.resolve(process.cwd(), p);
}

function ffprobeJSON(file) {
  const out = run('ffprobe', [
    '-v', 'error',
    '-show_entries', 'format=duration,size',
    '-show_streams',
    '-of', 'json',
    file,
  ], { maxBuffer: 50 * 1024 * 1024 });
  return JSON.parse(out);
}

function parseFPS(value) {
  if (!value || value === '0/0') return null;
  const [num, den] = value.split('/').map(Number);
  if (!Number.isFinite(num) || !Number.isFinite(den) || den === 0) return null;
  return num / den;
}

function videoInfo(file) {
  const probe = ffprobeJSON(file);
  const video = probe.streams?.find((s) => s.codec_type === 'video');
  const audio = probe.streams?.find((s) => s.codec_type === 'audio');
  if (!video) throw new Error(`no video stream found in ${file}`);
  const duration = Number(probe.format?.duration ?? video.duration);
  if (!Number.isFinite(duration) || duration <= 0) {
    throw new Error(`could not determine duration for ${file}`);
  }
  return {
    file,
    width: Number(video.width),
    height: Number(video.height),
    duration,
    fps: parseFPS(video.avg_frame_rate) ?? parseFPS(video.r_frame_rate),
    sizeBytes: Number(probe.format?.size ?? 0),
    hasAudio: Boolean(audio),
    videoCodec: video.codec_name,
    audioCodec: audio?.codec_name ?? null,
  };
}

function audioInfo(file) {
  const probe = ffprobeJSON(file);
  const audio = probe.streams?.find((s) => s.codec_type === 'audio');
  if (!audio) return null;
  const duration = Number(audio.duration ?? probe.format?.duration);
  if (!Number.isFinite(duration) || duration <= 0) {
    throw new Error(`could not determine audio duration for ${file}`);
  }
  return {
    file,
    duration,
    sampleRate: Number(audio.sample_rate ?? 0) || null,
    channels: Number(audio.channels ?? 0) || null,
    codec: audio.codec_name,
    sizeBytes: Number(probe.format?.size ?? 0),
  };
}

function findAudioAssets(dir, maxDepth = 4) {
  if (!existsSync(dir) || maxDepth < 0) return [];
  const extensions = new Set(['.aac', '.m4a', '.mp3', '.ogg', '.opus', '.wav']);
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry);
    let st;
    try {
      st = statSync(full);
    } catch {
      continue;
    }
    if (st.isDirectory()) {
      out.push(...findAudioAssets(full, maxDepth - 1));
    } else if (extensions.has(path.extname(entry).toLowerCase())) {
      out.push(full);
    }
  }
  return out.sort((a, b) => {
    const aInAudioDir = a.includes(`${path.sep}audio${path.sep}`) ? 0 : 1;
    const bInAudioDir = b.includes(`${path.sep}audio${path.sep}`) ? 0 : 1;
    return aInAudioDir - bInAudioDir || a.localeCompare(b);
  });
}

function resolveGoldenAudio(opts, goldenPath, appDist, golden) {
  if (opts.skipAudioCompare) return null;
  if (opts.goldenAudio) return abs(opts.goldenAudio);
  if (golden.hasAudio) return goldenPath;
  return findAudioAssets(appDist)[0] ?? null;
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function makeZip(srcDir, tmpRoot) {
  const zipPath = path.join(tmpRoot, 'app.zip');
  run('zip', ['-qr', zipPath, '.'], { cwd: srcDir, timeout: 120_000 });
  return zipPath;
}

async function sleep(ms) {
  await new Promise((resolve) => setTimeout(resolve, ms));
}

async function fetchText(url, options) {
  const resp = await fetch(url, options);
  const text = await resp.text();
  if (!resp.ok) {
    throw new Error(`${options?.method ?? 'GET'} ${url} failed: ${resp.status} ${resp.statusText}\n${text}`);
  }
  return { resp, text };
}

async function fetchJSON(url, options) {
  const { text } = await fetchText(url, options);
  return text ? JSON.parse(text) : null;
}

async function fetchBinary(url, options) {
  const resp = await fetch(url, options);
  const body = Buffer.from(await resp.arrayBuffer());
  if (!resp.ok) {
    throw new Error(`${options?.method ?? 'GET'} ${url} failed: ${resp.status} ${resp.statusText}\n${body.toString('utf8')}`);
  }
  return body;
}

async function waitForHTTP(url, timeoutMs, label) {
  const deadline = Date.now() + timeoutMs;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      const resp = await fetch(url, { signal: AbortSignal.timeout(2_000) });
      if (resp.ok) return;
      lastErr = new Error(`${resp.status} ${resp.statusText}`);
    } catch (err) {
      lastErr = err;
    }
    await sleep(500);
  }
  throw new Error(`timed out waiting for ${label} at ${url}: ${lastErr?.message ?? lastErr}`);
}

function dockerPort(containerName, containerPort) {
  const out = run('docker', ['port', containerName, `${containerPort}/tcp`]).trim().split('\n')[0];
  const match = out.match(/:(\d+)$/);
  if (!match) throw new Error(`could not parse docker port output for ${containerPort}: ${out}`);
  return Number(match[1]);
}

function dockerLogs(containerName, tail = 300) {
  try {
    return run('docker', ['logs', '--tail', String(tail), containerName], { maxBuffer: 10 * 1024 * 1024 });
  } catch (err) {
    return `failed to read docker logs: ${err.message}`;
  }
}

function startContainer(opts, golden) {
  const name = `replay-pipeline-${Date.now()}-${Math.random().toString(16).slice(2, 8)}`;
  const env = {
    WIDTH: String(golden.width),
    HEIGHT: String(golden.height),
    RECORD_AUDIO: 'true',
    KERNEL_IMAGES_API_RECORD_AUDIO: 'true',
    AUDIO_SOURCE: 'KernelOutput.monitor',
    CHROMIUM_FLAGS: '--no-sandbox',
  };

  const args = [
    'run', '-d',
    '--name', name,
    '--privileged',
    '--shm-size=2g',
    '-p', '127.0.0.1::10001',
    '-p', '127.0.0.1::9222',
    '-p', '127.0.0.1::9224',
  ];
  for (const [k, v] of Object.entries(env)) args.push('-e', `${k}=${v}`);
  args.push(opts.image);

  const id = run('docker', args, { timeout: 30_000 }).trim();
  return { id, name };
}

async function configureBrowser(apiBase, golden) {
  const form = new FormData();
  form.append('display', JSON.stringify({
    width: golden.width,
    height: golden.height,
    refresh_rate: 60,
    require_idle: false,
    restart_chromium: false,
  }));
  form.append('chromium_flags', JSON.stringify({
    flags: [
      '--kiosk',
      '--window-position=0,0',
      `--window-size=${golden.width},${golden.height}`,
      '--force-device-scale-factor=1',
      '--autoplay-policy=no-user-gesture-required',
    ],
  }));

  return fetchJSON(`${apiBase}/configure`, { method: 'POST', body: form, signal: AbortSignal.timeout(120_000) });
}

async function processExec(apiBase, body, timeoutMs = 30_000) {
  const result = await fetchJSON(`${apiBase}/process/exec`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(timeoutMs),
  });
  return {
    ...result,
    stdout: Buffer.from(result?.stdout_b64 ?? '', 'base64').toString('utf8'),
    stderr: Buffer.from(result?.stderr_b64 ?? '', 'base64').toString('utf8'),
  };
}

function requireExecOK(result, label) {
  if (result?.exit_code !== 0) {
    throw new Error(`${label} failed with exit ${result?.exit_code}\nSTDOUT:\n${result?.stdout ?? ''}\nSTDERR:\n${result?.stderr ?? ''}`);
  }
}

// /configure changes the active mode, but x11grab records the X root framebuffer.
// On the dummy Xorg driver the root framebuffer can remain at the previous max
// size unless we explicitly shrink it, so verify/correct it through the server
// process API before starting ffmpeg.
async function ensureXFramebuffer(apiBase, golden) {
  const script = `set -euo pipefail
export DISPLAY=:1
mode="${golden.width}x${golden.height}_60.00"
size="${golden.width}x${golden.height}"
output="$(xrandr --query | awk '/ connected/{print $1; exit}')"
if [ -z "$output" ]; then
  echo "no connected xrandr output found" >&2
  xrandr --query >&2
  exit 1
fi
if ! xrandr --query | grep -q "$size"; then
  echo "requested mode $size is not listed by xrandr" >&2
  xrandr --query >&2
  exit 1
fi
if xrandr --query | grep -q "$mode"; then
  xrandr --output "$output" --mode "$mode" --panning "$size"
else
  xrandr -s "$size"
fi
xrandr --fb "$size"
xrandr --query | awk '/^Screen / {gsub(",", "", $10); print $8 "x" $10; exit}'`;
  const result = await processExec(apiBase, { command: 'bash', args: ['-lc', script], timeout_sec: 20 }, 30_000);
  requireExecOK(result, 'x11 framebuffer resize');
  const dims = result.stdout.trim().split('\n').at(-1);
  assert(dims === `${golden.width}x${golden.height}`, `X root framebuffer ${dims} != golden ${golden.width}x${golden.height}`);
  return dims;
}

async function uploadApp(apiBase, zipPath) {
  await processExec(apiBase, { command: 'rm', args: ['-rf', '/tmp/kernel-replay-app', '/tmp/kernel-replay-server.mjs'] });

  const form = new FormData();
  form.append('zip_file', new Blob([readFileSync(zipPath)]), 'app.zip');
  form.append('dest_path', '/tmp/kernel-replay-app');
  await fetchText(`${apiBase}/fs/upload_zip`, { method: 'POST', body: form, signal: AbortSignal.timeout(120_000) });
}

function staticServerSource(appPort) {
  return `import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = '/tmp/kernel-replay-app';
const port = ${JSON.stringify(appPort)};
const mime = new Map([
  ['.html', 'text/html; charset=utf-8'],
  ['.js', 'text/javascript; charset=utf-8'],
  ['.css', 'text/css; charset=utf-8'],
  ['.svg', 'image/svg+xml'],
  ['.png', 'image/png'],
  ['.jpg', 'image/jpeg'],
  ['.jpeg', 'image/jpeg'],
  ['.webp', 'image/webp'],
  ['.mp4', 'video/mp4'],
  ['.mp3', 'audio/mpeg'],
  ['.m4a', 'audio/mp4'],
  ['.aac', 'audio/aac'],
  ['.ogg', 'audio/ogg'],
  ['.opus', 'audio/ogg'],
  ['.wav', 'audio/wav'],
  ['.json', 'application/json; charset=utf-8'],
]);

function safePath(urlPath) {
  const clean = decodeURIComponent(urlPath.split('?')[0]);
  const rel = clean === '/' ? '/index.html' : clean;
  const full = path.normalize(path.join(root, rel));
  if (!full.startsWith(root + path.sep) && full !== root) return null;
  return full;
}

const server = http.createServer((req, res) => {
  let full = safePath(req.url || '/');
  if (!full || !fs.existsSync(full) || fs.statSync(full).isDirectory()) {
    full = path.join(root, 'index.html');
  }
  res.setHeader('Cache-Control', 'no-store');
  res.setHeader('Content-Type', mime.get(path.extname(full)) || 'application/octet-stream');
  fs.createReadStream(full).pipe(res);
});

server.listen(port, '0.0.0.0', () => {
  console.log('kernel replay app listening on', port);
});
`;
}

async function writeFileViaAPI(apiBase, remotePath, data, mode = '644') {
  await fetchText(`${apiBase}/fs/write_file?path=${encodeURIComponent(remotePath)}&mode=${mode}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: data,
    signal: AbortSignal.timeout(30_000),
  });
}

async function spawnApp(apiBase, appPort) {
  await writeFileViaAPI(apiBase, '/tmp/kernel-replay-server.mjs', staticServerSource(appPort));
  const result = await fetchJSON(`${apiBase}/process/spawn`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ command: 'node', args: ['/tmp/kernel-replay-server.mjs'] }),
    signal: AbortSignal.timeout(30_000),
  });
  const processId = result?.process_id;
  if (!processId) throw new Error(`process/spawn response missing process_id: ${JSON.stringify(result)}`);
  return processId;
}

async function waitForAppInContainer(apiBase, appPort) {
  const code = `const url = 'http://127.0.0.1:${appPort}/';\nconst deadline = Date.now() + 15000;\nlet last = '';\nwhile (Date.now() < deadline) {\n  try {\n    const r = await fetch(url);\n    if (r.ok) process.exit(0);\n    last = r.status + ' ' + r.statusText;\n  } catch (e) { last = e.message; }\n  await new Promise(r => setTimeout(r, 250));\n}\nconsole.error(last);\nprocess.exit(1);`;
  const result = await fetchJSON(`${apiBase}/process/exec`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ command: 'node', args: ['--input-type=module', '-e', code], timeout_sec: 20 }),
    signal: AbortSignal.timeout(30_000),
  });
  if (result?.exit_code !== 0) {
    const stderr = Buffer.from(result?.stderr_b64 ?? '', 'base64').toString('utf8');
    throw new Error(`app did not become reachable in container: ${stderr}`);
  }
}

async function killProcess(apiBase, processId) {
  if (!processId) return;
  try {
    await fetchText(`${apiBase}/process/${processId}/kill`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ signal: 'TERM' }),
      signal: AbortSignal.timeout(10_000),
    });
  } catch (err) {
    log('cleanup', `failed to kill app process ${processId}: ${err.message}`);
  }
}

class CdpClient {
  constructor(ws) {
    this.ws = ws;
    this.nextId = 1;
    this.pending = new Map();
    this.waiters = [];
    this.events = [];
    ws.addEventListener('message', (event) => this.onMessage(event));
    ws.addEventListener('close', () => {
      for (const { reject } of this.pending.values()) reject(new Error('CDP websocket closed'));
      this.pending.clear();
    });
  }

  static async connect(wsURL) {
    const ws = new WebSocket(wsURL);
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`timeout connecting to ${wsURL}`)), 10_000);
      ws.addEventListener('open', () => { clearTimeout(timer); resolve(); }, { once: true });
      ws.addEventListener('error', (err) => { clearTimeout(timer); reject(err.error ?? err); }, { once: true });
    });
    return new CdpClient(ws);
  }

  onMessage(event) {
    let raw = event.data;
    if (Buffer.isBuffer(raw)) raw = raw.toString('utf8');
    if (raw instanceof ArrayBuffer) raw = Buffer.from(raw).toString('utf8');
    const msg = JSON.parse(String(raw));
    if (msg.id) {
      const pending = this.pending.get(msg.id);
      if (!pending) return;
      this.pending.delete(msg.id);
      if (msg.error) pending.reject(new Error(`${pending.method} failed: ${JSON.stringify(msg.error)}`));
      else pending.resolve(msg.result ?? {});
      return;
    }
    this.events.push(msg);
    const remaining = [];
    for (const waiter of this.waiters) {
      try {
        if (waiter.predicate(msg)) {
          clearTimeout(waiter.timer);
          waiter.resolve(msg);
        } else {
          remaining.push(waiter);
        }
      } catch (err) {
        clearTimeout(waiter.timer);
        waiter.reject(err);
      }
    }
    this.waiters = remaining;
  }

  send(method, params = {}, sessionId = undefined) {
    const id = this.nextId++;
    const msg = { id, method, params };
    if (sessionId) msg.sessionId = sessionId;
    this.ws.send(JSON.stringify(msg));
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject, method });
      setTimeout(() => {
        if (this.pending.delete(id)) reject(new Error(`${method} timed out`));
      }, 30_000).unref?.();
    });
  }

  waitForEvent(predicate, timeoutMs, label) {
    for (const event of this.events) {
      if (predicate(event)) return Promise.resolve(event);
    }
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.waiters = this.waiters.filter((w) => w.timer !== timer);
        reject(new Error(`timed out waiting for CDP event: ${label}`));
      }, timeoutMs);
      this.waiters.push({ predicate, resolve, reject, timer });
    });
  }

  close() {
    this.ws.close();
  }
}

async function fetchCDPVersionWithRetry(cdpBase, timeoutMs = 60_000) {
  const deadline = Date.now() + timeoutMs;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      const version = await fetchJSON(`${cdpBase}/json/version`, { signal: AbortSignal.timeout(5_000) });
      if (version?.webSocketDebuggerUrl) return version;
      lastErr = new Error(`/json/version missing webSocketDebuggerUrl: ${JSON.stringify(version)}`);
    } catch (err) {
      lastErr = err;
    }
    await sleep(500);
  }
  throw new Error(`timed out waiting for usable CDP /json/version: ${lastErr?.message ?? lastErr}`);
}

async function setupCDP(cdpBase, appURL, golden) {
  const version = await fetchCDPVersionWithRetry(cdpBase);
  const wsURL = version.webSocketDebuggerUrl;
  const cdp = await CdpClient.connect(wsURL);

  const replayEvents = [];
  const replayEventPredicate = (type) => (msg) => {
    if (msg.method !== 'Runtime.bindingCalled') return false;
    if (msg.params?.name !== '__kernelReplayEvent') return false;
    try {
      const payload = JSON.parse(msg.params.payload);
      replayEvents.push(payload);
      return payload.type === type;
    } catch {
      return false;
    }
  };

  const { targetId } = await cdp.send('Target.createTarget', { url: 'about:blank' });
  const { sessionId } = await cdp.send('Target.attachToTarget', { targetId, flatten: true });
  await cdp.send('Page.enable', {}, sessionId);
  await cdp.send('Runtime.enable', {}, sessionId);
  await cdp.send('Emulation.setDeviceMetricsOverride', {
    width: golden.width,
    height: golden.height,
    deviceScaleFactor: 1,
    mobile: false,
    screenWidth: golden.width,
    screenHeight: golden.height,
  }, sessionId);
  await cdp.send('Runtime.addBinding', { name: '__kernelReplayEvent' }, sessionId);

  const readyPromise = cdp.waitForEvent(replayEventPredicate('ready'), 20_000, 'kernel replay ready');
  await cdp.send('Page.navigate', { url: appURL }, sessionId);
  const readyMsg = await readyPromise;
  const ready = JSON.parse(readyMsg.params.payload);

  const metrics = await cdp.send('Runtime.evaluate', {
    expression: `({ innerWidth: window.innerWidth, innerHeight: window.innerHeight, devicePixelRatio: window.devicePixelRatio, href: location.href })`,
    returnByValue: true,
  }, sessionId);
  const viewport = metrics.result?.value;
  assert(viewport?.innerWidth === golden.width, `browser innerWidth ${viewport?.innerWidth} != golden width ${golden.width}`);
  assert(viewport?.innerHeight === golden.height, `browser innerHeight ${viewport?.innerHeight} != golden height ${golden.height}`);
  assert(viewport?.devicePixelRatio === 1, `browser devicePixelRatio ${viewport?.devicePixelRatio} != 1`);

  return {
    cdp,
    sessionId,
    ready,
    viewport,
    replayEvents,
    waitReplayEvent: (type, timeoutMs, label = type) => cdp.waitForEvent(replayEventPredicate(type), timeoutMs, `kernel replay ${label}`),
  };
}

async function startRecording(apiBase, golden, opts) {
  const maxDuration = Math.ceil(golden.duration + 30);
  const fps = opts.recordingFramerate;
  await fetchText(`${apiBase}/recording/start`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      maxDurationInSeconds: maxDuration,
      maxFileSizeInMB: 1000,
      framerate: fps,
    }),
    signal: AbortSignal.timeout(30_000),
  });
  return { maxDuration, fps };
}

async function stopRecording(apiBase) {
  await fetchText(`${apiBase}/recording/stop`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: '{}',
    signal: AbortSignal.timeout(120_000),
  });
}

async function forceStopRecording(apiBase) {
  try {
    await fetchText(`${apiBase}/recording/stop`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ forceStop: true }),
      signal: AbortSignal.timeout(15_000),
    });
  } catch {
    // Best effort cleanup.
  }
}

async function downloadRecording(apiBase) {
  return fetchBinary(`${apiBase}/recording/download`, { signal: AbortSignal.timeout(120_000) });
}

function parseSSIM(output) {
  const match = output.match(/All:([0-9.]+)/);
  if (!match) return null;
  return Number(match[1]);
}

function computeSampleSSIM(goldenPath, replayPath, golden, replay) {
  if (!commandExists('ffmpeg')) return null;
  const tmpRoot = mkdtempSync(path.join(tmpdir(), 'replay-ssim-'));
  const samples = [0.15, 0.5, 0.85];
  const results = [];
  try {
    for (let i = 0; i < samples.length; i++) {
      const ratio = samples[i];
      const goldenFrame = path.join(tmpRoot, `golden-${i}.png`);
      const replayFrame = path.join(tmpRoot, `replay-${i}.png`);
      const goldenTs = Math.max(0, Math.min(golden.duration - 0.1, golden.duration * ratio));
      const replayTs = Math.max(0, Math.min(replay.duration - 0.1, replay.duration * ratio));
      run('ffmpeg', ['-v', 'error', '-ss', String(goldenTs), '-i', goldenPath, '-frames:v', '1', '-vf', `scale=${golden.width}:${golden.height}`, '-y', goldenFrame], { timeout: 60_000 });
      run('ffmpeg', ['-v', 'error', '-ss', String(replayTs), '-i', replayPath, '-frames:v', '1', '-vf', `scale=${golden.width}:${golden.height}`, '-y', replayFrame], { timeout: 60_000 });
      const res = spawnSync('ffmpeg', ['-i', replayFrame, '-i', goldenFrame, '-lavfi', 'ssim', '-f', 'null', '-'], {
        encoding: 'utf8',
        timeout: 60_000,
        maxBuffer: 10 * 1024 * 1024,
      });
      const combined = `${res.stdout ?? ''}\n${res.stderr ?? ''}`;
      const ssim = parseSSIM(combined);
      if (ssim != null) results.push({ ratio, goldenTs, replayTs, ssim });
    }
  } finally {
    rmSync(tmpRoot, { recursive: true, force: true });
  }
  if (!results.length) return null;
  return {
    samples: results,
    average: results.reduce((sum, r) => sum + r.ssim, 0) / results.length,
  };
}

function extractMonoPCM(file, maxDurationSec) {
  const args = ['-v', 'error', '-i', file, '-vn'];
  if (maxDurationSec) args.push('-t', String(maxDurationSec));
  args.push('-ac', '1', '-ar', '16000', '-f', 's16le', '-');
  const res = spawnSync('ffmpeg', args, {
    timeout: 120_000,
    maxBuffer: 100 * 1024 * 1024,
  });
  if (res.error) throw res.error;
  if (res.status !== 0) {
    throw new Error(`ffmpeg audio extraction failed for ${file}\nSTDERR:\n${Buffer.from(res.stderr ?? '').toString('utf8')}`);
  }
  return Buffer.from(res.stdout ?? []);
}

function pcmEnvelope(pcm, sampleRate = 16000, windowMs = 100) {
  const sampleCount = Math.floor(pcm.length / 2);
  const windowSamples = Math.max(1, Math.floor(sampleRate * windowMs / 1000));
  const out = [];
  for (let start = 0; start + windowSamples <= sampleCount; start += windowSamples) {
    let sumSquares = 0;
    for (let i = 0; i < windowSamples; i++) {
      const sample = pcm.readInt16LE((start + i) * 2) / 32768;
      sumSquares += sample * sample;
    }
    out.push(Math.sqrt(sumSquares / windowSamples));
  }
  return out;
}

function mean(values) {
  if (!values.length) return 0;
  return values.reduce((sum, value) => sum + value, 0) / values.length;
}

function rms(values) {
  if (!values.length) return 0;
  return Math.sqrt(values.reduce((sum, value) => sum + value * value, 0) / values.length);
}

function pearsonAtLag(reference, candidate, lag, minOverlap) {
  const refStart = lag < 0 ? -lag : 0;
  const candStart = lag > 0 ? lag : 0;
  const count = Math.min(reference.length - refStart, candidate.length - candStart);
  if (count < minOverlap) return null;

  let refSum = 0;
  let candSum = 0;
  for (let i = 0; i < count; i++) {
    refSum += reference[refStart + i];
    candSum += candidate[candStart + i];
  }
  const refMean = refSum / count;
  const candMean = candSum / count;

  let covariance = 0;
  let refVariance = 0;
  let candVariance = 0;
  for (let i = 0; i < count; i++) {
    const refCentered = reference[refStart + i] - refMean;
    const candCentered = candidate[candStart + i] - candMean;
    covariance += refCentered * candCentered;
    refVariance += refCentered * refCentered;
    candVariance += candCentered * candCentered;
  }
  if (refVariance <= 0 || candVariance <= 0) return null;
  return covariance / Math.sqrt(refVariance * candVariance);
}

function bestEnvelopeCorrelation(reference, candidate, maxLagWindows) {
  const minOverlap = Math.floor(Math.min(reference.length, candidate.length) * 0.6);
  let best = null;
  for (let lag = -maxLagWindows; lag <= maxLagWindows; lag++) {
    const correlation = pearsonAtLag(reference, candidate, lag, minOverlap);
    if (correlation == null) continue;
    if (!best || correlation > best.correlation) {
      best = { correlation, lagWindows: lag };
    }
  }
  return best;
}

function compareAudio(referencePath, replayPath, opts) {
  if (!commandExists('ffmpeg')) return null;
  const reference = audioInfo(referencePath);
  const replay = audioInfo(replayPath);
  if (!reference) throw new Error(`audio reference has no audio stream: ${referencePath}`);
  if (!replay) throw new Error(`replay has no audio stream; expected audio similar to ${referencePath}`);

  const compareDuration = Math.min(reference.duration, replay.duration, 90);
  const referencePCM = extractMonoPCM(referencePath, compareDuration);
  const replayPCM = extractMonoPCM(replayPath, compareDuration);
  const envelopeWindowMs = 50;
  const maxLagSeconds = 3;
  const referenceEnvelope = pcmEnvelope(referencePCM, 16000, envelopeWindowMs);
  const replayEnvelope = pcmEnvelope(replayPCM, 16000, envelopeWindowMs);
  const best = bestEnvelopeCorrelation(referenceEnvelope, replayEnvelope, Math.round((maxLagSeconds * 1000) / envelopeWindowMs));
  if (!best) throw new Error('could not compute audio envelope correlation');

  const referenceRms = rms(referenceEnvelope);
  const replayRms = rms(replayEnvelope);
  const rmsRatio = referenceRms > 0 ? replayRms / referenceRms : null;
  const durationDelta = Math.abs(replay.duration - reference.duration);
  const durationTolerance = Math.max(2.5, reference.duration * 0.08);

  assert(durationDelta <= durationTolerance, `replay audio duration ${replay.duration.toFixed(3)}s differs from reference ${reference.duration.toFixed(3)}s by ${durationDelta.toFixed(3)}s (tolerance ${durationTolerance.toFixed(3)}s)`);
  assert(replayRms > Math.max(0.0005, referenceRms * 0.03), `replay audio RMS ${replayRms.toFixed(6)} is too low compared to reference ${referenceRms.toFixed(6)}`);
  assert(best.correlation >= opts.minAudioCorrelation, `audio envelope correlation ${best.correlation.toFixed(4)} < threshold ${opts.minAudioCorrelation}`);

  return {
    reference,
    replay,
    durationDelta,
    durationTolerance,
    compareDuration,
    envelopeWindowMs,
    maxLagSeconds,
    bestLagSeconds: (best.lagWindows * envelopeWindowMs) / 1000,
    correlation: best.correlation,
    referenceRms,
    replayRms,
    rmsRatio,
  };
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  requireCommand('docker');
  requireCommand('zip');
  requireCommand('ffprobe');

  const goldenPath = abs(opts.golden);
  const appDist = abs(opts.appDist);
  const outDir = abs(opts.outDir);

  assert(existsSync(goldenPath), `golden MP4 not found: ${goldenPath}`);
  assert(existsSync(path.join(appDist, 'index.html')), `built app index.html not found: ${path.join(appDist, 'index.html')}`);
  mkdirSync(outDir, { recursive: true });

  const golden = videoInfo(goldenPath);
  const goldenAudioPath = resolveGoldenAudio(opts, goldenPath, appDist, golden);
  const durationTolerance = opts.durationTolerance ?? Math.max(2.5, golden.duration * 0.08);
  log('golden', 'probed golden MP4', golden);
  let goldenAudio = null;
  if (goldenAudioPath) {
    assert(existsSync(goldenAudioPath), `golden audio reference not found: ${goldenAudioPath}`);
    goldenAudio = audioInfo(goldenAudioPath);
    assert(goldenAudio, `golden audio reference has no audio stream: ${goldenAudioPath}`);
    log('golden', 'probed audio reference', goldenAudio);
  } else if (!opts.skipAudioCompare) {
    log('golden', 'no audio reference found; audio comparison will be skipped');
  }

  const tmpRoot = mkdtempSync(path.join(tmpdir(), 'replay-pipeline-'));
  let container;
  let apiBase;
  let appProcessId;
  let cdpSession;
  let recordingStarted = false;

  try {
    const zipPath = makeZip(appDist, tmpRoot);
    log('docker', `starting ${opts.image}`);
    container = startContainer(opts, golden);
    log('docker', 'container started', { name: container.name, id: container.id });

    const apiPort = dockerPort(container.name, 10001);
    const cdpPort = dockerPort(container.name, 9222);
    apiBase = `http://127.0.0.1:${apiPort}`;
    const cdpBase = `http://127.0.0.1:${cdpPort}`;
    log('docker', 'mapped ports', { apiPort, cdpPort });

    await waitForHTTP(`${apiBase}/spec.yaml`, 180_000, 'Kernel Images API');
    await waitForHTTP(`${cdpBase}/json/version`, 180_000, 'CDP proxy');
    log('api', 'API and CDP are ready');

    log('configure', 'applying display size and kiosk flags', { width: golden.width, height: golden.height });
    await configureBrowser(apiBase, golden);
    await waitForHTTP(`${cdpBase}/json/version`, 60_000, 'CDP proxy after configure');
    const framebuffer = await ensureXFramebuffer(apiBase, golden);
    log('configure', 'configure completed', { framebuffer });

    log('app', 'uploading built app');
    await uploadApp(apiBase, zipPath);
    appProcessId = await spawnApp(apiBase, opts.appPort);
    await waitForAppInContainer(apiBase, opts.appPort);
    const appURL = `http://127.0.0.1:${opts.appPort}/?kernelReplayControl=manual`;
    log('app', 'app is running', { processId: appProcessId, appURL });

    log('cdp', 'attaching to browser and waiting for manual replay ready');
    cdpSession = await setupCDP(cdpBase, appURL, golden);
    log('cdp', 'page ready with expected viewport', { ready: cdpSession.ready, viewport: cdpSession.viewport });

    log('recording', 'starting server replay recorder');
    const recordingConfig = await startRecording(apiBase, golden, opts);
    recordingStarted = true;
    log('recording', 'recorder started', recordingConfig);

    const startSeen = cdpSession.waitReplayEvent('start', 5_000);
    await cdpSession.cdp.send('Runtime.evaluate', {
      expression: 'window.startRecording() === undefined ? null : undefined',
      awaitPromise: true,
      returnByValue: true,
    }, cdpSession.sessionId);
    const startMsg = await startSeen;
    log('cdp', 'observed app start event', JSON.parse(startMsg.params.payload));

    const stopTimeoutMs = Math.ceil((golden.duration + 15) * 1000);
    const stopMsg = await cdpSession.waitReplayEvent('stop', stopTimeoutMs);
    log('cdp', 'observed app stop event', JSON.parse(stopMsg.params.payload));

    log('recording', 'stopping server replay recorder');
    await stopRecording(apiBase);
    recordingStarted = false;

    const replayBytes = await downloadRecording(apiBase);
    const stamp = new Date().toISOString().replace(/[:.]/g, '-');
    const replayPath = path.join(outDir, `replay-${stamp}.mp4`);
    writeFileSync(replayPath, replayBytes);
    log('download', 'saved replay', { replayPath, bytes: replayBytes.length });

    const replay = videoInfo(replayPath);
    log('replay', 'probed replay MP4', replay);

    assert(replay.width === golden.width, `replay width ${replay.width} != golden width ${golden.width}`);
    assert(replay.height === golden.height, `replay height ${replay.height} != golden height ${golden.height}`);
    assert(replay.sizeBytes > 100_000, `replay file unexpectedly small: ${replay.sizeBytes} bytes`);

    const durationDelta = Math.abs(replay.duration - golden.duration);
    assert(durationDelta <= durationTolerance, `replay duration ${replay.duration.toFixed(3)}s differs from golden ${golden.duration.toFixed(3)}s by ${durationDelta.toFixed(3)}s (tolerance ${durationTolerance.toFixed(3)}s)`);

    let visual = null;
    if (!opts.skipVisualCompare) {
      try {
        visual = computeSampleSSIM(goldenPath, replayPath, golden, replay);
        if (visual) {
          log('compare', 'sampled frame SSIM', visual);
          if (opts.minSsim != null) {
            assert(visual.average >= opts.minSsim, `average SSIM ${visual.average.toFixed(4)} < threshold ${opts.minSsim}`);
          }
        } else {
          log('compare', 'SSIM unavailable');
        }
      } catch (err) {
        if (opts.minSsim != null) throw err;
        log('compare', `SSIM comparison failed (non-fatal): ${err.message}`);
      }
    }

    let audio = null;
    if (!opts.skipAudioCompare && goldenAudioPath) {
      audio = compareAudio(goldenAudioPath, replayPath, opts);
      assert(audio, 'audio comparison unavailable; ffmpeg is required when an audio reference is present');
      log('compare', 'audio envelope correlation', audio);
    }

    const report = {
      ok: true,
      image: opts.image,
      golden,
      goldenAudio,
      replay,
      durationDelta,
      durationTolerance,
      viewport: cdpSession.viewport,
      appReady: cdpSession.ready,
      replayEvents: cdpSession.replayEvents,
      visual,
      audio,
      replayPath,
    };
    const reportPath = path.join(outDir, `report-${stamp}.json`);
    writeFileSync(reportPath, JSON.stringify(report, null, 2));
    log('result', 'verifier passed', { replayPath, reportPath });
  } catch (err) {
    if (container?.name) {
      console.error('\n--- container logs (tail) ---');
      console.error(dockerLogs(container.name));
      console.error('--- end container logs ---\n');
    }
    throw err;
  } finally {
    if (recordingStarted && apiBase) await forceStopRecording(apiBase);
    if (appProcessId && apiBase) await killProcess(apiBase, appProcessId);
    if (cdpSession) cdpSession.cdp.close();
    rmSync(tmpRoot, { recursive: true, force: true });
    if (container?.name && !opts.keepContainer) {
      try {
        run('docker', ['rm', '-f', container.name], { timeout: 30_000 });
        log('cleanup', 'removed container', { name: container.name });
      } catch (err) {
        log('cleanup', `failed to remove container ${container.name}: ${err.message}`);
      }
    } else if (container?.name) {
      log('cleanup', 'kept container', { name: container.name });
    }
  }
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((err) => {
    console.error(`\nVerifier failed: ${err.stack ?? err.message}`);
    process.exit(1);
  });
}
