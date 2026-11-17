// NOTE: this harness auto-follows the on-demand signal for automated testing and
// omits the one-time consent gate the product controller (client/src/kernel/camera.ts)
// enforces. camera.ts is authoritative.
// Prototype: camera on demand for live view. Not production code.
// When a page inside the Kernel browser asks for the camera, the viewer's camera turns on
// automatically; when the page stops using it, it turns off. The button is a manual override.
(function () {
  function client() { const el = document.querySelector("#neko"); return el && el.__vue__ && el.__vue__.$client; }
  function connected() { const c = client(); return c && c._peer && ["connected", "completed"].includes(c._peer.iceConnectionState); }
  let stream = null, sender = null, manual = false, starting = false, idleSince = 0;
  const ui = document.createElement("div");
  ui.style.cssText = "position:fixed;left:16px;bottom:16px;z-index:99999;display:flex;flex-direction:column;align-items:flex-start;gap:8px;font:13px system-ui,sans-serif";
  const note = document.createElement("div");
  note.style.cssText = "padding:6px 10px;border-radius:8px;background:rgba(33,34,37,.88);color:#edeef0;display:none;max-width:280px";
  const btn = document.createElement("button");
  btn.style.cssText = "padding:9px 15px;border:0;border-radius:999px;font:600 13px system-ui,sans-serif;box-shadow:0 2px 8px rgba(0,0,0,.35);cursor:pointer";
  ui.append(note, btn);
  const say = (m) => { note.textContent = m || ""; note.style.display = m ? "block" : "none"; };
  function render() {
    if (stream) { btn.textContent = "● Camera on — stop"; btn.style.background = "#e5484d"; btn.style.color = "#fff"; }
    else { btn.textContent = "📷 Camera off"; btn.style.background = "#81b300"; btn.style.color = "#212225"; }
  }
  async function start(reason) {
    if (stream || starting || !connected()) return;
    starting = true;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ video: { width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 30 } }, audio: false });
      sender = client()._peer.addTrack(stream.getVideoTracks()[0], stream);
      say(reason === "auto" ? "The site asked for a camera, so your camera is on." : "");
      if (reason === "auto") setTimeout(() => stream && say(""), 5000);
    } catch (e) {
      stream = null;
      say(e.name === "NotAllowedError" ? "Camera blocked. Allow camera access for this page (icon left of the address bar) and try again." : "Camera error: " + e.name);
    } finally { starting = false; render(); }
  }
  function stop() {
    const c = client();
    try { if (c && c._peer && sender) c._peer.removeTrack(sender); } catch (e) {}
    if (stream) stream.getTracks().forEach((t) => t.stop());
    stream = null; sender = null; render();
  }
  btn.onclick = () => { if (stream) { manual = false; stop(); say(""); } else { manual = true; start("manual"); } };
  async function poll() {
    try {
      const base = location.pathname.replace(/\/?$/, "/");
      const q = location.search ? location.search + "&" : "?";
      let r = await fetch(base + "camera-state.json" + q + "t=" + Date.now(), { cache: "no-store" });
      if (!r.ok) r = await fetch("camera-state.json?t=" + Date.now(), { cache: "no-store" });
      if (r.ok) {
        const s = await r.json();
        if (s.wanted) { idleSince = 0; if (!stream) start("auto"); }
        else if (stream && !manual) {
          idleSince = idleSince || Date.now();
          if (Date.now() - idleSince > 3000) { stop(); say("The site stopped using the camera, so your camera is off."); setTimeout(() => !stream && say(""), 4000); idleSince = 0; }
        }
      }
    } catch (e) {}
    setTimeout(poll, 700);
  }
  function mount() { document.body.appendChild(ui); render(); poll(); }
  document.readyState === "loading" ? document.addEventListener("DOMContentLoaded", mount) : mount();
})();
