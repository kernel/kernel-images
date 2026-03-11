# Headful under Headless Resource Constraints

**Date**: 2026-03-10
**Goal**: Measure headful Chromium performance when given the same resource budget as headless (1024 MB / 4 vCPUs), compared against the standard headful allocation (8192 MB / 8 vCPUs) and headless baselines.

## Resource Configuration

| Variant | Image | Memory | vCPUs | Image Size |
|---|---|---|---|---|
| Baseline | headless | 1024 MB | 4 | 2.10 GB |
| Approach 1 (Xvfb/noVNC) | headless | 1024 MB | 4 | 2.22 GB |
| Approach 2 (CDP screencast) | headless | 1024 MB | 4 | 2.11 GB |
| Headful (standard) | headful | 8192 MB | 8 | 2.66 GB |
| **Headful (constrained)** | **headful** | **1024 MB** | **4** | **2.66 GB** |

## Memory Usage

| Variant | Idle | After Workload | Idle % of Limit |
|---|---|---|---|
| Baseline | 198.8 MiB | 274.2 MiB | 19% |
| Approach 1 | 268.0 MiB | 279.1 MiB | 26% |
| Approach 2 | 194.7 MiB | 343.1 MiB | 19% |
| Headful (8 GB) | 389.9 MiB | 862.0 MiB | 4.8% |
| **Headful (1 GB)** | **399.2 MiB** | **529.4 MiB** | **39%** |

The headful image consumes ~400 MiB at idle regardless of memory allocation. Under the 1 GB constraint this leaves only ~600 MiB for actual browsing — versus ~7.6 GB when given the full headful allocation. After a workload, it reaches 52% utilization, leaving little headroom for complex pages.

## Concurrent Throughput

| Variant | Workers | Duration | Operations Completed |
|---|---|---|---|
| Baseline | 3 | 20s | 471 |
| Approach 1 | 3 | 20s | 471 |
| Approach 2 | 3 | 20s | 471 |
| Headful (8 GB) | 3 | 20s | 471 |
| **Headful (1 GB)** | **3** | **20s** | **379** |

Constrained headful produced **19% fewer** concurrent operations than any other variant.

## Side-by-Side Comparison (Median Latency)

| Operation | Baseline | Approach 1 | Approach 2 | Headful (8 GB) | Headful (1 GB) | vs Baseline |
|---|---:|---:|---:|---:|---:|---|
| **Navigation** | | | | | | |
| Navigate[static] | 22.8ms | 30.4ms | 27.2ms | 35.1ms | 34.4ms | +50% |
| Navigate[content] | 119.4ms | 119.3ms | 214.4ms | 113.7ms | 124.2ms | +4% |
| Navigate[spa] | 1.45s | 1.37s | 1.36s | 1.29s | 1.46s | ~ |
| Navigate[media] | 764.5ms | 965.9ms | 867.2ms | 790.2ms | 950.8ms | +24% |
| **Screenshot** | | | | | | |
| Screenshot.JPEG.q80 | 62.5ms | 83.7ms | 57.4ms | 94.4ms | 88.1ms | +41% |
| Screenshot.PNG | 108.2ms | 120.5ms | 121.0ms | 171.9ms | 165.5ms | +53% |
| Screenshot.FullPage | 461.4ms | 494.4ms | 446.3ms | 448.1ms | 489.7ms | +6% |
| Screenshot.ClipRegion | 36.6ms | 42.9ms | 38.7ms | 84.9ms | 79.8ms | +118% |
| **JS Evaluation** | | | | | | |
| Eval.Trivial | 596us | 684us | 473us | 579us | 809us | +36% |
| Eval.QuerySelectorAll | 560us | 469us | 338us | 386us | 481us | -14% |
| Eval.InnerText | 964us | 1.9ms | 739us | 1.8ms | 1.2ms | +23% |
| Eval.GetComputedStyle | 5.3ms | 6.7ms | 4.9ms | 4.0ms | 5.7ms | +7% |
| Eval.ScrollToBottom | 376us | 579us | 490us | 372us | 480us | +28% |
| Eval.DOMManipulation | 627us | 872us | 652us | 580us | 838us | +34% |
| Eval.BoundingRects | 855us | 760us | 4.4ms | 658us | 1.1ms | +24% |
| **DOM** | | | | | | |
| DOM.GetDocument.Shallow | 238us | 355us | 277us | 219us | 281us | +18% |
| DOM.GetDocument.Deep | 4.2ms | 3.3ms | 5.9ms | 4.6ms | 4.0ms | ~ |
| DOM.GetDocument.Full | 17.8ms | 21.2ms | 22.1ms | 22.8ms | 28.7ms | +61% |
| DOM.QuerySelector | 305us | 326us | 304us | 314us | 327us | +7% |
| DOM.GetOuterHTML | 23.0ms | 24.4ms | 24.2ms | 19.5ms | 16.8ms | -27% |
| **Input** | | | | | | |
| Input.MouseMove | 4.5ms | 8.0ms | 4.9ms | 15.6ms | 22.4ms | **+397%** |
| Input.Click | 3.2ms | 3.2ms | 3.4ms | 3.1ms | 3.5ms | +8% |
| Input.TypeText | 9.5ms | 9.9ms | 9.4ms | 11.3ms | 11.5ms | +22% |
| Input.Scroll | 74.5ms | 72.9ms | 76.3ms | 198.8ms | 199.2ms | **+167%** |
| **Network** | | | | | | |
| Network.GetCookies | 483us | 660us | 394us | 523us | 544us | +13% |
| Network.GetResponseBody | 65.7ms | 59.2ms | 55.0ms | 68.3ms | 66.2ms | ~ |
| **Page** | | | | | | |
| Page.Reload | 139.8ms | 146.6ms | 135.1ms | 140.4ms | 141.3ms | ~ |
| Page.GetNavigationHistory | 1.2ms | 1.6ms | 1.3ms | 1.5ms | 1.2ms | ~ |
| Page.GetLayoutMetrics | 3.6ms | 3.3ms | 3.3ms | 2.3ms | 3.1ms | -14% |
| Page.PrintToPDF | 221.7ms | 225.8ms | 216.9ms | 229.7ms | 232.1ms | +5% |
| **Emulation** | | | | | | |
| Emulation.SetViewport | 499us | 322us | 302us | 692us | 550us | +10% |
| Emulation.SetMobile | 101.0ms | 105.0ms | 110.4ms | 95.4ms | 124.9ms | +24% |
| Emulation.SetGeolocation | 145us | 172us | 151us | 212us | 170us | +17% |
| **Target** | | | | | | |
| Target.GetTargets | 124us | 140us | 121us | 143us | 171us | +38% |
| Target.CreateAndClose | 17.4ms | 17.7ms | 16.9ms | 17.2ms | 22.5ms | +29% |
| **Composite** | | | | | | |
| Composite.NavAndScreenshot | 135.7ms | 154.1ms | 156.7ms | 228.1ms | 216.3ms | +59% |
| Composite.ScrapeLinks | 3.9ms | 4.7ms | 3.4ms | 4.1ms | 4.3ms | +9% |
| Composite.FillForm | 6.7ms | 8.0ms | 6.7ms | 7.5ms | 7.6ms | +13% |
| Composite.ClickAndWait | 24.6ms | 27.1ms | 26.8ms | 20.6ms | 23.1ms | -6% |
| Composite.RapidScreenshots | 693.2ms | 865.6ms | 691.7ms | 1.13s | 1.17s | **+69%** |
| Composite.ScrollAndCapture | 520.8ms | 649.0ms | 503.5ms | 605.2ms | 639.1ms | +23% |
| **Concurrent** | | | | | | |
| Concurrent.DOM | 339us | 432us | 328us | 328us | 342us | ~ |
| Concurrent.Evaluate | 19.1ms | 19.5ms | 18.4ms | 533us | 583us | -97% |
| Concurrent.Screenshot | 106.3ms | 112.5ms | 98.5ms | 147.8ms | 155.5ms | +46% |

## Key Findings

### Severely impacted operations (>50% slower than baseline)

- **Input.MouseMove**: +397% (4.5ms -> 22.4ms) — X display server overhead dominates
- **Input.Scroll**: +167% (74.5ms -> 199.2ms) — compositor + rendering pipeline under memory pressure
- **Screenshot.ClipRegion**: +118% (36.6ms -> 79.8ms)
- **Composite.RapidScreenshots**: +69% (693ms -> 1.17s)
- **DOM.GetDocument.Full**: +61% (17.8ms -> 28.7ms)
- **Composite.NavAndScreenshot**: +59% (135.7ms -> 216.3ms)
- **Screenshot.PNG**: +53% (108ms -> 165ms)

### Moderately impacted (10–50% slower)

- Screenshot.JPEG.q80: +41%
- Target.GetTargets: +38%
- Eval.Trivial: +36%
- Eval.DOMManipulation: +34%
- Target.CreateAndClose: +29%
- Eval.ScrollToBottom: +28%
- Navigate[static]: +50%

### Unaffected operations (~baseline)

- Page.Reload, Page.GetNavigationHistory, Network.GetResponseBody
- DOM.GetDocument.Deep, Concurrent.DOM
- Input.Click (already fast enough that overhead doesn't matter)

## Analysis

The headful image running under headless-level resource constraints (1024 MB / 4 vCPUs) is **not a viable configuration**:

1. **Memory pressure**: The X display server (Xvfb), window manager, and VNC/noVNC infrastructure consume ~400 MiB at idle — 39% of the 1 GB limit. This leaves only ~600 MiB for Chromium to render pages. In contrast, baseline headless uses only 199 MiB idle (19%).

2. **Input latency explosion**: Operations that require rendering through the X display pipeline (MouseMove, Scroll) see the largest regressions. The compositor must process events through Xvfb's framebuffer, which becomes a bottleneck under CPU constraints.

3. **Screenshot overhead**: Every screenshot requires reading from the X framebuffer, adding overhead proportional to the captured area. Clip-region screenshots (+118%) and rapid sequential screenshots (+69%) are particularly affected.

4. **Reduced throughput**: Concurrent workloads completed only 379 operations versus 471 for baseline — a 19% throughput reduction. Under sustained parallel load, the constrained headful image falls behind.

5. **Comparison with standard headful**: The constrained headful performs similarly to or slightly worse than standard headful (8 GB/8 vCPUs) on most operations. The main difference is that standard headful has ample headroom, while constrained headful is operating near its limits and would degrade further on complex pages.

## Conclusion

Running the headful image at headless-equivalent resources is not advisable. If a live view is needed within the headless resource envelope (1024 MB / 4 vCPUs), **Approach 2 (CDP screencast)** is the clear winner: it maintains near-baseline CDP latency while adding only ~10 MB to image size and negligible idle memory overhead.
