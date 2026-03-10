#!/usr/bin/env node
/**
 * CDP benchmark: measures latency of common browser operations.
 * Uses Node.js ws library (same as Playwright/Puppeteer).
 */

import { WebSocket } from 'ws';
import https from 'https';

const ITERATIONS = 3;
const URLS = [
  ['Wikipedia', 'https://en.wikipedia.org/wiki/Main_Page'],
  ['Apple', 'https://www.apple.com'],
  ['GitHub', 'https://github.com'],
  ['CNN', 'https://www.cnn.com'],
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
    // Check buffered events first
    const idx = this.events.findIndex(e =>
      e.method === name && (!sessionId || e.sessionId === sessionId));
    if (idx >= 0) {
      return Promise.resolve(this.events.splice(idx, 1)[0]);
    }
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
    await this.send('Page.enable', null, sessionId);
    const times = [];
    for (let i = 0; i < ITERATIONS; i++) {
      this.events = [];
      const start = performance.now();
      await this.send('Page.navigate', { url }, sessionId);
      try {
        await this.waitForEvent('Page.loadEventFired', sessionId, 30000);
      } catch { /* timeout is ok, still measure */ }
      times.push((performance.now() - start) / 1000);
      await sleep(500);
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
      await sleep(200);
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

  async getMemory() {
    try {
      const resp = await this.send('SystemInfo.getProcessInfo');
      const procs = resp.result?.processInfo || [];
      const totalMem = procs.reduce((s, p) => s + (p.privateMemory || 0), 0);
      const totalCpu = procs.reduce((s, p) => s + (p.cpuTime || 0), 0);
      return { memMB: totalMem / 1024 / 1024, cpuTime: totalCpu, nProcs: procs.length };
    } catch {
      return { memMB: 0, cpuTime: 0, nProcs: 0 };
    }
  }

  async run() {
    console.log(`\n${'='.repeat(60)}`);
    console.log(`  BENCHMARK: ${this.label}`);
    console.log(`  Host: ${this.host}`);
    console.log(`  Iterations per test: ${ITERATIONS}`);
    console.log(`${'='.repeat(60)}\n`);

    await this.connect();
    const { targetId, sessionId } = await this.createTarget();
    await this.send('Page.enable', null, sessionId);
    await this.send('Runtime.enable', null, sessionId);

    const results = {};

    // Navigation benchmarks
    console.log('--- Navigation Latency ---');
    for (const [name, url] of URLS) {
      const times = await this.benchNavigate(sessionId, name, url);
      const med = median(times);
      console.log(`  ${name.padEnd(20)} median=${med.toFixed(3)}s  min=${Math.min(...times).toFixed(3)}s  max=${Math.max(...times).toFixed(3)}s`);
      results[`nav_${name}`] = { median: med, min: Math.min(...times), max: Math.max(...times), raw: times };
    }

    // Navigate to Wikipedia for remaining tests
    await this.send('Page.navigate', { url: 'https://en.wikipedia.org/wiki/Main_Page' }, sessionId);
    await sleep(3000);
    this.events = [];

    console.log('\n--- CDP Operation Latency ---');

    // Screenshot
    const ss = await this.benchScreenshot(sessionId);
    const ssMed = median(ss.times);
    const avgSize = ss.sizes.reduce((a, b) => a + b, 0) / ss.sizes.length;
    console.log(`  ${'Screenshot'.padEnd(20)} median=${ssMed.toFixed(3)}s  size=${(avgSize/1024).toFixed(0)}KB`);
    results.screenshot = { median: ssMed, raw: ss.times, avgSizeKB: avgSize / 1024 };

    // JS Evaluate
    const evalTimes = await this.benchEvaluate(sessionId);
    const evalMed = median(evalTimes);
    console.log(`  ${'JS Evaluate'.padEnd(20)} median=${(evalMed * 1000).toFixed(1)}ms`);
    results.js_evaluate = { medianMs: evalMed * 1000, raw: evalTimes };

    // Mouse Click
    const clickTimes = await this.benchClick(sessionId);
    const clickMed = median(clickTimes);
    console.log(`  ${'Mouse Click'.padEnd(20)} median=${(clickMed * 1000).toFixed(1)}ms`);
    results.mouse_click = { medianMs: clickMed * 1000, raw: clickTimes };

    // Keyboard Type
    const typeTimes = await this.benchType(sessionId);
    const typeMed = median(typeTimes);
    console.log(`  ${'Type 11 chars'.padEnd(20)} median=${(typeMed * 1000).toFixed(1)}ms`);
    results.keyboard_type = { medianMs: typeMed * 1000, raw: typeTimes };

    // Layout Metrics
    const lmTimes = await this.benchLayoutMetrics(sessionId);
    const lmMed = median(lmTimes);
    console.log(`  ${'Layout Metrics'.padEnd(20)} median=${(lmMed * 1000).toFixed(1)}ms`);
    results.layout_metrics = { medianMs: lmMed * 1000, raw: lmTimes };

    // Memory
    console.log('\n--- Resource Usage ---');
    const mem = await this.getMemory();
    console.log(`  Browser processes: ${mem.nProcs}`);
    console.log(`  Private memory:    ${mem.memMB.toFixed(0)} MB`);
    console.log(`  CPU time:          ${mem.cpuTime.toFixed(1)}s`);
    results.memoryMB = mem.memMB;
    results.cpuTimeS = mem.cpuTime;
    results.processCount = mem.nProcs;

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

function sleep(ms) {
  return new Promise(r => setTimeout(r, ms));
}

async function warmup(host, label) {
  for (let i = 0; i < 30; i++) {
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

async function main() {
  const instances = [
    ['BASELINE (v29 headless, 1vCPU/3GB)', 'winter-mountain-2k9xdihk.dev-iad-unikraft-3.onkernel.app'],
    ['NEW (headless+live view, 1vCPU/3GB)', 'silent-thunder-42i78h9l.dev-iad-unikraft-3.onkernel.app'],
    ['HEADFUL (kernel-cu-v33, 4vCPU/4GB)', 'misty-cherry-74utb712.dev-iad-unikraft-3.onkernel.app'],
  ];

  console.log('Warming up instances...');
  for (const [label, host] of instances) {
    await warmup(host, label);
  }

  const allResults = {};
  const labels = [];

  for (const [label, host] of instances) {
    const bench = new CDPBench(host, label);
    try {
      allResults[label] = await bench.run();
      labels.push(label);
    } catch (e) {
      console.log(`\n  ERROR benchmarking ${label}: ${e.message}\n`);
    }
  }

  if (labels.length < 2) {
    console.log('Not enough successful benchmarks to compare.');
    return;
  }

  // Summary
  console.log(`\n${'='.repeat(100)}`);
  console.log('  COMPARISON SUMMARY');
  console.log(`${'='.repeat(100)}`);

  const shortLabels = labels.map(l => {
    if (l.includes('BASELINE')) return 'Baseline';
    if (l.includes('NEW')) return 'New+LiveView';
    return 'Headful';
  });

  let header = 'Operation'.padEnd(25);
  for (const sl of shortLabels) header += sl.padStart(15);
  console.log(`\n${header}`);
  console.log('-'.repeat(25) + (' ' + '-'.repeat(14)).repeat(labels.length));

  for (const [name] of URLS) {
    const key = `nav_${name}`;
    let row = `Nav ${name}`.padEnd(25);
    for (const l of labels) {
      row += `${allResults[l][key].median.toFixed(3)}s`.padStart(15);
    }
    console.log(row);
  }
  console.log();

  for (const [op, key, unit] of [
    ['Screenshot', 'screenshot', 's'],
    ['JS Evaluate', 'js_evaluate', 'ms'],
    ['Mouse Click', 'mouse_click', 'ms'],
    ['Type 11 chars', 'keyboard_type', 'ms'],
    ['Layout Metrics', 'layout_metrics', 'ms'],
  ]) {
    let row = op.padEnd(25);
    for (const l of labels) {
      if (unit === 's') {
        row += `${allResults[l][key].median.toFixed(3)}s`.padStart(15);
      } else {
        row += `${allResults[l][key].medianMs.toFixed(1)}ms`.padStart(15);
      }
    }
    console.log(row);
  }
  console.log();

  let memRow = 'Memory'.padEnd(25);
  let cpuRow = 'CPU time'.padEnd(25);
  let procRow = 'Processes'.padEnd(25);
  for (const l of labels) {
    memRow += `${allResults[l].memoryMB.toFixed(0)}MB`.padStart(15);
    cpuRow += `${allResults[l].cpuTimeS.toFixed(1)}s`.padStart(15);
    procRow += `${allResults[l].processCount}`.padStart(15);
  }
  console.log(memRow);
  console.log(cpuRow);
  console.log(procRow);
}

main().catch(console.error);
