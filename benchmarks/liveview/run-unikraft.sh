#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT="$SCRIPT_DIR/../.."
RESULTS_DIR="$SCRIPT_DIR/results/ukc-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"

ITERATIONS="${ITERATIONS:-2}"
WARMUP="${WARMUP:-1}"
UKC_METRO="${UKC_METRO:-dal}"
CONCURRENT_WORKERS="${CONCURRENT_WORKERS:-3}"
CONCURRENT_DURATION="${CONCURRENT_DURATION:-20s}"
SKIP_CONCURRENT="${SKIP_CONCURRENT:-false}"

export KRAFTKIT_NO_CHECK_UPDATES=true
export UKC_METRO
if [[ -z "${UKC_TOKEN:-}" ]]; then
  export UKC_TOKEN=$(python3 -c "import yaml; print(yaml.safe_load(open('$HOME/.config/kraftcloud/config.yaml'))['users']['onkernel']['token'])")
fi

log() { echo "[ukc-bench] $(date +%H:%M:%S) $*"; }

# Variants: label:git-ref:image-type:vcpus:memory-mb
# image-type: headless or headful
VARIANTS="${VARIANTS:-baseline:main:headless:4:1024 approach1:feat/headless-live-view:headless:4:1024 approach2:headless-cdp-live-view:headless:4:1024 headful:main:headful:8:4096}"

build_bench() {
  log "Building benchmark binary..."
  cd "$REPO_ROOT/server"
  go build -o "$SCRIPT_DIR/bench" "$SCRIPT_DIR/main.go"
  cd "$SCRIPT_DIR"
  log "Benchmark binary built."
}

build_and_push_image() {
  local label="$1" ref="$2" image_type="$3"
  local ukc_image="onkernel/bench-${label}:latest"

  log "Building and pushing '$ukc_image' from ref '$ref'..."
  cd "$REPO_ROOT"

  local current_ref
  current_ref=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || git rev-parse HEAD)
  local stashed=false
  if ! git diff --quiet 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
    git stash push -m "ukc-bench-$label" >/dev/null 2>&1
    stashed=true
  fi
  git checkout "$ref" --quiet 2>/dev/null || git checkout "origin/$ref" --quiet

  if [[ "$image_type" == "headful" ]]; then
    local dockerfile="images/chromium-headful/Dockerfile"
    local kraftfile_dir="images/chromium-headful"
  else
    local dockerfile="images/chromium-headless/image/Dockerfile"
    local kraftfile_dir="images/chromium-headless/image"
  fi

  docker build --platform linux/amd64 -f "$dockerfile" -t "ukc-bench-${label}:latest" . 2>&1 | tail -5

  # Extract rootfs and build erofs
  local app_name="ukc-bench-${label}"
  docker rm -f "cnt-$app_name" 2>/dev/null || true
  docker create --platform linux/amd64 --name "cnt-$app_name" "ukc-bench-${label}:latest" /bin/sh
  rm -rf "$kraftfile_dir/.rootfs" || true
  docker cp "cnt-$app_name":/ "$kraftfile_dir/.rootfs"
  docker rm -f "cnt-$app_name" 2>/dev/null || true
  rm -f "$kraftfile_dir/initrd" || true
  log "Building erofs image (this may take a minute)..."
  mkfs.erofs --all-root -E noinline_data -b 4096 "$kraftfile_dir/initrd" "$kraftfile_dir/.rootfs" 2>/dev/null

  kraft pkg \
    --name "index.unikraft.io/$ukc_image" \
    --plat kraftcloud \
    --arch x86_64 \
    --strategy overwrite \
    --rootfs-type erofs \
    --push \
    "$kraftfile_dir"

  git checkout "$current_ref" --quiet 2>/dev/null || true
  if $stashed; then
    git stash pop --quiet 2>/dev/null || true
  fi

  cd "$SCRIPT_DIR"
  log "Image '$ukc_image' pushed."
}

deploy_instance() {
  local label="$1" image_type="$2" vcpus="$3" memory_mb="$4"
  local ukc_image="onkernel/bench-${label}:latest"
  local inst_name="bench-${label}"

  kraft cloud inst rm "$inst_name" 2>/dev/null || true

  local deploy_args=(
    --start
    --vcpus "$vcpus"
    -M "$memory_mb"
    -p 9222:9222/tls
    -p 444:10001/tls
    -e RUN_AS_ROOT=false
    -e LOG_CDP_MESSAGES=false
    -n "$inst_name"
  )

  if [[ "$image_type" == "headful" ]]; then
    deploy_args+=(
      -e DISPLAY_NUM=1
      -e HEIGHT=1080
      -e WIDTH=1920
      -e ENABLE_WEBRTC=false
    )
  fi

  log "Deploying $inst_name (vcpus=$vcpus, mem=${memory_mb}MB, type=$image_type)..." >&2
  kraft cloud inst create "${deploy_args[@]}" "$ukc_image" >&2

  sleep 5

  # Get FQDN - only print the FQDN to stdout, everything else to stderr
  local fqdn
  fqdn=$(kraft cloud inst get "$inst_name" -o json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['fqdn'])" 2>/dev/null || echo "")
  if [[ -z "$fqdn" ]]; then
    log "ERROR: Could not get FQDN for $inst_name" >&2
    return 1
  fi
  log "Instance FQDN: $fqdn" >&2
  echo "$fqdn"
}

wait_for_cdp() {
  local fqdn="$1"
  local url="https://${fqdn}:9222/json/version"
  log "Waiting for CDP at $url..."
  local attempts=0
  while ! curl -sf "$url" >/dev/null 2>&1; do
    sleep 2
    attempts=$((attempts + 1))
    if (( attempts > 60 )); then
      log "ERROR: CDP not ready after 120s"
      return 1
    fi
  done
  log "CDP ready after $((attempts * 2))s."
}

bench_variant() {
  local label="$1" ref="$2" image_type="$3" vcpus="$4" memory_mb="$5"
  local variant_dir="$RESULTS_DIR/$label"
  mkdir -p "$variant_dir"

  log "========================================"
  log "  Benchmarking: $label (Unikraft)"
  log "  Ref: $ref | Type: $image_type | vCPUs: $vcpus | Mem: ${memory_mb}MB"
  log "========================================"

  # Build and push image
  build_and_push_image "$label" "$ref" "$image_type"

  # Deploy
  local fqdn
  fqdn=$(deploy_instance "$label" "$image_type" "$vcpus" "$memory_mb")

  # Record resource config
  cat > "$variant_dir/memory.txt" <<EOF
config_memory: ${memory_mb}MB
config_vcpus: $vcpus
image_type: $image_type
platform: unikraft
metro: $UKC_METRO
fqdn: $fqdn
EOF

  # Wait for CDP and settle
  wait_for_cdp "$fqdn"
  log "Settling for 15s..."
  sleep 15

  local cdp_url="https://${fqdn}:9222"
  local bench_args=(
    -cdp-url "$cdp_url"
    -session-mode
    -iterations "$ITERATIONS"
    -warmup "$WARMUP"
    -label "$label"
    -concurrent-workers "$CONCURRENT_WORKERS"
    -concurrent-duration "$CONCURRENT_DURATION"
  )
  if [[ "$SKIP_CONCURRENT" == "true" ]]; then
    bench_args+=("-skip-concurrent")
  fi

  log "Running CDP benchmark (iterations=$ITERATIONS, warmup=$WARMUP, session-mode)..."
  "$SCRIPT_DIR/bench" "${bench_args[@]}" \
    > "$variant_dir/results.md" 2> "$variant_dir/bench.log" || true

  "$SCRIPT_DIR/bench" "${bench_args[@]}" -json \
    > "$variant_dir/results.json" 2> "$variant_dir/bench-json.log" || true

  # Save instance info
  kraft cloud inst get "bench-${label}" -o json > "$variant_dir/instance.json" 2>/dev/null || true

  # Tear down
  kraft cloud inst rm "bench-${label}" 2>/dev/null || true

  log "Variant '$label' complete. Results in $variant_dir/"
}

summarize() {
  local summary_file="$RESULTS_DIR/SUMMARY.md"
  cat > "$summary_file" <<EOF
# Unikraft Live View Benchmark Results

Date: $(date)
Metro: $UKC_METRO
Iterations: $ITERATIONS (warmup: $WARMUP)

EOF

  for variant_dir in "$RESULTS_DIR"/*/; do
    local label=$(basename "$variant_dir")
    [[ "$label" == "SUMMARY.md" ]] && continue
    echo "## $label" >> "$summary_file"
    echo "" >> "$summary_file"
    if [[ -f "$variant_dir/results.md" ]]; then
      cat "$variant_dir/results.md" >> "$summary_file"
    fi
    echo "" >> "$summary_file"
    echo "### $label — Config" >> "$summary_file"
    echo '```' >> "$summary_file"
    cat "$variant_dir/memory.txt" >> "$summary_file"
    echo '```' >> "$summary_file"
    echo "" >> "$summary_file"
  done

  # Side-by-side comparison
  echo "## Side-by-side Comparison (Median)" >> "$summary_file"
  echo "" >> "$summary_file"
  python3 "$SCRIPT_DIR/compare.py" "$RESULTS_DIR" >> "$summary_file" 2>/dev/null || echo "(comparison script failed)" >> "$summary_file"

  log "Summary: $summary_file"
}

# ---- Main ----
log "Unikraft Live View Benchmark"
log "Results dir: $RESULTS_DIR"
log "Variants: $VARIANTS"
log "Metro: $UKC_METRO"

build_bench

for entry in $VARIANTS; do
  IFS=':' read -r label ref image_type vcpus memory_mb <<< "$entry"
  bench_variant "$label" "$ref" "$image_type" "$vcpus" "$memory_mb" || \
    log "WARNING: Variant '$label' failed, continuing..."
  sleep 5
done

summarize
log "All done! Results in $RESULTS_DIR/"
