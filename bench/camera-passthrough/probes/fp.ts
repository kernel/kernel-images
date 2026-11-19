// Dump what a page can observe about the camera.
// usage: bun fp.ts <cdp-url>
import { chromium } from "playwright-core";

const browser = await chromium.connectOverCDP(process.argv[2] ?? "http://127.0.0.1:9222");
const ctx = browser.contexts()[0];
await ctx.grantPermissions(["camera"], { origin: "https://webrtc.github.io" });
const page = await ctx.newPage();
await page.goto("https://webrtc.github.io/samples/src/content/getusermedia/gum/");
const out = await page.evaluate(async () => {
  const strip = (o: any) => { const { deviceId, groupId, ...rest } = o; return rest; };
  const r: any = {};
  r.supportedConstraints = Object.keys(navigator.mediaDevices.getSupportedConstraints()).length;
  const s = await navigator.mediaDevices.getUserMedia({ video: true });
  const t = s.getVideoTracks()[0];
  r.devices = (await navigator.mediaDevices.enumerateDevices()).map((d) => `${d.kind}:${d.label}`);
  r.label = t.label;
  r.contentHint = t.contentHint;
  r.settings = strip(t.getSettings());
  r.capabilities = strip(t.getCapabilities());
  try {
    const ic = new (window as any).ImageCapture(t);
    r.photoCapabilities = await ic.getPhotoCapabilities();
    r.photoSettings = await ic.getPhotoSettings();
  } catch (e) { r.imageCapture = String(e); }
  t.stop();
  r.resolutions = {};
  for (const [w, h] of [[320, 240], [640, 480], [1280, 720], [1920, 1080]]) {
    for (const mode of ["exact", "ideal"]) {
      try {
        const c = mode === "exact" ? { width: { exact: w }, height: { exact: h } } : { width: w, height: h };
        const st = await navigator.mediaDevices.getUserMedia({ video: c });
        const se = st.getVideoTracks()[0].getSettings();
        r.resolutions[`${mode} ${w}x${h}`] = `${se.width}x${se.height} resize=${se.resizeMode}`;
        st.getTracks().forEach((x) => x.stop());
      } catch (e: any) { r.resolutions[`${mode} ${w}x${h}`] = e.name; }
    }
  }
  try {
    const st = await navigator.mediaDevices.getUserMedia({ video: { resizeMode: "none", width: 640 } });
    const se = st.getVideoTracks()[0].getSettings();
    r.nativeOnly640 = `${se.width}x${se.height}`;
    st.getTracks().forEach((x) => x.stop());
  } catch (e: any) { r.nativeOnly640 = e.name; }
  return r;
});
console.log(JSON.stringify(out, null, 1));
await page.close();
await browser.close();
