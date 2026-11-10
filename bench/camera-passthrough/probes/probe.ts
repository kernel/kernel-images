// Probe a Chromium (local launch or remote CDP) for a live camera on the gUM sample page.
// usage: bun probe.ts [cdp-ws-or-http-url] [screenshot-path]
import { chromium } from "playwright-core";

const cdp = process.argv[2];
const shot = process.argv[3] ?? "/tmp/gum.png";
const url = "https://webrtc.github.io/samples/src/content/getusermedia/gum/";

const browser = cdp
  ? await chromium.connectOverCDP(cdp)
  : await chromium.launch({ executablePath: process.env.CHROMIUM ?? "/usr/bin/chromium", headless: true, args: ["--no-sandbox"] });
const ctx = browser.contexts()[0] ?? (await browser.newContext());

// Playwright maps this to CDP Browser.grantPermissions(["videoCapture"]) for the context.
await ctx.grantPermissions(["camera"], { origin: "https://webrtc.github.io" });

const page = await ctx.newPage();
await page.goto(url, { waitUntil: "load" });
const before = await page.evaluate(async () =>
  (await navigator.mediaDevices.enumerateDevices()).map((d) => ({ kind: d.kind, label: d.label, id: d.deviceId.slice(0, 8) })),
);
await page.click("#showVideo");
await page.waitForFunction(() => (document.querySelector("video") as HTMLVideoElement)?.videoWidth > 0, null, { timeout: 15000 });

const result = await page.evaluate(async () => {
  const v = document.querySelector("video") as HTMLVideoElement;
  const track = (v.srcObject as MediaStream).getVideoTracks()[0];
  let frames = 0;
  const t0 = performance.now();
  await new Promise<void>((res) => {
    const cb = () => {
      frames++;
      if (performance.now() - t0 < 2000) v.requestVideoFrameCallback(cb);
      else res();
    };
    v.requestVideoFrameCallback(cb);
  });
  return {
    label: track.label,
    fps_observed: +(frames / ((performance.now() - t0) / 1000)).toFixed(1),
    settings: track.getSettings(),
    capabilities: track.getCapabilities?.(),
    devices: (await navigator.mediaDevices.enumerateDevices()).map((d) => ({ kind: d.kind, label: d.label, groupId: d.groupId.slice(0, 8) })),
    size: [v.videoWidth, v.videoHeight],
  };
});
console.log(JSON.stringify({ devicesBeforeGrant: before, ...result }, null, 2));
await page.screenshot({ path: shot });
await page.close();
await browser.close();
