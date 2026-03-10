#!/usr/bin/env node
/**
 * Unikernel benchmark: measures memory, CPU, and CDP latency
 * for baseline headless vs CDP live-view headless instances.
 */

import { WebSocket } from 'ws';
import https from 'https';

const ITERATIONS = 5;
const URLS = [
  ['Wikipedia', 'https://en.wikipedia.org/wiki/Main_Page'],
  ['Apple', 'https://www.apple.com'],
  ['GitHub', 'https://github.com'],
  ['Hacker News', 'https://news.ycombinator.com'],
];

const agent = new https.Agent({ rejectUnauthorized: false });

function httpsGet(url) {
  return new Promise((resolve, reject) => {
    https.get(url, { agent }, (res) => {
      let data = '';
      res.on('data', (chunk) => data += chunk);
      res.on('end', () => resolve({ status: res.statusCode, data }));
    }).on('error', reject);
  });
}

function httpsPost(url, body) {
  return new Promise((resolve, reject) => {
    const u = new URL(url);
    const opts = {
      hostname: u.hostname, port: u.port || 443, path: u.pathname,
      method: 'POST', agent,
      headers: { 'Content-Type': 'application/json' },
    };
    const req = https.request(opts, (res) => {
      let data = '';
      res.on('data', (chunk) => data += chunk);
      res.on('end', () => resolve({ status: res.statusCode, data }));
    });
    req.on('error', reject);
    req.write(JSON.stringify(body));
    req.end();
  });
}

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }
function median(arr) {
  const sorted = [...arr].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

class CDPBench {
  constructor(host, label) {
    this.host = host;
    this.label = label;
    this.ws = null;
    this.msgId = 0;
    this.pending = new Map();
    this.events = [];
  }

  async connect() {
    const url = `wss://${this.host}:9222/devtools/browser`;
    return new Promise((resolve, reject) => {
      this.ws = new WebSocket(url, { rejectUnauthorized: false });
      this.ws.on('open', () => resolve());
      this.ws.on('error', (e) => reject(e));
      this.ws.on('message', (raw) => {
        const msg = JSON.parse(raw.toString());
        if (msg.id && this.pending.has(msg.id)) {
          this.pending.get(msg.id)(msg);
          this.pending.delete(msg.id);
        } else {
          this.events.push(msg);
        }
      });
    });
  }

  send(method, params, sessionId) {
    return new Promise((resolve) => {
      this.msgId++;
      const msg = { id: this.msgId, method };
      if (params) msg.params = params;
      if (sessionId) msg.sessionId = sessionId;
      this.pending.set(this.msgId, resolve);
      this.ws.send(JSON.stringify(msg));
    });
  }

  waitForEvent(name, sessionId, timeoutMs = 30000) {
    const idx = this.events.findIndex(e =>
      e.method === name && (!sessionId || e.sessionId === sessionId));
    if (idx >= 0) return Promise.resolve(this.events.splice(idx, 1)[0]);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`Timeout waiting for ${name}`)), timeoutMs);
      const check = setInterval(() => {
        const i = this.events.findIndex(e =>
          e.method === name && (!sessionId || e.sessionId === sessionId));
        if (i >= 0) {
          clearInterval(check);
          clearTimeout(timer);
          resolve(this.events.splice(i, 1)[0]);
        }
      }, 10);
    });
  }

  async createTarget() {
    const resp = await this.send('Target.createTarget', { url: 'about:blank' });
    const targetId = resp.result.targetId;
    const attach = await this.send('Target.attachToTarget', { targetId, flatten: true });
    return { targetId, sessionId: attach.result.sessionId };
  }

  close() { if (this.ws) this.ws.close(); }
}

async function execInContainer(host, command, args) {
  const url = `https://${host}:444/process/exec`;
  const resp = await httpsPost(url, { command, args: args || [], timeout: 10 });
  const parsed = JSON.parse(resp.data);
  const stdout = parsed.stdout_b64 ? Buffer.from(parsed.stdout_b64, 'base64').toString() : '';
  const stderr = parsed.stderr_b64 ? Buffer.from(parsed.stderr_b64, 'base64').toString() : '';
  return { stdout, stderr, exitCode: parsed.exit_code };
}

async function getMemInfo(host) {
  const { stdout } = await execInContainer(host, 'cat', ['/proc/meminfo']);
  const lines = stdout.split('\n');
  const vals = {};
  for (const line of lines) {
    const m = line.match(/^(\w+):\s+(\d+)/);
    if (m) vals[m[1]] = parseInt(m[2]);
  }
  const totalKB = vals.MemTotal || 0;
  const availKB = vals.MemAvailable || 0;
  const freeKB = vals.MemFree || 0;
  const buffersKB = vals.Buffers || 0;
  const cachedKB = vals.Cached || 0;
  const usedKB = totalKB - freeKB - buffersKB - cachedKB;
  return {
    totalMB: totalKB / 1024,
    usedMB: usedKB / 1024,
    availableMB: availKB / 1024,
    freeMB: freeKB / 1024,
    cachedMB: cachedKB / 1024,
  };
}

async function getCPUInfo(host) {
  const { stdout } = await execInContainer(host, 'cat', ['/proc/stat']);
  const cpuLine = stdout.split('\n').find(l => l.startsWith('cpu '));
  if (!cpuLine) return null;
  const parts = cpuLine.split(/\s+/).slice(1).map(Number);
  const [user, nice, system, idle, iowait, irq, softirq, steal] = parts;
  const total = user + nice + system + idle + (iowait || 0) + (irq || 0) + (softirq || 0) + (steal || 0);
  const busy = total - idle - (iowait || 0);
  return { user, nice, system, idle, iowait: iowait || 0, total, busy, busyPct: (busy / total * 100) };
}

async function getProcessList(host) {
  const { stdout } = await execInContainer(host, 'ps', ['aux']);
  return stdout;
}

async function warmup(host, label) {
  for (let i = 0; i < 40; i++) {
    try {
      await httpsGet(`https://${host}:444/spec.json`);
      console.log(`  ${label}: ready`);
      return true;
    } catch {
      await sleep(5000);
    }
  }
  console.log(`  ${label}: FAILED to warm up`);
  return false;
}

async function benchmarkInstance(host, label) {
  console.log(`\n${'='.repeat(70)}`);
  console.log(`  BENCHMARK: ${label}`);
  console.log(`  Host: ${host}`);
  console.log(`  Iterations per test: ${ITERATIONS}`);
  console.log(`${'='.repeat(70)}\n`);

  // --- Resource snapshot BEFORE workload ---
  console.log('--- Resource Usage (before workload) ---');
  const memBefore = await getMemInfo(host);
  console.log(`  Memory total:     ${memBefore.totalMB.toFixed(0)} MB`);
  console.log(`  Memory used:      ${memBefore.usedMB.toFixed(0)} MB`);
  console.log(`  Memory available: ${memBefore.availableMB.toFixed(0)} MB`);
  console.log(`  Memory cached:    ${memBefore.cachedMB.toFixed(0)} MB`);

  const cpuBefore = await getCPUInfo(host);
  if (cpuBefore) {
    console.log(`  CPU busy:         ${cpuBefore.busyPct.toFixed(1)}% (cumulative since boot)`);
  }

  // --- Process list ---
  console.log('\n--- Process List ---');
  const ps = await getProcessList(host);
  console.log(ps);

  // --- CDP benchmark ---
  const cdp = new CDPBench(host, label);
  await cdp.connect();
  const { targetId, sessionId } = await cdp.createTarget();
  await cdp.send('Page.enable', null, sessionId);
  await cdp.send('Runtime.enable', null, sessionId);

  // Navigation
  console.log('--- Navigation Latency ---');
  const navResults = {};
  for (const [name, url] of URLS) {
    const times = [];
    for (let i = 0; i < ITERATIONS; i++) {
      cdp.events = [];
      const start = performance.now();
      await cdp.send('Page.navigate', { url }, sessionId);
      try {
        await cdp.waitForEvent('Page.loadEventFired', sessionId, 30000);
      } catch { /* ok */ }
      times.push((performance.now() - start) / 1000);
      await sleep(500);
    }
    const med = median(times);
    console.log(`  ${name.padEnd(20)} median=${med.toFixed(3)}s  [${times.map(t => t.toFixed(3)).join(', ')}]`);
    navResults[name] = { median: med, raw: times };
  }

  // Settle on a page for operation benchmarks
  await cdp.send('Page.navigate', { url: 'https://en.wikipedia.org/wiki/Main_Page' }, sessionId);
  await sleep(3000);
  cdp.events = [];

  console.log('\n--- CDP Operation Latency ---');

  // Screenshot
  const ssTimes = [], ssSizes = [];
  for (let i = 0; i < ITERATIONS; i++) {
    const start = performance.now();
    const resp = await cdp.send('Page.captureScreenshot', { format: 'png' }, sessionId);
    ssTimes.push((performance.now() - start) / 1000);
    ssSizes.push(Buffer.from(resp.result?.data || '', 'base64').length);
    await sleep(200);
  }
  console.log(`  ${'Screenshot'.padEnd(20)} median=${median(ssTimes).toFixed(3)}s  size=${(ssSizes.reduce((a,b)=>a+b,0)/ssSizes.length/1024).toFixed(0)}KB`);

  // JS Evaluate
  const evalTimes = [];
  for (let i = 0; i < ITERATIONS; i++) {
    const start = performance.now();
    await cdp.send('Runtime.evaluate', { expression: 'document.title' }, sessionId);
    evalTimes.push((performance.now() - start) / 1000);
  }
  console.log(`  ${'JS Evaluate'.padEnd(20)} median=${(median(evalTimes)*1000).toFixed(1)}ms`);

  // Mouse Click
  const clickTimes = [];
  for (let i = 0; i < ITERATIONS; i++) {
    const start = performance.now();
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: 100, y: 100, button: 'left', clickCount: 1 }, sessionId);
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: 100, y: 100, button: 'left', clickCount: 1 }, sessionId);
    clickTimes.push((performance.now() - start) / 1000);
  }
  console.log(`  ${'Mouse Click'.padEnd(20)} median=${(median(clickTimes)*1000).toFixed(1)}ms`);

  // Keyboard Type
  const typeTimes = [];
  for (let i = 0; i < ITERATIONS; i++) {
    const start = performance.now();
    for (const ch of 'hello world') {
      await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', text: ch }, sessionId);
      await cdp.send('Input.dispatchKeyEvent', { type: 'keyUp' }, sessionId);
    }
    typeTimes.push((performance.now() - start) / 1000);
  }
  console.log(`  ${'Type 11 chars'.padEnd(20)} median=${(median(typeTimes)*1000).toFixed(1)}ms`);

  // Layout Metrics
  const lmTimes = [];
  for (let i = 0; i < ITERATIONS; i++) {
    const start = performance.now();
    await cdp.send('Page.getLayoutMetrics', null, sessionId);
    lmTimes.push((performance.now() - start) / 1000);
  }
  console.log(`  ${'Layout Metrics'.padEnd(20)} median=${(median(lmTimes)*1000).toFixed(1)}ms`);

  // --- Resource snapshot AFTER workload ---
  console.log('\n--- Resource Usage (after workload) ---');
  const memAfter = await getMemInfo(host);
  console.log(`  Memory total:     ${memAfter.totalMB.toFixed(0)} MB`);
  console.log(`  Memory used:      ${memAfter.usedMB.toFixed(0)} MB`);
  console.log(`  Memory available: ${memAfter.availableMB.toFixed(0)} MB`);
  console.log(`  Memory cached:    ${memAfter.cachedMB.toFixed(0)} MB`);

  const cpuAfter = await getCPUInfo(host);
  if (cpuAfter) {
    console.log(`  CPU busy:         ${cpuAfter.busyPct.toFixed(1)}% (cumulative since boot)`);
  }

  // Measure CPU during a short active period
  console.log('\n--- CPU Usage (5s active sampling with page navigations) ---');
  const cpuStart = await getCPUInfo(host);
  const wallStart = Date.now();
  // Drive some activity during the sampling
  for (let i = 0; i < 3; i++) {
    await cdp.send('Page.navigate', { url: URLS[i % URLS.length][1] }, sessionId);
    await sleep(1500);
  }
  const cpuEnd = await getCPUInfo(host);
  const wallElapsed = (Date.now() - wallStart) / 1000;
  if (cpuStart && cpuEnd) {
    const tickDelta = cpuEnd.total - cpuStart.total;
    const busyDelta = cpuEnd.busy - cpuStart.busy;
    const idleDelta = (cpuEnd.idle + cpuEnd.iowait) - (cpuStart.idle + cpuStart.iowait);
    const cpuPct = tickDelta > 0 ? (busyDelta / tickDelta * 100) : 0;
    console.log(`  Wall time:        ${wallElapsed.toFixed(1)}s`);
    console.log(`  CPU busy ticks:   ${busyDelta}  idle ticks: ${idleDelta}  total: ${tickDelta}`);
    console.log(`  CPU utilization:  ${cpuPct.toFixed(1)}%`);
  }

  await cdp.send('Target.closeTarget', { targetId });
  cdp.close();

  return {
    memBefore, memAfter, cpuBefore, cpuAfter,
    navResults,
    screenshot: { median: median(ssTimes), avgSizeKB: ssSizes.reduce((a,b)=>a+b,0)/ssSizes.length/1024 },
    jsEvaluate: { medianMs: median(evalTimes) * 1000 },
    mouseClick: { medianMs: median(clickTimes) * 1000 },
    keyboardType: { medianMs: median(typeTimes) * 1000 },
    layoutMetrics: { medianMs: median(lmTimes) * 1000 },
  };
}

async function main() {
  const instances = [
    ['BASELINE (v29 headless, no live view)', 'winter-mountain-2k9xdihk.dev-iad-unikraft-3.onkernel.app'],
    ['CDP LIVE VIEW (headless + screencast)', 'autumn-shape-25lzr63z.dev-iad-unikraft-3.onkernel.app'],
  ];

  console.log('Warming up instances...');
  for (const [label, host] of instances) {
    const ok = await warmup(host, label);
    if (!ok) { console.log(`Skipping ${label}`); continue; }
  }

  // Let instances settle after warmup
  await sleep(5000);

  const allResults = {};
  for (const [label, host] of instances) {
    try {
      allResults[label] = await benchmarkInstance(host, label);
    } catch (e) {
      console.log(`\n  ERROR benchmarking ${label}: ${e.message}\n`);
      console.log(e.stack);
    }
  }

  const labels = Object.keys(allResults);
  if (labels.length < 2) {
    console.log('Not enough successful benchmarks to compare.');
    return;
  }

  // --- Comparison ---
  console.log(`\n${'='.repeat(90)}`);
  console.log('  COMPARISON SUMMARY');
  console.log(`${'='.repeat(90)}\n`);

  const shortLabels = labels.map(l => l.includes('BASELINE') ? 'Baseline' : 'CDP LiveView');

  let header = 'Metric'.padEnd(28);
  for (const sl of shortLabels) header += sl.padStart(16);
  header += '     Delta'.padStart(16);
  console.log(header);
  console.log('-'.repeat(76));

  const r = labels.map(l => allResults[l]);

  const rows = [
    ['Mem used (before)', r.map(x => `${x.memBefore.usedMB.toFixed(0)} MB`), r.map(x => x.memBefore.usedMB)],
    ['Mem used (after)', r.map(x => `${x.memAfter.usedMB.toFixed(0)} MB`), r.map(x => x.memAfter.usedMB)],
    ['Mem available (after)', r.map(x => `${x.memAfter.availableMB.toFixed(0)} MB`), r.map(x => x.memAfter.availableMB)],
    ['', [], []],
    ...URLS.map(([name]) => [
      `Nav ${name}`,
      r.map(x => `${x.navResults[name].median.toFixed(3)}s`),
      r.map(x => x.navResults[name].median),
    ]),
    ['', [], []],
    ['Screenshot', r.map(x => `${x.screenshot.median.toFixed(3)}s`), r.map(x => x.screenshot.median)],
    ['Screenshot size', r.map(x => `${x.screenshot.avgSizeKB.toFixed(0)} KB`), r.map(x => x.screenshot.avgSizeKB)],
    ['JS Evaluate', r.map(x => `${x.jsEvaluate.medianMs.toFixed(1)}ms`), r.map(x => x.jsEvaluate.medianMs)],
    ['Mouse Click', r.map(x => `${x.mouseClick.medianMs.toFixed(1)}ms`), r.map(x => x.mouseClick.medianMs)],
    ['Type 11 chars', r.map(x => `${x.keyboardType.medianMs.toFixed(1)}ms`), r.map(x => x.keyboardType.medianMs)],
    ['Layout Metrics', r.map(x => `${x.layoutMetrics.medianMs.toFixed(1)}ms`), r.map(x => x.layoutMetrics.medianMs)],
  ];

  for (const [label, vals, nums] of rows) {
    if (!label) { console.log(); continue; }
    let row = label.padEnd(28);
    for (const v of vals) row += v.padStart(16);
    if (nums.length === 2) {
      const delta = nums[1] - nums[0];
      const pct = nums[0] !== 0 ? (delta / nums[0] * 100) : 0;
      const sign = delta >= 0 ? '+' : '';
      row += `  ${sign}${pct.toFixed(1)}%`.padStart(16);
    }
    console.log(row);
  }
  console.log();
}

main().catch(console.error);
