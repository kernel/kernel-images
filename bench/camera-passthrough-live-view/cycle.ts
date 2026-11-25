// Open/stop the camera repeatedly and report per-attempt results.
// usage: bun cycle.ts <cdp-url> [rounds]
import { chromium } from "playwright-core";

const browser = await chromium.connectOverCDP(process.argv[2] ?? "http://127.0.0.1:9222");
const ctx = browser.contexts()[0];
await ctx.grantPermissions(["camera"], { origin: "https://webrtc.github.io" });
const page = await ctx.newPage();
await page.goto("https://webrtc.github.io/samples/src/content/getusermedia/gum/");
const rounds = parseInt(process.argv[3] ?? "6", 10);
for (let i = 0; i < rounds; i++) {
  const constraint = i % 2 === 0 ? true : { width: { exact: 1280 }, height: { exact: 720 } };
  const out = await page.evaluate(async (c: any) => {
    try {
      const s = await navigator.mediaDevices.getUserMedia({ video: c });
      const t = s.getVideoTracks()[0];
      await new Promise((r) => setTimeout(r, 400));
      const se = t.getSettings();
      // measure inter-frame timing over ~1s via rVFC
      const video = document.createElement("video");
      video.srcObject = s;
      await video.play();
      const deltas: number[] = [];
      let last = 0;
      await new Promise<void>((resolve) => {
        const cb = (now: number, meta: any) => {
          if (last) deltas.push(meta.mediaTime ? 0 : now - last);
          last = now;
          if (deltas.length >= 25) resolve();
          else video.requestVideoFrameCallback(cb);
        };
        video.requestVideoFrameCallback(cb);
      });
      t.stop();
      video.remove();
      const d = deltas.slice(1);
      const avg = d.reduce((a, b) => a + b, 0) / d.length;
      const sd = Math.sqrt(d.reduce((a, b) => a + (b - avg) ** 2, 0) / d.length);
      return `${se.width}x${se.height}@${se.frameRate} rVFC avg=${avg.toFixed(1)}ms sd=${sd.toFixed(2)}ms n=${d.length}`;
    } catch (e: any) {
      return `FAIL ${e.name}: ${e.message}`;
    }
  }, constraint);
  console.log(`round ${i} (${constraint}): ${out}`);
}
await page.close();
await browser.close();
