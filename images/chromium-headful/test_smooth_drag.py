#!/usr/bin/env python3
"""
Demo: smooth Bezier drag_mouse vs linear drag_mouse.
Uploads a custom HTML page with a glowing cursor trail,
navigates the browser to it, records smooth then linear drags.
"""

import json
import time
import urllib.request
import urllib.parse
import os

BASE_URL = os.environ.get("BASE_URL", "https://mammary-unhorizontal-beula.ngrok-free.dev")
REC_ID = f"smooth-drag-demo-{int(time.time())}"
OUTPUT_FILE = "smooth_drag_demo.mp4"
HTML_PATH = "/tmp/cursor_demo.html"
HEADERS = {"Content-Type": "application/json", "ngrok-skip-browser-warning": "1"}

BROWSER_CHROME_Y = 80

# Drag path through the target circles
DRAG_PATH = [
    [240,  240 + BROWSER_CHROME_Y],
    [940,  190 + BROWSER_CHROME_Y],
    [1690, 260 + BROWSER_CHROME_Y],
    [1740, 740 + BROWSER_CHROME_Y],
    [1000, 840 + BROWSER_CHROME_Y],
    [220,  790 + BROWSER_CHROME_Y],
    [540,  520 + BROWSER_CHROME_Y],
    [1440, 520 + BROWSER_CHROME_Y],
]

TARGET_IDS = ["t0", "t1", "t2", "t3", "t4", "t5", "t6", "t7"]


def post(path, body=None):
    data = json.dumps(body or {}).encode()
    req = urllib.request.Request(
        f"{BASE_URL}{path}", data=data, headers=HEADERS, method="POST"
    )
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def get(path):
    req = urllib.request.Request(
        f"{BASE_URL}{path}", headers={"ngrok-skip-browser-warning": "1"}, method="GET"
    )
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def write_file(remote_path, local_path):
    with open(local_path, "rb") as f:
        data = f.read()
    encoded = urllib.parse.quote(remote_path)
    req = urllib.request.Request(
        f"{BASE_URL}/fs/write_file?path={encoded}",
        data=data,
        headers={"Content-Type": "application/octet-stream", "ngrok-skip-browser-warning": "1"},
        method="PUT",
    )
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def drag(path, smooth, duration_ms=None, show=True):
    body = {"path": path, "smooth": smooth}
    if duration_ms is not None:
        body["duration_ms"] = duration_ms
    if show:
        cmd_text = f"POST /computer/drag_mouse\n{json.dumps(body, indent=2)}"
        run_js("kernel-cmd", cmd_text)
        time.sleep(0.3)
    status, resp = post("/computer/drag_mouse", body)
    if status != 200:
        print(f"  WARNING: drag_mouse returned HTTP {status}: {resp.decode()}")


def move(x, y, smooth=False):
    post("/computer/move_mouse", {"x": x, "y": y, "smooth": smooth})


def navigate_to(url):
    status, body = post("/playwright/execute", {"code": f"await page.goto('{url}', {{waitUntil: 'load'}});"})
    if status != 200:
        print(f"  WARNING: navigate HTTP {status}: {body.decode()}")
    time.sleep(1.0)


def run_js(event, detail):
    detail_json = json.dumps(detail)
    code = f"await page.evaluate(() => {{ document.dispatchEvent(new CustomEvent('{event}', {{detail: {detail_json}}})); }});"
    status, body = post("/playwright/execute", {"code": code})
    if status != 200:
        print(f"  WARNING: playwright/execute HTTP {status}: {body.decode()}")


# ── Upload HTML page ─────────────────────────────────────────────────────────
html_file = os.path.join(os.path.dirname(__file__), "cursor_demo.html")
print(f"Uploading {html_file} -> {HTML_PATH} ...")
status, body = write_file(HTML_PATH, html_file)
print(f"  HTTP {status}: {body.decode() or '(empty)'}")
if status not in (200, 201):
    print("Failed to upload HTML, aborting.")
    exit(1)

# ── Navigate to demo page ────────────────────────────────────────────────────
print("Navigating to demo page...")
navigate_to(f"file://{HTML_PATH}")

# ── Start recording ──────────────────────────────────────────────────────────
print("Starting recording...")
status, body = post("/recording/start", {"id": REC_ID, "framerate": 30})
print(f"  HTTP {status}: {body.decode() or '(empty)'}")
if status not in (200, 201):
    print("Failed to start recording, aborting.")
    exit(1)

time.sleep(0.5)

# ── Part 1: smooth Bezier drag ───────────────────────────────────────────────
print("\n[SMOOTH] Bézier curve drag:")
run_js("kernel-mode", "smooth")
move(DRAG_PATH[0][0], DRAG_PATH[0][1], smooth=False)
time.sleep(0.5)

print(f"  Dragging through {len(DRAG_PATH)} waypoints (smooth=true, duration_ms=5000)")
drag(DRAG_PATH, smooth=True, duration_ms=5000)

for tid in TARGET_IDS:
    run_js("kernel-hit", {"id": tid, "mode": "smooth"})

time.sleep(1.5)

# ── Part 2: linear drag ─────────────────────────────────────────────────────
print("\n[LINEAR] Linear interpolation drag:")
run_js("kernel-mode", "instant")
move(DRAG_PATH[0][0], DRAG_PATH[0][1], smooth=False)
time.sleep(0.5)

print(f"  Dragging through {len(DRAG_PATH)} waypoints (smooth=false, steps_per_segment=20, step_delay_ms=30)")
body = {
    "path": DRAG_PATH,
    "smooth": False,
    "steps_per_segment": 20,
    "step_delay_ms": 30,
}
cmd_text = f"POST /computer/drag_mouse\n{json.dumps(body, indent=2)}"
run_js("kernel-cmd", cmd_text)
time.sleep(0.3)
status, resp = post("/computer/drag_mouse", body)
if status != 200:
    print(f"  WARNING: drag_mouse returned HTTP {status}: {resp.decode()}")

for tid in TARGET_IDS:
    run_js("kernel-hit", {"id": tid, "mode": "instant"})

time.sleep(1.0)

# ── Stop recording ───────────────────────────────────────────────────────────
print("\nStopping recording...")
status, _ = post("/recording/stop", {"id": REC_ID})
print(f"  HTTP {status}")

# ── Download ─────────────────────────────────────────────────────────────────
print(f"\nDownloading {OUTPUT_FILE}...")
for attempt in range(15):
    status, data = get(f"/recording/download?id={REC_ID}")
    if status == 202:
        print(f"  Still finalizing, retrying in 2s... (attempt {attempt + 1})")
        time.sleep(2)
        continue
    if status == 200:
        with open(OUTPUT_FILE, "wb") as f:
            f.write(data)
        size_kb = os.path.getsize(OUTPUT_FILE) / 1024
        print(f"  Saved {OUTPUT_FILE} ({size_kb:.0f} KB)")
        break
    else:
        print(f"  HTTP {status}: {data.decode()}")
        break

print("\nDone.")
