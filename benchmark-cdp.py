#!/usr/bin/env python3
"""CDP benchmark: measures latency of common browser operations."""

import asyncio
import json
import ssl
import sys
import time
import statistics
import base64

try:
    import websockets
except ImportError:
    print("Installing websockets...")
    import subprocess
    subprocess.check_call([sys.executable, "-m", "pip", "install", "websockets", "-q"])
    import websockets

URLS = [
    ("Wikipedia", "https://en.wikipedia.org/wiki/Main_Page"),
    ("Apple", "https://www.apple.com"),
    ("GitHub", "https://github.com"),
    ("CNN", "https://www.cnn.com"),
    ("Hacker News", "https://news.ycombinator.com"),
]

ITERATIONS = 3


class CDPBenchmark:
    def __init__(self, host, label):
        self.host = host
        self.label = label
        self.ws = None
        self.msg_id = 0
        self.results = {}

    async def connect(self):
        self.ssl_ctx = ssl.create_default_context()
        self.ssl_ctx.check_hostname = False
        self.ssl_ctx.verify_mode = ssl.CERT_NONE

        # CDP proxy routes all WS to browser-level endpoint regardless of path
        ws_url = f"wss://{self.host}:9222/devtools/browser"
        self.ws = await websockets.connect(ws_url, ssl=self.ssl_ctx, max_size=50 * 1024 * 1024,
                                            additional_headers={"Host": self.host})

    async def send(self, method, params=None):
        self.msg_id += 1
        msg = {"id": self.msg_id, "method": method}
        if params:
            msg["params"] = params
        await self.ws.send(json.dumps(msg))
        while True:
            resp = json.loads(await self.ws.recv())
            if resp.get("id") == self.msg_id:
                return resp
            # skip events

    async def create_target(self):
        resp = await self.send("Target.createTarget", {"url": "about:blank"})
        target_id = resp["result"]["targetId"]
        resp = await self.send("Target.attachToTarget", {"targetId": target_id, "flatten": True})
        session_id = resp["result"]["sessionId"]
        return target_id, session_id

    async def send_session(self, session_id, method, params=None):
        self.msg_id += 1
        msg = {"id": self.msg_id, "method": method, "sessionId": session_id}
        if params:
            msg["params"] = params
        await self.ws.send(json.dumps(msg))
        while True:
            resp = json.loads(await self.ws.recv())
            if resp.get("id") == self.msg_id:
                return resp

    async def bench_navigate(self, session_id, name, url):
        """Navigate and wait for load event."""
        await self.send_session(session_id, "Page.enable")

        times = []
        for i in range(ITERATIONS):
            start = time.perf_counter()
            await self.send_session(session_id, "Page.navigate", {"url": url})
            # Wait for loadEventFired
            deadline = time.perf_counter() + 30
            while time.perf_counter() < deadline:
                resp = json.loads(await self.ws.recv())
                if resp.get("method") == "Page.loadEventFired":
                    break
            elapsed = time.perf_counter() - start
            times.append(elapsed)
            # small pause between iterations
            await asyncio.sleep(0.5)

        return times

    async def bench_screenshot(self, session_id):
        """Take a full-page screenshot."""
        times = []
        sizes = []
        for _ in range(ITERATIONS):
            start = time.perf_counter()
            resp = await self.send_session(session_id, "Page.captureScreenshot",
                                           {"format": "png"})
            elapsed = time.perf_counter() - start
            times.append(elapsed)
            data = resp.get("result", {}).get("data", "")
            sizes.append(len(base64.b64decode(data)) if data else 0)
            await asyncio.sleep(0.2)
        return times, sizes

    async def bench_evaluate(self, session_id):
        """Evaluate JS expression."""
        times = []
        for _ in range(ITERATIONS):
            start = time.perf_counter()
            await self.send_session(session_id, "Runtime.evaluate",
                                    {"expression": "document.title"})
            elapsed = time.perf_counter() - start
            times.append(elapsed)
        return times

    async def bench_click(self, session_id):
        """Dispatch mouse click at (100, 100)."""
        times = []
        for _ in range(ITERATIONS):
            start = time.perf_counter()
            await self.send_session(session_id, "Input.dispatchMouseEvent",
                                    {"type": "mousePressed", "x": 100, "y": 100,
                                     "button": "left", "clickCount": 1})
            await self.send_session(session_id, "Input.dispatchMouseEvent",
                                    {"type": "mouseReleased", "x": 100, "y": 100,
                                     "button": "left", "clickCount": 1})
            elapsed = time.perf_counter() - start
            times.append(elapsed)
        return times

    async def bench_type(self, session_id):
        """Type a string character by character."""
        text = "hello world"
        times = []
        for _ in range(ITERATIONS):
            start = time.perf_counter()
            for ch in text:
                await self.send_session(session_id, "Input.dispatchKeyEvent",
                                        {"type": "keyDown", "text": ch})
                await self.send_session(session_id, "Input.dispatchKeyEvent",
                                        {"type": "keyUp"})
            elapsed = time.perf_counter() - start
            times.append(elapsed)
        return times

    async def bench_get_layout_metrics(self, session_id):
        """Get layout metrics (viewport info)."""
        times = []
        for _ in range(ITERATIONS):
            start = time.perf_counter()
            await self.send_session(session_id, "Page.getLayoutMetrics")
            elapsed = time.perf_counter() - start
            times.append(elapsed)
        return times

    async def get_memory(self):
        """Get browser memory usage via systeminfo."""
        resp = await self.send("SystemInfo.getProcessInfo")
        if "result" in resp:
            procs = resp["result"].get("processInfo", [])
            total_mem = sum(p.get("privateMemory", 0) for p in procs)
            total_cpu = sum(p.get("cpuTime", 0) for p in procs)
            return total_mem, total_cpu, len(procs)
        return 0, 0, 0

    async def get_memory_via_api(self):
        """Get memory via kernel-images API."""
        import urllib.request
        try:
            url = f"https://{self.host}:444/health"
            req = urllib.request.Request(url)
            ctx = ssl.create_default_context()
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
            with urllib.request.urlopen(req, context=ctx, timeout=5) as r:
                return json.loads(r.read())
        except Exception:
            return None

    async def run(self):
        print(f"\n{'='*60}")
        print(f"  BENCHMARK: {self.label}")
        print(f"  Host: {self.host}")
        print(f"  Iterations per test: {ITERATIONS}")
        print(f"{'='*60}\n")

        await self.connect()

        target_id, session_id = await self.create_target()
        await self.send_session(session_id, "Page.enable")
        await self.send_session(session_id, "Runtime.enable")

        all_results = {}

        # Navigation benchmarks
        print("--- Navigation Latency ---")
        for name, url in URLS:
            times = await self.bench_navigate(session_id, name, url)
            med = statistics.median(times)
            mn = min(times)
            mx = max(times)
            print(f"  {name:20s}  median={med:.3f}s  min={mn:.3f}s  max={mx:.3f}s")
            all_results[f"nav_{name}"] = {"median": med, "min": mn, "max": mx, "raw": times}

        # Navigate to Wikipedia for the remaining tests
        await self.send_session(session_id, "Page.navigate",
                                {"url": "https://en.wikipedia.org/wiki/Main_Page"})
        await asyncio.sleep(2)
        # drain events
        try:
            while True:
                await asyncio.wait_for(self.ws.recv(), timeout=0.5)
        except asyncio.TimeoutError:
            pass

        print("\n--- CDP Operation Latency ---")

        # Screenshot
        times, sizes = await self.bench_screenshot(session_id)
        med = statistics.median(times)
        avg_size = statistics.mean(sizes)
        print(f"  {'Screenshot':20s}  median={med:.3f}s  size={avg_size/1024:.0f}KB")
        all_results["screenshot"] = {"median": med, "raw": times, "avg_size_kb": avg_size/1024}

        # JS Evaluate
        times = await self.bench_evaluate(session_id)
        med = statistics.median(times)
        print(f"  {'JS Evaluate':20s}  median={med*1000:.1f}ms")
        all_results["js_evaluate"] = {"median_ms": med*1000, "raw": times}

        # Mouse Click
        times = await self.bench_click(session_id)
        med = statistics.median(times)
        print(f"  {'Mouse Click':20s}  median={med*1000:.1f}ms")
        all_results["mouse_click"] = {"median_ms": med*1000, "raw": times}

        # Keyboard Type
        times = await self.bench_type(session_id)
        med = statistics.median(times)
        print(f"  {'Type 11 chars':20s}  median={med*1000:.1f}ms")
        all_results["keyboard_type"] = {"median_ms": med*1000, "raw": times}

        # Layout Metrics
        times = await self.bench_get_layout_metrics(session_id)
        med = statistics.median(times)
        print(f"  {'Layout Metrics':20s}  median={med*1000:.1f}ms")
        all_results["layout_metrics"] = {"median_ms": med*1000, "raw": times}

        # Memory
        print("\n--- Resource Usage ---")
        mem, cpu, nprocs = await self.get_memory()
        print(f"  Browser processes: {nprocs}")
        print(f"  Private memory:    {mem/1024/1024:.0f} MB")
        print(f"  CPU time:          {cpu:.1f}s")
        all_results["memory_mb"] = mem/1024/1024
        all_results["cpu_time_s"] = cpu
        all_results["process_count"] = nprocs

        await self.send("Target.closeTarget", {"targetId": target_id})
        await self.ws.close()

        return all_results


async def main():
    instances = [
        ("BASELINE (v29 headless, 1vCPU/3GB)",
         "winter-mountain-2k9xdihk.dev-iad-unikraft-3.onkernel.app"),
        ("NEW (headless + live view, 1vCPU/3GB)",
         "silent-thunder-42i78h9l.dev-iad-unikraft-3.onkernel.app"),
        ("HEADFUL (kernel-cu-v33, 4vCPU/4GB)",
         "snowy-grass-r2apwx6u.dev-iad-unikraft-3.onkernel.app"),
    ]

    # Warm up all instances (they might be scaled to zero)
    print("Warming up instances...")
    ssl_ctx = ssl.create_default_context()
    ssl_ctx.check_hostname = False
    ssl_ctx.verify_mode = ssl.CERT_NONE
    import urllib.request
    for label, host in instances:
        for attempt in range(30):
            try:
                req = urllib.request.Request(f"https://{host}:444/spec.json")
                with urllib.request.urlopen(req, context=ssl_ctx, timeout=15) as r:
                    r.read()
                print(f"  {label}: ready")
                break
            except Exception as e:
                if attempt < 29:
                    await asyncio.sleep(5)
                else:
                    print(f"  {label}: FAILED ({e})")

    # Run benchmarks
    all_results = {}
    labels = []
    for label, host in instances:
        bench = CDPBenchmark(host, label)
        try:
            all_results[label] = await bench.run()
            labels.append(label)
        except Exception as e:
            print(f"\n  ERROR benchmarking {label}: {e}\n")

    if len(labels) < 2:
        print("Not enough successful benchmarks to compare.")
        return

    # Summary comparison table
    print(f"\n{'='*100}")
    print(f"  COMPARISON SUMMARY")
    print(f"{'='*100}")

    header = f"{'Operation':25s}"
    for l in labels:
        short = l.split("(")[1].split(",")[0] if "(" in l else l[:15]
        header += f" {short:>15s}"
    print(f"\n{header}")
    print(f"{'-'*25}" + f" {'-'*15}" * len(labels))

    # Navigation
    for name, url in URLS:
        key = f"nav_{name}"
        row = f"Nav {name:20s}"
        for l in labels:
            v = all_results[l][key]["median"]
            row += f" {v:>13.3f}s"
        print(row)

    print()

    # CDP ops
    for op, key, unit in [
        ("Screenshot", "screenshot", "s"),
        ("JS Evaluate", "js_evaluate", "ms"),
        ("Mouse Click", "mouse_click", "ms"),
        ("Type 11 chars", "keyboard_type", "ms"),
        ("Layout Metrics", "layout_metrics", "ms"),
    ]:
        row = f"{op:25s}"
        for l in labels:
            if unit == "s":
                v = all_results[l][key]["median"]
                row += f" {v:>13.3f}s"
            else:
                v = all_results[l][key]["median_ms"]
                row += f" {v:>12.1f}ms"
        print(row)

    print()

    # Resources
    row_mem = f"{'Memory':25s}"
    row_cpu = f"{'CPU time':25s}"
    row_proc = f"{'Processes':25s}"
    for l in labels:
        row_mem += f" {all_results[l]['memory_mb']:>12.0f}MB"
        row_cpu += f" {all_results[l]['cpu_time_s']:>13.1f}s"
        row_proc += f" {all_results[l]['process_count']:>15d}"
    print(row_mem)
    print(row_cpu)
    print(row_proc)


if __name__ == "__main__":
    asyncio.run(main())
