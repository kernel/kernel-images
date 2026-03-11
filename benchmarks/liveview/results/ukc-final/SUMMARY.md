# Unikraft (KraftCloud) Live View Benchmark Results

**Date**: 2026-03-11
**Platform**: KraftCloud (dev-iad-unikraft-3)
**Iterations**: 2 (warmup: 1)
**Concurrent**: 3 workers, 20s

## Variants

| Variant | Branch | Image Type | Memory | vCPUs | Image Size |
|---|---|---|---|---|---|
| Baseline | main | headless | 3072 MiB | default | 2.1 GB |
| Approach 1 (Xvfb/noVNC) | feat/headless-live-view | headless | 3072 MiB | default | 2.2 GB |
| Approach 2 (CDP screencast) | headless-cdp-live-view | headless | 3072 MiB | default | 2.1 GB |
| Headful | main | headful | 4096 MiB | 4 | 2.6 GB |

Note: headless instances use KraftCloud default vCPU allocation (1 vCPU in run-unikernel.sh). Headful gets 4 vCPUs and 4096 MiB per run-unikernel.sh defaults.

## Side-by-Side Comparison (Median Latency)

| Operation | Baseline | Approach 1 | Approach 2 | Headful |
|---|---:|---:|---:|---:|
| **Navigation** | | | | |
| Navigate[static] | 91.2ms | 96.2ms | 82.8ms | 54.1ms |
| Navigate[content] | 218.1ms | 176.0ms | 180.0ms | 130.5ms |
| Navigate[spa] | 3.30s | 2.33s | 2.52s | 1.51s |
| Navigate[media] | 3.02s | 1.83s | 1.81s | 797.9ms |
| **Screenshot** | | | | |
| Screenshot.JPEG.q80 | 78.5ms | 87.4ms | 105.8ms | 134.9ms |
| Screenshot.PNG | 112.4ms | 178.7ms | 144.3ms | 146.6ms |
| Screenshot.FullPage | 695.8ms | 636.0ms | 764.8ms | 548.9ms |
| Screenshot.ClipRegion | 40.6ms | 42.5ms | 40.6ms | 77.0ms |
| **JS Evaluation** | | | | |
| Eval.Trivial | 1.8ms | 1.9ms | 2.1ms | 1.7ms |
| Eval.QuerySelectorAll | 1.6ms | 1.6ms | 1.7ms | 1.6ms |
| Eval.InnerText | 2.0ms | 2.9ms | 3.1ms | 2.2ms |
| Eval.GetComputedStyle | 6.8ms | 20.4ms | 8.0ms | 6.5ms |
| Eval.ScrollToBottom | 2.4ms | 3.5ms | 1.9ms | 1.6ms |
| Eval.DOMManipulation | 2.0ms | 2.2ms | 2.0ms | 2.0ms |
| Eval.BoundingRects | 2.8ms | 2.2ms | 2.2ms | 2.0ms |
| **DOM** | | | | |
| DOM.GetDocument.Shallow | 1.7ms | 1.7ms | 1.6ms | 1.6ms |
| DOM.GetDocument.Deep | 5.7ms | 5.7ms | 5.7ms | 4.9ms |
| DOM.GetDocument.Full | 42.7ms | 47.0ms | 43.7ms | 40.4ms |
| DOM.QuerySelector | 2.1ms | 1.9ms | 2.1ms | 1.8ms |
| DOM.GetOuterHTML | 32.2ms | 32.0ms | 32.4ms | 33.7ms |
| **Input** | | | | |
| Input.MouseMove | 4.6ms | 12.1ms | 7.2ms | 34.3ms |
| Input.Click | 6.0ms | 5.7ms | 6.4ms | 5.4ms |
| Input.TypeText | 70.2ms | 72.2ms | 122.5ms | 61.6ms |
| Input.Scroll | 87.1ms | 89.4ms | 81.5ms | 195.5ms |
| **Network** | | | | |
| Network.GetCookies | 1.9ms | 1.8ms | 1.6ms | 1.7ms |
| Network.GetResponseBody | 70.7ms | 71.3ms | 77.6ms | 32.7ms |
| **Page** | | | | |
| Page.Reload | 164.4ms | 259.1ms | 179.1ms | 105.7ms |
| Page.GetNavigationHistory | 4.4ms | 4.7ms | 2.8ms | 1.8ms |
| Page.GetLayoutMetrics | 9.4ms | 4.9ms | 11.0ms | 9.4ms |
| Page.PrintToPDF | 577.0ms | 390.8ms | 370.9ms | 332.1ms |
| **Emulation** | | | | |
| Emulation.SetViewport | 2.9ms | 2.3ms | 2.7ms | 2.0ms |
| Emulation.SetMobile | 144.7ms | 110.2ms | 98.7ms | 78.9ms |
| Emulation.SetGeolocation | 1.6ms | 1.3ms | 1.4ms | 1.6ms |
| **Target** | | | | |
| Target.GetTargets | 1.7ms | 1.4ms | 1.4ms | 1.4ms |
| Target.CreateAndClose | 80.7ms | 60.5ms | 62.1ms | 35.4ms |
| **Composite** | | | | |
| Composite.NavAndScreenshot | 344.4ms | 418.2ms | 366.8ms | 289.4ms |
| Composite.ScrapeLinks | 4.6ms | 3.2ms | 6.2ms | 2.9ms |
| Composite.FillForm | 16.8ms | 26.9ms | 26.2ms | 13.8ms |
| Composite.ClickAndWait | 109.3ms | 122.7ms | 121.4ms | 37.4ms |
| Composite.RapidScreenshots | 912.3ms | 895.5ms | 941.2ms | 1.07s |
| Composite.ScrollAndCapture | 521.6ms | 537.8ms | 503.5ms | 642.7ms |
| **Concurrent** | | | | |
| Concurrent.DOM | 2.1ms | 2.3ms | 2.1ms | 1.8ms |
| Concurrent.Evaluate | 18.5ms | 18.3ms | 19.2ms | 10.0ms |
| Concurrent.Screenshot | 152.1ms | 157.6ms | 150.7ms | 151.4ms |

## Key Observations

### Network latency baseline

All operations on Unikraft include ~1.5ms of round-trip network overhead (TLS WebSocket over the internet) compared to the Docker benchmarks which ran inside the container. This explains why sub-millisecond operations (Eval.Trivial, DOM.GetDocument.Shallow, etc.) show 1.5-2ms minimums on Unikraft.

### Approach 1 (Xvfb/noVNC) vs Baseline

- **Input.MouseMove**: 12.1ms vs 4.6ms (+163%) — X display pipeline overhead
- **Eval.GetComputedStyle**: 20.4ms vs 6.8ms (+200%) — rendering costs with active X display
- **Page.Reload**: 259.1ms vs 164.4ms (+58%)
- **Composite.FillForm**: 26.9ms vs 16.8ms (+60%)
- Most DOM/Network operations: minimal difference

### Approach 2 (CDP screencast) vs Baseline

- **Screenshot.JPEG.q80**: 105.8ms vs 78.5ms (+35%) — screencast background processing
- **Input.TypeText**: 122.5ms vs 70.2ms (+74%) — input event forwarding overhead
- **Most operations within noise**: DOM, Network, Target, Emulation nearly identical
- **Some operations faster**: Navigate[media] 1.81s vs 3.02s (-40%), Emulation.SetMobile 98.7ms vs 144.7ms (-32%)

### Headful vs Headless Baseline

Headful gets 4x more memory (4096 vs 3072 MiB) and 4x more vCPUs. This extra allocation shows:
- **Navigation much faster**: Navigate[spa] 1.51s vs 3.30s, Navigate[media] 797ms vs 3.02s
- **Input.Scroll much slower**: 195.5ms vs 87.1ms (+124%) — X display compositor
- **Input.MouseMove much slower**: 34.3ms vs 4.6ms (+646%) — X display pipeline
- **Target.CreateAndClose faster**: 35.4ms vs 80.7ms — more CPU available
- **Concurrent.Screenshot similar**: 151.4ms vs 152.1ms — screenshot perf CPU-bound

### Unikraft vs Docker comparison (Baseline)

| Operation | Docker (in-container) | Unikraft (remote) | Notes |
|---|---:|---:|---|
| Eval.Trivial | 570us | 1.8ms | +~1.2ms network RTT |
| DOM.GetDocument.Shallow | 295us | 1.7ms | +~1.4ms network RTT |
| Screenshot.JPEG.q80 | 65.4ms | 78.5ms | +20% (network + env diff) |
| Navigate[static] | 22.7ms | 91.2ms | 4x slower (1 vCPU vs 4) |
| Navigate[spa] | 1.44s | 3.30s | 2.3x slower (CPU-bound) |
| Concurrent.Screenshot | 106.3ms | 152.1ms | +43% |

The biggest difference is CPU-sensitive operations. Unikraft headless runs on 1 vCPU (per run-unikernel.sh default) vs Docker's 4 vCPUs, making navigation and rendering significantly slower.

## Conclusion

On Unikraft, both live view approaches add **moderate overhead** to the headless baseline:

- **Approach 1 (Xvfb/noVNC)**: Largest impact on input and rendering operations due to the X display server pipeline. Adds ~100 MB to image size.
- **Approach 2 (CDP screencast)**: Smaller and more targeted overhead, mainly on screenshot and input forwarding operations. Adds ~10 MB to image size.
- **Headful**: Fastest for navigation due to 4x more resources, but pays a heavy penalty on input/scroll operations due to compositor overhead.

For the headless use case on Unikraft, **Approach 2 (CDP screencast) remains the better choice** — it preserves true headless mode, has minimal image size impact, and its overhead is concentrated in screenshot operations that are only active when someone is watching the live view.
