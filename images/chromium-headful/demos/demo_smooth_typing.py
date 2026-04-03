#!/usr/bin/env python3
"""
Demo script: smooth typing vs instant typing.

Drives the typing_demo.html page through the kernel-images API to produce
a side-by-side comparison suitable for recording as a GIF/MP4.

Usage:
    # 1. Start a kernel-images container
    # 2. Upload typing_demo.html to the container
    # 3. Run this script:
    python demo_smooth_typing.py --base-url http://localhost:8000

Requirements:
    pip install requests
"""

import argparse
import base64
import json
import time
from pathlib import Path

import requests

DEMO_TEXT = "The quick brown fox jumps over the lazy dog. Hello world!"


def api(base: str, method: str, path: str, **kwargs):
    url = f"{base}{path}"
    resp = getattr(requests, method)(url, **kwargs)
    resp.raise_for_status()
    return resp


def upload_demo_page(base: str):
    html_path = Path(__file__).parent / "typing_demo.html"
    html_bytes = html_path.read_bytes()
    api(base, "put", "/fs/write_file", params={"path": "/tmp/typing_demo.html"},
        data=html_bytes, headers={"Content-Type": "application/octet-stream"})
    print("Uploaded typing_demo.html")


def execute_js(base: str, code: str):
    api(base, "post", "/playwright/execute", json={"code": code, "timeout_sec": 10})


def navigate(base: str):
    execute_js(base, "await page.goto('file:///tmp/typing_demo.html');")
    time.sleep(1)


def click_input(base: str):
    execute_js(base, "await page.click('#input');")
    time.sleep(0.3)


def clear_input(base: str):
    execute_js(base, "window.demoApi.clear();")
    time.sleep(0.3)


def set_mode(base: str, label: str, cls: str):
    execute_js(base, f"window.demoApi.setMode('{label}', '{cls}');")


def type_text(base: str, text: str, smooth: bool = False, typo_chance: float = 0):
    body = {"text": text, "smooth": smooth}
    if typo_chance > 0:
        body["typo_chance"] = typo_chance
    if not smooth:
        body["delay"] = 0
    api(base, "post", "/computer/type", json=body)


def start_recording(base: str):
    api(base, "post", "/recording/start", json={"framerate": 15, "id": "typing-demo"})
    print("Recording started")
    time.sleep(0.5)


def stop_recording(base: str):
    api(base, "post", "/recording/stop", json={"id": "typing-demo"})
    time.sleep(1)
    print("Recording stopped")


def download_recording(base: str, output: str):
    resp = api(base, "get", "/recording/download", params={"id": "typing-demo"})
    Path(output).write_bytes(resp.content)
    print(f"Saved recording to {output}")


def run_demo(base: str, output: str):
    upload_demo_page(base)
    navigate(base)
    start_recording(base)

    # --- Phase 1: Instant typing (no delay) ---
    set_mode(base, "INSTANT TYPING — delay: 0", "instant")
    time.sleep(1)
    click_input(base)
    type_text(base, DEMO_TEXT, smooth=False)
    time.sleep(2)

    # --- Phase 2: Smooth typing (no typos) ---
    clear_input(base)
    set_mode(base, "SMOOTH TYPING — HUMAN-LIKE", "smooth")
    time.sleep(1)
    click_input(base)
    type_text(base, DEMO_TEXT, smooth=True)
    time.sleep(2)

    # --- Phase 3: Smooth typing with typos ---
    clear_input(base)
    set_mode(base, "SMOOTH TYPING — WITH TYPOS", "typos")
    time.sleep(1)
    click_input(base)
    type_text(base, DEMO_TEXT, smooth=True, typo_chance=0.04)
    time.sleep(2)

    stop_recording(base)
    download_recording(base, output)


def main():
    parser = argparse.ArgumentParser(description="Smooth typing demo recorder")
    parser.add_argument("--base-url", default="http://localhost:8000",
                        help="Base URL of the kernel-images API")
    parser.add_argument("--output", default="smooth_typing_demo.mp4",
                        help="Output video file path")
    args = parser.parse_args()
    run_demo(args.base_url, args.output)


if __name__ == "__main__":
    main()
