// Measure glass-to-getUserMedia latency for frames stamped by bridge/send.html.
// usage: bun latency.ts <cdp-url> [seconds]
import { chromium } from "playwright-core";

const cdp = process.argv[2] ?? "http://127.0.0.1:9222";
const seconds = Number(process.argv[3] ?? 20);
const browser = await chromium.connectOverCDP(cdp);
const ctx = browser.contexts()[0];
await ctx.grantPermissions(["camera"], { origin: "https://webrtc.github.io" });
const page = await ctx.newPage();
await page.goto("https://webrtc.github.io/samples/src/content/getusermedia/gum/");

const r = await page.evaluate(async (seconds) => {
  const BITS = 44, B = 24;
  const stream = await navigator.mediaDevices.getUserMedia({ video: { width: 1280, height: 720 } });
  const v = document.createElement("video");
  v.muted = true;
  v.srcObject = stream;
  await v.play();
  const c = document.createElement("canvas");
  c.width = BITS * B;
  c.height = 2 * B;
  const g = c.getContext("2d", { willReadFrequently: true })!;
  const lat: number[] = [];
  let bad = 0, frames = 0;
  const t0 = performance.now();
  await new Promise<void>((done) => {
    const cb = () => {
      frames++;
      g.drawImage(v, 0, 0, c.width, c.height, 0, 0, c.width, c.height);
      const px = g.getImageData(0, 0, c.width, c.height).data;
      const lum = (x: number, y: number) => px[(y * c.width + x) * 4 + 1];
      let t = 0, ok = true;
      for (let i = 0; i < BITS; i++) {
        const a = lum(i * B + B / 2, B / 2) > 128, b = lum(i * B + B / 2, B + B / 2) > 128;
        if (a === b) ok = false;
        if (a) t += 2 ** i;
      }
      if (ok) lat.push(Date.now() - t);
      else bad++;
      if (performance.now() - t0 < seconds * 1000) v.requestVideoFrameCallback(cb);
      else done();
    };
    v.requestVideoFrameCallback(cb);
  });
  stream.getTracks().forEach((t) => t.stop());
  lat.sort((a, b) => a - b);
  const q = (p: number) => lat[Math.min(lat.length - 1, Math.floor(p * lat.length))];
  return { frames, fps: +(frames / seconds).toFixed(1), decoded: lat.length, bad, min: lat[0], p50: q(0.5), p90: q(0.9), p99: q(0.99), max: lat[lat.length - 1] };
}, seconds);
console.log(JSON.stringify(r));
await page.close();
await browser.close();
