# Live View Benchmark

Compares CDP operation latency and resource usage across four image variants:

| Variant | Branch | Image | Description |
|---|---|---|---|
| `baseline` | `main` | headless | Current production headless (no live view) |
| `approach1` | `feat/headless-live-view` | headless | Xvfb + noVNC live view (PR #174) |
| `approach2` | `headless-cdp-live-view` | headless | CDP screencast live view (PR #176) |
| `headful` | `main` | headful | Full headed browser (reference) |

## Test sites (15 sites across 5 categories)

| Category | Sites |
|---|---|
| Static | example.com, httpbin.org, lite.duckduckgo.com |
| Content | Hacker News, Wikipedia, Reuters, Lobsters |
| SPA | Google Maps, GitHub, Reddit |
| Media | Unsplash, BBC News, CNN |
| Complex | Amazon, eBay |

## CDP operations tested (40+ operations across 9 categories)

**Navigation** — full page load to each site category

**Screenshots** — JPEG (q80), PNG, full-page capture, clipped region

**JS Evaluation** — trivial expression, querySelectorAll count, innerText length, getComputedStyle, scroll to bottom, DOM manipulation (create/remove 100 elements), bounding rect collection

**DOM** — getDocument at depth 1/5/full, querySelector, getOuterHTML

**Input** — mouse move, click (press+release), type 21 chars, scroll (5x wheel events)

**Network** — getCookies, fetch page body via Runtime.evaluate

**Page** — reload (to load event), getNavigationHistory, getLayoutMetrics, printToPDF

**Emulation** — set desktop viewport, toggle mobile viewport, set geolocation

**Target** — getTargets, create+close tab round-trip

**Composite scenarios** (realistic multi-step automations):
- Navigate + screenshot
- Scrape all links on page
- Inject form + fill + read back
- Click first link + wait for navigation
- 10 rapid screenshots in sequence
- Scroll 5 steps with screenshot at each position

**Concurrent load test** — N parallel CDP connections all firing screenshots, evaluates, and DOM queries simultaneously for a configurable duration

## Resource allocation

Containers are constrained to match production allocations:

| Type | Memory | CPUs | Variants |
|---|---|---|---|
| Headless | 1024 MB | 4 | baseline, approach1, approach2 |
| Headful | 8192 MB | 8 | headful |

Override with env vars:

```bash
HEADLESS_MEMORY=2048m HEADLESS_CPUS=4 HEADFUL_MEMORY=8192m HEADFUL_CPUS=8 ./run.sh
```

## Quick start

```bash
./run.sh

# Customize
ITERATIONS=5 WARMUP=2 CONCURRENT_WORKERS=5 CONCURRENT_DURATION=60s ./run.sh

# Skip concurrent test for faster runs
SKIP_CONCURRENT=true ITERATIONS=3 ./run.sh

# Run specific variants only
VARIANTS="baseline:main:headless: approach2:headless-cdp-live-view:headless:-e=ENABLE_LIVE_VIEW=true=-p=8080:8080" ./run.sh
```

## Requirements

- Docker with BuildKit
- Go 1.25+
- python3 (for summary table generation; optional)

## Output

Results go to `results/<timestamp>/`:

```
results/20260310-143000/
├── SUMMARY.md              # Combined comparison table
├── baseline/
│   ├── results.md          # Markdown latency table
│   ├── results.json        # Machine-readable results
│   ├── docker-stats.csv    # CPU/memory time series
│   ├── memory.txt          # Idle + post-workload memory snapshots
│   ├── container.log       # Full container logs
│   └── bench.log           # Benchmark stderr
├── approach1/
├── approach2/
└── headful/
```

## Running the Go benchmark standalone

```bash
cd server
go build -o ../benchmarks/liveview/bench ../benchmarks/liveview/main.go

# Against an already-running container
../benchmarks/liveview/bench \
  -cdp-url ws://127.0.0.1:9222 \
  -iterations 3 \
  -label mytest \
  -concurrent-workers 3 \
  -concurrent-duration 30s

# JSON output
../benchmarks/liveview/bench -cdp-url ws://127.0.0.1:9222 -iterations 3 -label mytest -json

# Quick run without concurrent test
../benchmarks/liveview/bench -cdp-url ws://127.0.0.1:9222 -iterations 2 -skip-concurrent -label quick
```
