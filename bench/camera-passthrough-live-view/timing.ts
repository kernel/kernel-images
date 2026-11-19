// Measure frame cadence + flat-region noise of the camera as a page sees it.
// usage: bun timing.ts <cdp-url> [seconds]
import { chromium } from "playwright-core";

const browser = await chromium.connectOverCDP(process.argv[2] ?? "http://127.0.0.1:9222");
const ctx = browser.contexts()[0];
await ctx.grantPermissions(["camera"], { origin: "https://webrtc.github.io" });
const page = await ctx.newPage();
await page.goto("https://webrtc.github.io/samples/src/content/getusermedia/gum/");
const secs = parseInt(process.argv[3] ?? "4", 10);
const out = await page.evaluate(async (secs) => {
  const s = await navigator.mediaDevices.getUserMedia({ video: true });
  const t = s.getVideoTracks()[0];
  const video = document.createElement("video");
  video.muted = true;
  video.srcObject = s;
  await video.play();
  await new Promise((r) => setTimeout(r, 1500)); // let the mode negotiation settle

  // cadence via rVFC
  const stamps: number[] = [];
  const mediaTimes: number[] = [];
  const deadline = performance.now() + secs * 1000;
  await new Promise<void>((resolve) => {
    const cb = (now: number, meta: any) => {
      stamps.push(now);
      mediaTimes.push(meta.mediaTime);
      if (performance.now() < deadline) video.requestVideoFrameCallback(cb);
      else resolve();
    };
    video.requestVideoFrameCallback(cb);
  });

  const pct = (a: number[], p: number) => a.slice().sort((x, y) => x - y)[Math.floor(a.length * p)];
  const deltas = stamps.slice(1).map((v, i) => v - stamps[i]);
  const mtd = mediaTimes.slice(1).map((v, i) => (v - mediaTimes[i]) * 1000);
  const mtMean = mtd.reduce((a, b) => a + b, 0) / mtd.length;
  const mtSd = Math.sqrt(mtd.reduce((a, b) => a + (b - mtMean) ** 2, 0) / mtd.length);
  const mean = deltas.reduce((a, b) => a + b, 0) / deltas.length;
  const sd = Math.sqrt(deltas.reduce((a, b) => a + (b - mean) ** 2, 0) / deltas.length);

  // noise: draw frames to canvas, measure per-pixel temporal stddev over 8
  // frames in a flat region, and check no two frames are identical
  const c = document.createElement("canvas");
  c.width = video.videoWidth;
  c.height = video.videoHeight;
  const g = c.getContext("2d", { willReadFrequently: true })!;
  const frames: Uint8ClampedArray[] = [];
  for (let i = 0; i < 8; i++) {
    g.drawImage(video, 0, 0);
    frames.push(g.getImageData(0, 0, c.width, c.height).data);
    await new Promise((r) => setTimeout(r, 120));
  }
  let identical = 0;
  for (let i = 1; i < frames.length; i++) {
    if (frames[i].every((v, j) => v === frames[0][j])) identical++;
  }
  // temporal noise: stddev of the center pixel's R channel across frames
  const center = (Math.floor(c.height / 2) * c.width + Math.floor(c.width / 2)) * 4;
  const series = frames.map((f) => f[center]);
  const m = series.reduce((a, b) => a + b, 0) / series.length;
  const tsd = Math.sqrt(series.reduce((a, b) => a + (b - m) ** 2, 0) / series.length);
  // spatial noise floor: stddev across a 32x32 flat patch in one frame
  const patch: number[] = [];
  for (let y = 0; y < 32; y += 4)
    for (let x = 0; x < 32; x += 4) patch.push(frames[0][(y * c.width + x) * 4]);
  const pm = patch.reduce((a, b) => a + b, 0) / patch.length;
  const ssd = Math.sqrt(patch.reduce((a, b) => a + (b - pm) ** 2, 0) / patch.length);

  t.stop();
  video.remove();
  return {
    frames: stamps.length,
    avgIntervalMs: +mean.toFixed(2),
    intervalSdMs: +sd.toFixed(3),
    p05: +pct(deltas, 0.05).toFixed(1),
    p95: +pct(deltas, 0.95).toFixed(1),
    mediaTimeDeltaMs: +mtMean.toFixed(2), mediaTimeSdMs: +mtSd.toFixed(3), mediaTimeP05: +pct(mtd, 0.05).toFixed(2), mediaTimeP95: +pct(mtd, 0.95).toFixed(2),
    identicalFrames: identical,
    temporalNoiseSd: +tsd.toFixed(3),
    spatialNoiseSd: +ssd.toFixed(3),
    meanLuma: +m.toFixed(1),
    settings: t ? undefined : undefined,
  };
}, secs);
console.log(JSON.stringify(out, null, 1));
await page.close();
await browser.close();
