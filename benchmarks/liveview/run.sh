#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT="$SCRIPT_DIR/../.."
RESULTS_DIR="$SCRIPT_DIR/results/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"

ITERATIONS="${ITERATIONS:-10}"
WARMUP="${WARMUP:-2}"
SETTLE_SECS="${SETTLE_SECS:-15}"
STATS_INTERVAL="${STATS_INTERVAL:-1}"
STATS_DURATION="${STATS_DURATION:-60}"
# Chrome listens on 9223 inside the container (127.0.0.1 only).
# The kernel-images-api proxy on 9222 doesn't forward all CDP methods,
# so we copy the benchmark binary into the container and run it there.
CHROME_INTERNAL_PORT=9223

# Resource limits matching production allocations:
#   headless: 1024 MB / 4 vCPUs
#   headful:  8192 MB / 8 vCPUs
HEADLESS_MEMORY="${HEADLESS_MEMORY:-1024m}"
HEADLESS_CPUS="${HEADLESS_CPUS:-4}"
HEADFUL_MEMORY="${HEADFUL_MEMORY:-8192m}"
HEADFUL_CPUS="${HEADFUL_CPUS:-8}"

# --------------------------------------------------------------------------- #
# Which image variants to benchmark                                            #
# --------------------------------------------------------------------------- #
# Each entry: <label>:<git-ref>:<image-type>:<extra-docker-run-args>
# image-type is "headless" or "headful"
# extra-docker-run-args are pipe-separated, no spaces. Use = between flag and value:
#   -eENABLE_LIVE_VIEW=true|-p8080:8080
# Set VARIANTS env to override, e.g.:
#   VARIANTS="baseline:main:headless:" ./run.sh
VARIANTS="${VARIANTS:-baseline:main:headless: approach1:feat/headless-live-view:headless:-eENABLE_LIVE_VIEW=true|-p8080:8080 approach2:headless-cdp-live-view:headless:-eENABLE_LIVE_VIEW=true|-p8080:8080 headful:main:headful:}"

log() { echo "[bench] $(date +%H:%M:%S) $*"; }

# --------------------------------------------------------------------------- #
# Build the benchmark Go binary                                                #
# --------------------------------------------------------------------------- #
build_bench() {
  log "Building benchmark binary..."
  cd "$REPO_ROOT/server"
  go build -o "$SCRIPT_DIR/bench" "$SCRIPT_DIR/main.go"
  cd "$SCRIPT_DIR"
  log "Benchmark binary built: $SCRIPT_DIR/bench"
}

# --------------------------------------------------------------------------- #
# Build a docker image from a specific git ref                                 #
# --------------------------------------------------------------------------- #
build_image() {
  local label="$1" ref="$2" image_type="$3"
  local image_tag="liveview-bench-${label}:latest"

  log "Building image '$image_tag' from ref '$ref' (type=$image_type)..."

  # Stash any local changes, checkout the ref, build, then restore
  cd "$REPO_ROOT"

  local current_ref
  current_ref=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || git rev-parse HEAD)
  local stashed=false
  if ! git diff --quiet 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
    git stash push -m "bench-$label" >/dev/null 2>&1
    stashed=true
  fi

  git checkout "$ref" --quiet 2>/dev/null || git checkout "origin/$ref" --quiet

  if [[ "$image_type" == "headful" ]]; then
    docker build -f images/chromium-headful/Dockerfile -t "$image_tag" . 2>&1 | tail -5
  else
    docker build -f images/chromium-headless/image/Dockerfile -t "$image_tag" . 2>&1 | tail -5
  fi

  git checkout "$current_ref" --quiet 2>/dev/null || true
  if $stashed; then
    git stash pop --quiet 2>/dev/null || true
  fi

  cd "$SCRIPT_DIR"
  log "Image '$image_tag' built."
}

# --------------------------------------------------------------------------- #
# Run a container and wait for CDP to be ready                                 #
# --------------------------------------------------------------------------- #
start_container() {
  local label="$1" image_type="$2" extra_args_encoded="$3"
  local image_tag="liveview-bench-${label}:latest"
  local container_name="liveview-bench-${label}"

  docker rm -f "$container_name" 2>/dev/null || true

  local container_memory container_cpus
  if [[ "$image_type" == "headful" ]]; then
    container_memory="$HEADFUL_MEMORY"
    container_cpus="$HEADFUL_CPUS"
  else
    container_memory="$HEADLESS_MEMORY"
    container_cpus="$HEADLESS_CPUS"
  fi

  log "Resources: memory=$container_memory cpus=$container_cpus (image_type=$image_type)"

  local run_args=(
    --name "$container_name"
    --privileged
    --tmpfs /dev/shm:size=2g
    --memory "$container_memory"
    --cpus "$container_cpus"
    -p 10001:10001
    -d
  )

  # Parse extra args (pipe-separated, e.g. "-eENABLE_LIVE_VIEW=true|-p8080:8080")
  if [[ -n "$extra_args_encoded" ]]; then
    IFS='|' read -ra parts <<< "$extra_args_encoded"
    for part in "${parts[@]}"; do
      [[ -n "$part" ]] && run_args+=("$part")
    done
  fi

  if [[ "$image_type" == "headful" ]]; then
    run_args+=(
      -e DISPLAY_NUM=1
      -e HEIGHT=1080
      -e WIDTH=1920
      -e HOME=/
      -e ENABLE_WEBRTC=true
      -e RUN_AS_ROOT=false
    )
  fi

  log "Starting container '$container_name'..."
  docker run "${run_args[@]}" "$image_tag"

  log "Waiting for CDP on container port $CHROME_INTERNAL_PORT..."
  local attempts=0
  while ! docker exec "$container_name" bash -c "echo > /dev/tcp/127.0.0.1/$CHROME_INTERNAL_PORT" 2>/dev/null; do
    sleep 1
    attempts=$((attempts + 1))
    if (( attempts > 120 )); then
      log "ERROR: CDP not ready after 120s"
      docker logs "$container_name" --tail 30
      return 1
    fi
  done
  log "CDP ready after ${attempts}s."

  # Copy benchmark binary into the container
  docker cp "$SCRIPT_DIR/bench" "$container_name:/tmp/bench"
}

# --------------------------------------------------------------------------- #
# Collect docker stats in background                                           #
# --------------------------------------------------------------------------- #
collect_stats() {
  local container_name="$1" output_file="$2" duration="$3"
  log "Collecting docker stats for ${duration}s -> $output_file"
  (
    echo "timestamp,cpu_pct,mem_usage,mem_limit,mem_pct,net_io,pids"
    local elapsed=0
    while (( elapsed < duration )); do
      local line
      line=$(docker stats "$container_name" --no-stream --format \
        "{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}},{{.NetIO}},{{.PIDs}}" 2>/dev/null || echo ",,,,")
      echo "$(date +%s),$line"
      sleep "$STATS_INTERVAL"
      elapsed=$((elapsed + STATS_INTERVAL))
    done
  ) > "$output_file" &
  echo $!
}

# --------------------------------------------------------------------------- #
# Snapshot memory at a point in time                                           #
# --------------------------------------------------------------------------- #
snapshot_memory() {
  local container_name="$1" label="$2" output_file="$3"
  local mem
  mem=$(docker stats "$container_name" --no-stream --format "{{.MemUsage}}" 2>/dev/null || echo "N/A")
  echo "${label}: ${mem}" >> "$output_file"
  log "Memory ($label): $mem"
}

# --------------------------------------------------------------------------- #
# Run the full benchmark for one variant                                       #
# --------------------------------------------------------------------------- #
bench_variant() {
  local label="$1" ref="$2" image_type="$3" extra_args="$4"
  local container_name="liveview-bench-${label}"
  local variant_dir="$RESULTS_DIR/$label"
  mkdir -p "$variant_dir"

  log "========================================"
  log "  Benchmarking: $label"
  log "  Ref: $ref | Type: $image_type"
  log "========================================"

  # Build & start
  build_image "$label" "$ref" "$image_type"
  start_container "$label" "$image_type" "$extra_args"

  # Record resource config
  local container_memory container_cpus
  if [[ "$image_type" == "headful" ]]; then
    container_memory="$HEADFUL_MEMORY"
    container_cpus="$HEADFUL_CPUS"
  else
    container_memory="$HEADLESS_MEMORY"
    container_cpus="$HEADLESS_CPUS"
  fi
  echo "config_memory: $container_memory" > "$variant_dir/memory.txt"
  echo "config_cpus: $container_cpus" >> "$variant_dir/memory.txt"
  echo "image_type: $image_type" >> "$variant_dir/memory.txt"

  # Let the container fully settle
  log "Settling for ${SETTLE_SECS}s..."
  sleep "$SETTLE_SECS"

  # Memory snapshot: idle (after startup, no workload)
  snapshot_memory "$container_name" "idle" "$variant_dir/memory.txt"

  # Collect docker stats throughout the benchmark
  local stats_pid
  stats_pid=$(collect_stats "$container_name" "$variant_dir/docker-stats.csv" "$STATS_DURATION")

  # Run CDP benchmark
  CONCURRENT_WORKERS="${CONCURRENT_WORKERS:-3}"
  CONCURRENT_DURATION="${CONCURRENT_DURATION:-30s}"
  SKIP_CONCURRENT="${SKIP_CONCURRENT:-false}"
  CONCURRENT_FLAG=""
  if [[ "$SKIP_CONCURRENT" == "true" ]]; then
    CONCURRENT_FLAG="-skip-concurrent"
  fi

  local bench_args=(
    -cdp-url "http://127.0.0.1:${CHROME_INTERNAL_PORT}"
    -iterations "$ITERATIONS"
    -warmup "$WARMUP"
    -label "$label"
    -concurrent-workers "$CONCURRENT_WORKERS"
    -concurrent-duration "$CONCURRENT_DURATION"
  )
  if [[ -n "$CONCURRENT_FLAG" ]]; then
    bench_args+=("$CONCURRENT_FLAG")
  fi

  log "Running CDP benchmark (iterations=$ITERATIONS, warmup=$WARMUP, concurrent_workers=$CONCURRENT_WORKERS)..."
  docker exec "$container_name" /tmp/bench "${bench_args[@]}" \
    > "$variant_dir/results.md" 2> "$variant_dir/bench.log"

  # Also save JSON
  docker exec "$container_name" /tmp/bench "${bench_args[@]}" -json \
    > "$variant_dir/results.json" 2> "$variant_dir/bench-json.log"

  # Memory snapshot: after workload
  snapshot_memory "$container_name" "after-workload" "$variant_dir/memory.txt"

  # Docker image size
  local image_size
  image_size=$(docker images "liveview-bench-${label}:latest" --format "{{.Size}}")
  echo "image_size: $image_size" >> "$variant_dir/memory.txt"
  log "Image size: $image_size"

  # Stop stats collection
  kill "$stats_pid" 2>/dev/null || true
  wait "$stats_pid" 2>/dev/null || true

  # Save container logs
  docker logs "$container_name" > "$variant_dir/container.log" 2>&1 || true

  # Tear down
  docker rm -f "$container_name" 2>/dev/null || true

  log "Variant '$label' complete. Results in $variant_dir/"
}

# --------------------------------------------------------------------------- #
# Summarize all variants into a single comparison table                        #
# --------------------------------------------------------------------------- #
summarize() {
  local summary="$RESULTS_DIR/SUMMARY.md"

  echo "# Live View Benchmark Results" > "$summary"
  echo "" >> "$summary"
  echo "Date: $(date)" >> "$summary"
  echo "Iterations: $ITERATIONS (warmup: $WARMUP)" >> "$summary"
  echo "" >> "$summary"
  echo "### Resource Allocation" >> "$summary"
  echo "| Type | Memory | CPUs |" >> "$summary"
  echo "|---|---|---|" >> "$summary"
  echo "| Headless | $HEADLESS_MEMORY | $HEADLESS_CPUS |" >> "$summary"
  echo "| Headful | $HEADFUL_MEMORY | $HEADFUL_CPUS |" >> "$summary"
  echo "" >> "$summary"

  # Per-variant results
  for entry in $VARIANTS; do
    IFS=':' read -r label ref image_type extra <<< "$entry"
    local variant_dir="$RESULTS_DIR/$label"
    if [[ -f "$variant_dir/results.md" ]]; then
      cat "$variant_dir/results.md" >> "$summary"
      echo "" >> "$summary"
    fi
    if [[ -f "$variant_dir/memory.txt" ]]; then
      echo "### ${label} — Resource Usage" >> "$summary"
      echo '```' >> "$summary"
      cat "$variant_dir/memory.txt" >> "$summary"
      echo '```' >> "$summary"
      echo "" >> "$summary"
    fi
  done

  # Side-by-side comparison from JSON
  echo "## Side-by-side Comparison (Median)" >> "$summary"
  echo "" >> "$summary"

  # Collect all operation names from the first available JSON
  local all_ops=""
  for entry in $VARIANTS; do
    IFS=':' read -r label _ _ _ <<< "$entry"
    local json_file="$RESULTS_DIR/$label/results.json"
    if [[ -f "$json_file" ]]; then
      all_ops=$(python3 -c "
import json
data = json.load(open('$json_file'))
for r in data.get('results', []):
    print(r['operation'])
" 2>/dev/null || echo "")
      break
    fi
  done

  if [[ -z "$all_ops" ]]; then
    echo "(no JSON results found)" >> "$summary"
  else
    # Header
    printf "| %-34s " "Operation" >> "$summary"
    for entry in $VARIANTS; do
      IFS=':' read -r label _ _ _ <<< "$entry"
      printf "| %12s " "$label" >> "$summary"
    done
    echo "|" >> "$summary"

    # Separator
    printf "|%s" "$(printf '%.0s-' {1..36})" >> "$summary"
    for entry in $VARIANTS; do
      printf "|%s" "$(printf '%.0s-' {1..14})" >> "$summary"
    done
    echo "|" >> "$summary"

    local current_cat=""
    while IFS= read -r op; do
      [[ -z "$op" ]] && continue

      # Print category header if changed
      local cat=""
      for entry in $VARIANTS; do
        IFS=':' read -r label _ _ _ <<< "$entry"
        local json_file="$RESULTS_DIR/$label/results.json"
        if [[ -f "$json_file" ]]; then
          cat=$(python3 -c "
import json
data = json.load(open('$json_file'))
for r in data.get('results', []):
    if r['operation'] == '$op':
        print(r.get('category', ''))
        break
" 2>/dev/null || echo "")
          [[ -n "$cat" ]] && break
        fi
      done

      if [[ -n "$cat" && "$cat" != "$current_cat" ]]; then
        current_cat="$cat"
        printf "| **%-32s** " "$cat" >> "$summary"
        for entry in $VARIANTS; do
          printf "| %12s " "" >> "$summary"
        done
        echo "|" >> "$summary"
      fi

      printf "| %-34s " "$op" >> "$summary"
      for entry in $VARIANTS; do
        IFS=':' read -r label _ _ _ <<< "$entry"
        local json_file="$RESULTS_DIR/$label/results.json"
        if [[ -f "$json_file" ]]; then
          local median
          median=$(python3 -c "
import json, sys
data = json.load(open('$json_file'))
for r in data.get('results', []):
    if r['operation'] == '$op':
        v = r['median_ms']
        if v < 1:
            print(f'{v*1000:.0f}µs')
        elif v < 1000:
            print(f'{v:.1f}ms')
        else:
            print(f'{v/1000:.2f}s')
        sys.exit()
print('-')
" 2>/dev/null || echo "-")
          printf "| %12s " "$median" >> "$summary"
        else
          printf "| %12s " "-" >> "$summary"
        fi
      done
      echo "|" >> "$summary"
    done <<< "$all_ops"
  fi
  echo "" >> "$summary"

  log "Summary written to $summary"
  echo ""
  cat "$summary"
}

# --------------------------------------------------------------------------- #
# Main                                                                         #
# --------------------------------------------------------------------------- #
main() {
  log "Live View Benchmark"
  log "Results dir: $RESULTS_DIR"
  log "Variants: $VARIANTS"
  log "Iterations: $ITERATIONS, Warmup: $WARMUP"
  log "Headless resources: ${HEADLESS_MEMORY} / ${HEADLESS_CPUS} CPUs"
  log "Headful resources:  ${HEADFUL_MEMORY} / ${HEADFUL_CPUS} CPUs"

  build_bench

  for entry in $VARIANTS; do
    IFS=':' read -r label ref image_type extra <<< "$entry"
    bench_variant "$label" "$ref" "$image_type" "$extra"
    # Pause between variants so the system fully recovers
    sleep 5
  done

  summarize
  log "All done! Results in $RESULTS_DIR/"
}

main "$@"
