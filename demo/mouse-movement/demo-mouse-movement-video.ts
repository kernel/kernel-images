/**
 * Before/After mouse movement demo for video recording.
 *
 * Demonstrates:
 * - BEFORE: Instant mouse movement (smooth: false) — cursor teleports in straight lines
 * - AFTER: Human-like Bezier curve movement (smooth: true) — natural curved trajectory
 *
 * Inspired by https://camoufox.com/stealth/ and https://camoufox.com/fingerprint/cursor-movement
 *
 * Usage:
 *   1. Ensure KERNEL_BROWSER_ID and KERNEL_API_KEY are set
 *   2. Start screen recording (OBS, QuickTime, ffmpeg) on the browser live view
 *   3. Run: npm run demo
 *   4. Record: BEFORE (instant) then AFTER (smooth Bezier) segments
 */

import Kernel from "@onkernel/sdk";
import { chromium } from "playwright-core";
import { readFileSync } from "fs";
import { join, dirname } from "path";
import { fileURLToPath } from "url";

const BROWSER_ID = process.env.KERNEL_BROWSER_ID!;

function sleep(ms: number) {
  return new Promise((r) => setTimeout(r, ms));
}

const __dirname = dirname(fileURLToPath(import.meta.url));

// Movement path chosen to clearly show the difference: diagonal + arc
const DEMO_PATH: [number, number][] = [
  [200, 200],
  [600, 350],
  [1000, 200],
  [700, 500],
  [400, 400],
  [800, 300],
];

(async () => {
  if (!BROWSER_ID) {
    console.error("Set KERNEL_BROWSER_ID");
    process.exit(1);
  }

  const kernel = new Kernel();
  const session = await kernel.browsers.retrieve(BROWSER_ID);

  console.log("Session:", BROWSER_ID);
  console.log("Live view (record this):", session.browser_live_view_url);

  const browser = await chromium.connectOverCDP(session.cdp_ws_url);
  const page = browser.contexts()[0].pages()[0] ?? (await browser.newPage());

  // Load cursor trail demo page
  const demoHtml = readFileSync(
    join(__dirname, "cursor-trail-demo.html"),
    "utf-8"
  );
  await page.setContent(demoHtml, { waitUntil: "domcontentloaded" });
  await page.setViewportSize({ width: 1280, height: 720 });

  await sleep(500);

  // --- BEFORE: Instant movement (smooth: false) ---
  await page.evaluate(() => {
    (window as any).demoApi?.setMode("BEFORE: Instant movement (smooth: false)", "instant");
    (window as any).demoApi?.clear();
  });
  await sleep(800);

  console.log("[BEFORE] Running instant mouse moves...");
  for (let i = 0; i < DEMO_PATH.length; i++) {
    const [x, y] = DEMO_PATH[i];
    await kernel.browsers.computer.moveMouse(BROWSER_ID, { x, y, smooth: false });
    await sleep(400);
  }
  await sleep(2000);

  // --- Clear and switch to AFTER ---
  await page.evaluate(() => {
    (window as any).demoApi?.setMode("AFTER: Human-like Bezier movement (smooth: true)", "smooth");
    (window as any).demoApi?.clear();
  });
  await sleep(1500);

  // --- AFTER: Smooth Bezier movement (smooth: true) ---
  console.log("[AFTER] Running smooth Bezier mouse moves...");
  for (let i = 0; i < DEMO_PATH.length; i++) {
    const [x, y] = DEMO_PATH[i];
    await kernel.browsers.computer.moveMouse(BROWSER_ID, {
      x,
      y,
      smooth: true,
      step_delay_ms: 12,
    });
    await sleep(400);
  }
  await sleep(3000);

  console.log("Demo complete. Stop recording.");
  await browser.close();
})();
