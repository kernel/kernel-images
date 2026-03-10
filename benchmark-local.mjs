#!/usr/bin/env node
/**
 * Local CDP benchmark: baseline (no live view) vs CDP live view.
 * Both run the same Docker image, only difference is ENABLE_LIVE_VIEW.
 */

import { WebSocket } from 'ws';
import http from 'http';

const ITERATIONS = 5;
const URLS = [
  ['Wikipedia', 'https://en.wikipedia.org/wiki/Main_Page'],
  ['Apple', 'https://www.apple.com'],
  ['GitHub', 'https://github.com'],
  ['Hacker News', 'https://news.ycombinator.com'],
];

function httpGet(url) {
  return new Promise((resolve, reject) => {
    http.get(url, (res) => {
      let data = '';
      res.on('data', (chunk) => data += chunk);
      res.on('end', () => resolve({ status: res.statusCode, data }));
    }).on('error', reject);
  });
}

class CDPBench {
  constructor(wsPort, apiPort, label) {
    this.wsPort = wsPort;
    this.apiPort = apiPort;
    this.label = label;
    this.ws = null;
    this.msgId = 0;
    this.pending = new Map();
    this.events = [];
  }

  async connect() {
    const url = `ws://127.0.0.1:${this.wsPort}`;
    return new Promise((resolve, reject) => {
      this.ws = new WebSocket(url);
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
    return new Promise((resolve, reject) => {
      this.msgId++;
      const msg = { id: this.msgId, method };
      if (params) msg.params = params;
      if (sessionId) msg.sessionId = sessionId;
      this.pending.set(this.msgId, resolve);
      this.ws.send(JSON.stringify(msg));
      setTimeout(() => {
        if (this.pending.has(this.msgId)) {
          this.pending.delete(this.msgId);
          reject(new Error(`Timeout: ${method}`));
        }
      }, 30000);
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

  async benchNavigate(sessionId, name, url) {
    const times = [];
    for (let i = 0; i < ITERATIONS; i++) {
      this.events = [];
      const start = performance.now();
      await this.send('Page.navigate', { url }, sessionId);
      try {
        await this.waitForEvent('Page.loadEventFired', sessionId, 30000);
      } catch { /* timeout ok */ }
      times.push((performance.now() - start) / 1000);
      await sleep(300);
    }
    return times;
  }

  async benchScreenshot(sessionId) {
    const times = [];
    const sizes = [];
    for (let i = 0; i < ITERATIONS; i++) {
      const start = performance.now();
      const resp = await this.send('Page.captureScreenshot', { format: 'png' }, sessionId);
      times.push((performance.now() - start) / 1000);
      const data = resp.result?.data || '';
      sizes.push(Buffer.from(data, 'base64').length);
      await sleep(100);
    }
    return { times, sizes };
  }

  async benchEvaluate(sessionId) {
    const times = [];
    for (let i = 0; i < ITERATIONS; i++) {
      const start = performance.now();
      await this.send('Runtime.evaluate', { expression: 'document.title' }, sessionId);
      times.push((performance.now() - start) / 1000);
    }
    return times;
  }

  async benchClick(sessionId) {
    const times = [];
    for (let i = 0; i < ITERATIONS; i++) {
      const start = performance.now();
      await this.send('Input.dispatchMouseEvent',
        { type: 'mousePressed', x: 100, y: 100, button: 'left', clickCount: 1 }, sessionId);
      await this.send('Input.dispatchMouseEvent',
        { type: 'mouseReleased', x: 100, y: 100, button: 'left', clickCount: 1 }, sessionId);
      times.push((performance.now() - start) / 1000);
    }
    return times;
  }

  async benchType(sessionId) {
    const text = 'hello world';
    const times = [];
    for (let i = 0; i < ITERATIONS; i++) {
      const start = performance.now();
      for (const ch of text) {
        await this.send('Input.dispatchKeyEvent', { type: 'keyDown', text: ch }, sessionId);
        await this.send('Input.dispatchKeyEvent', { type: 'keyUp' }, sessionId);
      }
      times.push((performance.now() - start) / 1000);
    }
    return times;
  }

  async benchLayoutMetrics(sessionId) {
    const times = [];
    for (let i = 0; i < ITERATIONS; i++) {
      const start = performance.now();
      await this.send('Page.getLayoutMetrics', null, sessionId);
      times.push((performance.now() - start) / 1000);
    }
    return times;
  }

  async getProcessMemory() {
    try {
      const resp = await httpGet(`http://127.0.0.1:${this.apiPort}/process/exec`);
      return null;
    } catch { return null; }
  }

  async getMemInfo() {
    // Read /proc/meminfo via the API
    try {
      const resp = await new Promise((resolve, reject) => {
        const postData = JSON.stringify({ command: 'cat', args: ['/proc/meminfo'] });
        const req = http.request({
          hostname: '127.0.0.1', port: this.apiPort,
          path: '/process/exec', method: 'POST',
          headers: { 'Content-Type': 'application/json', 'Content-Length': postData.length }
        }, (res) => {
          let data = '';
          res.on('data', (chunk) => data += chunk);
          res.on('end', () => resolve(JSON.parse(data)));
        });
        req.on('error', reject);
        req.write(postData);
        req.end();
      });
      const stdout = Buffer.from(resp.stdout_b64 || '', 'base64').toString();
      const memTotal = parseInt(stdout.match(/MemTotal:\s+(\d+)/)?.[1] || '0');
      const memFree = parseInt(stdout.match(/MemFree:\s+(\d+)/)?.[1] || '0');
      const memAvail = parseInt(stdout.match(/MemAvailable:\s+(\d+)/)?.[1] || '0');
      const cached = parseInt(stdout.match(/Cached:\s+(\d+)/)?.[1] || '0');
      return { totalMB: memTotal / 1024, freeMB: memFree / 1024, usedMB: (memTotal - memFree) / 1024, availMB: memAvail / 1024 };
    } catch (e) {
      return { totalMB: 0, freeMB: 0, usedMB: 0, availMB: 0 };
    }
  }

  async run() {
    console.log(`\n${'='.repeat(60)}`);
    console.log(`  BENCHMARK: ${this.label}`);
    console.log(`  CDP: ws://127.0.0.1:${this.wsPort}  API: :${this.apiPort}`);
    console.log(`  Iterations per test: ${ITERATIONS}`);
    console.log(`${'='.repeat(60)}\n`);

    // Memory before
    const memBefore = await this.getMemInfo();
    console.log(`  Memory before: ${memBefore.usedMB.toFixed(0)} MB used / ${memBefore.totalMB.toFixed(0)} MB total\n`);

    await this.connect();
    const { targetId, sessionId } = await this.createTarget();
    await this.send('Page.enable', null, sessionId);
    await this.send('Runtime.enable', null, sessionId);

    const results = { memBefore };

    // Navigation benchmarks
    console.log('--- Navigation Latency ---');
    for (const [name, url] of URLS) {
      const times = await this.benchNavigate(sessionId, name, url);
      const med = median(times);
      console.log(`  ${name.padEnd(20)} median=${med.toFixed(3)}s  min=${Math.min(...times).toFixed(3)}s  max=${Math.max(...times).toFixed(3)}s`);
      results[`nav_${name}`] = { median: med, min: Math.min(...times), max: Math.max(...times) };
    }

    // Navigate to Wikipedia for remaining tests
    await this.send('Page.navigate', { url: 'https://en.wikipedia.org/wiki/Main_Page' }, sessionId);
    await sleep(3000);
    this.events = [];

    console.log('\n--- CDP Operation Latency ---');

    const ss = await this.benchScreenshot(sessionId);
    const ssMed = median(ss.times);
    const avgSize = ss.sizes.reduce((a, b) => a + b, 0) / ss.sizes.length;
    console.log(`  ${'Screenshot'.padEnd(20)} median=${ssMed.toFixed(3)}s  size=${(avgSize/1024).toFixed(0)}KB`);
    results.screenshot = { median: ssMed, avgSizeKB: avgSize / 1024 };

    const evalTimes = await this.benchEvaluate(sessionId);
    const evalMed = median(evalTimes);
    console.log(`  ${'JS Evaluate'.padEnd(20)} median=${(evalMed * 1000).toFixed(1)}ms`);
    results.js_evaluate = { medianMs: evalMed * 1000 };

    const clickTimes = await this.benchClick(sessionId);
    const clickMed = median(clickTimes);
    console.log(`  ${'Mouse Click'.padEnd(20)} median=${(clickMed * 1000).toFixed(1)}ms`);
    results.mouse_click = { medianMs: clickMed * 1000 };

    const typeTimes = await this.benchType(sessionId);
    const typeMed = median(typeTimes);
    console.log(`  ${'Type 11 chars'.padEnd(20)} median=${(typeMed * 1000).toFixed(1)}ms`);
    results.keyboard_type = { medianMs: typeMed * 1000 };

    const lmTimes = await this.benchLayoutMetrics(sessionId);
    const lmMed = median(lmTimes);
    console.log(`  ${'Layout Metrics'.padEnd(20)} median=${(lmMed * 1000).toFixed(1)}ms`);
    results.layout_metrics = { medianMs: lmMed * 1000 };

    // Memory after
    const memAfter = await this.getMemInfo();
    console.log(`\n--- Memory ---`);
    console.log(`  Before: ${memBefore.usedMB.toFixed(0)} MB used`);
    console.log(`  After:  ${memAfter.usedMB.toFixed(0)} MB used`);
    console.log(`  Delta:  +${(memAfter.usedMB - memBefore.usedMB).toFixed(0)} MB`);
    results.memAfter = memAfter;

    await this.send('Target.closeTarget', { targetId });
    this.ws.close();
    return results;
  }
}

function median(arr) {
  const sorted = [...arr].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}
function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

async function main() {
  const instances = [
    { label: 'BASELINE (headless, no live view)', wsPort: 9232, apiPort: 10002 },
    { label: 'CDP LIVE VIEW (headless + screencast)', wsPort: 9222, apiPort: 10001 },
  ];

  const allResults = {};

  for (const inst of instances) {
    const bench = new CDPBench(inst.wsPort, inst.apiPort, inst.label);
    try {
      allResults[inst.label] = await bench.run();
    } catch (e) {
      console.log(`\n  ERROR: ${e.message}\n`);
    }
  }

  const labels = Object.keys(allResults);
  if (labels.length < 2) {
    console.log('Not enough results to compare.');
    return;
  }

  console.log(`\n${'='.repeat(80)}`);
  console.log('  COMPARISON: Baseline vs CDP Live View');
  console.log(`${'='.repeat(80)}`);

  const base = allResults[labels[0]];
  const live = allResults[labels[1]];

  const fmt = (val, unit) => unit === 's' ? `${val.toFixed(3)}s` : `${val.toFixed(1)}ms`;
  const pct = (b, l) => {
    const diff = ((l - b) / b) * 100;
    return diff > 0 ? `+${diff.toFixed(0)}%` : `${diff.toFixed(0)}%`;
  };

  console.log(`\n${'Metric'.padEnd(25)}${'Baseline'.padStart(15)}${'CDP Live View'.padStart(15)}${'Delta'.padStart(10)}`);
  console.log('-'.repeat(65));

  // Navigation
  for (const [name] of URLS) {
    const key = `nav_${name}`;
    if (base[key] && live[key]) {
      const b = base[key].median, l = live[key].median;
      console.log(`Nav ${name}`.padEnd(25) + fmt(b, 's').padStart(15) + fmt(l, 's').padStart(15) + pct(b, l).padStart(10));
    }
  }
  console.log();

  // Operations
  for (const [op, key, unit] of [
    ['Screenshot', 'screenshot', 's'],
    ['JS Evaluate', 'js_evaluate', 'ms'],
    ['Mouse Click', 'mouse_click', 'ms'],
    ['Type 11 chars', 'keyboard_type', 'ms'],
    ['Layout Metrics', 'layout_metrics', 'ms'],
  ]) {
    if (base[key] && live[key]) {
      const b = unit === 's' ? base[key].median : base[key].medianMs;
      const l = unit === 's' ? live[key].median : live[key].medianMs;
      console.log(op.padEnd(25) + fmt(b, unit).padStart(15) + fmt(l, unit).padStart(15) + pct(b, l).padStart(10));
    }
  }

  // Screenshot size
  if (base.screenshot && live.screenshot) {
    console.log(`Screenshot size`.padEnd(25) +
      `${base.screenshot.avgSizeKB.toFixed(0)}KB`.padStart(15) +
      `${live.screenshot.avgSizeKB.toFixed(0)}KB`.padStart(15));
  }
  console.log();

  // Memory
  console.log(`Memory (idle)`.padEnd(25) +
    `${base.memBefore.usedMB.toFixed(0)}MB`.padStart(15) +
    `${live.memBefore.usedMB.toFixed(0)}MB`.padStart(15) +
    `+${(live.memBefore.usedMB - base.memBefore.usedMB).toFixed(0)}MB`.padStart(10));
  console.log(`Memory (after bench)`.padEnd(25) +
    `${base.memAfter.usedMB.toFixed(0)}MB`.padStart(15) +
    `${live.memAfter.usedMB.toFixed(0)}MB`.padStart(15) +
    `+${(live.memAfter.usedMB - base.memAfter.usedMB).toFixed(0)}MB`.padStart(10));
}

main().catch(console.error);
