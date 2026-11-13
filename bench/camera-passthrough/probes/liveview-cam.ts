// Join live view (neko) as a viewer and send a camera track upstream.
// The "camera" is a canvas stamped with the send time (same encoding as
// bridge/send.html) so latency.ts can measure glass-to-getUserMedia latency.
// usage: bun liveview-cam.ts <neko-url> <seconds-to-keep-open>
import { chromium } from "playwright-core";

const url = process.argv[2] ?? "http://127.0.0.1:8080/?usr=viewer&pwd=admin";
const hold = Number(process.argv[3] ?? 60);
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM ?? "/usr/bin/chromium", headless: true, args: ["--no-sandbox", "--autoplay-policy=no-user-gesture-required"] });
const page = await browser.newPage();
page.on("console", (m) => /negotiation|error|answer/i.test(m.text()) && console.log("viewer:", m.text().slice(0, 160)));
await page.goto(url);
await page.waitForFunction(() => {
  const c = (document.querySelector("#neko") as any)?.__vue__?.$client;
  return c && c._peer && ["connected", "completed"].includes(c._peer.iceConnectionState);
}, null, { timeout: 60000 });
console.log("live view connected");

const info = await page.evaluate(async () => {
  const BITS = 44, B = 24;
  const c = document.createElement("canvas");
  c.width = 1280; c.height = 720;
  const g = c.getContext("2d")!;
  const draw = () => {
    const t = Date.now();
    g.fillStyle = "#406080"; g.fillRect(0, 0, c.width, c.height);
    const a = (t / 1000) % (2 * Math.PI);
    g.fillStyle = "#e0c040"; g.beginPath(); g.arc(640 + 250 * Math.cos(a), 400 + 200 * Math.sin(a), 80, 0, 7); g.fill();
    for (let i = 0; i < BITS; i++) {
      const bit = Math.floor(t / 2 ** i) % 2;
      g.fillStyle = bit ? "#fff" : "#000"; g.fillRect(i * B, 0, B, B);
      g.fillStyle = bit ? "#000" : "#fff"; g.fillRect(i * B, B, B, B);
    }
  };
  setInterval(draw, 1000 / 60);
  const track = c.captureStream(30).getVideoTracks()[0];
  const client = (document.querySelector("#neko") as any).__vue__.$client;
  client._peer.addTrack(track); // fires onnegotiationneeded -> signal/offer
  await new Promise((r) => setTimeout(r, 8000));
  const st = await client._peer.getStats();
  const out: any[] = [];
  st.forEach((s: any) => s.type === "outbound-rtp" && s.kind === "video" && out.push({ frames: s.framesEncoded, w: s.frameWidth, h: s.frameHeight, fps: s.framesPerSecond, codec: st.get(s.codecId)?.mimeType }));
  return out;
});
console.log("upstream:", JSON.stringify(info));
await new Promise((r) => setTimeout(r, hold * 1000));
await browser.close();
