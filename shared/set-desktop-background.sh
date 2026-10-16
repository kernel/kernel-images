#!/bin/bash
# Paint the X root window with a blue gradient so the desktop behind the browser
# is never blank. Falls back to a solid mid-gradient via xsetroot when ImageMagick
# or the geometry probe is unavailable. Runs with a timeout so a wedged X call
# can never stall wrapper startup past openbox.
set -u

# Per-stage bound. BG_TIMEOUT exists so tests can shrink it.
export TIMEOUT="${BG_TIMEOUT:-10}"
export FALLBACK="#18436d" # solid mid-gradient, matches the gradient at mid-screen

run_bg() {
    timeout "$TIMEOUT" "$@"
}

# Paint at the root window's true size. The X screen (RANDR) can be smaller
# than the root window at boot (the dummy DDX starts at its max mode), and a
# smaller pixmap tiles across the root.
main() {
    # Solid base first: display's exit code is unreliable (it can paint the
    # gradient and still exit nonzero), so the fallback must not be able to
    # clobber a successful gradient paint.
    run_bg xsetroot -solid "$FALLBACK"

    BG="$(mktemp /tmp/desktop-background.XXXXXX.png)" || return 0
    trap 'rm -f "$BG"' EXIT

    # The probe gets the same per-stage bound as everything else: a stalled
    # geometry probe must not hold up wrapper startup past the fallback.
    SIZE="$(run_bg xwininfo -root 2>/dev/null | awk '/^  Width:/{w=$2} /^  Height:/{h=$2} END{print w "x" h}')"
    SIZE="${SIZE:-1920x1080}"

    if run_bg convert -size "$SIZE" gradient:'#123250'-'#1a4a79' "$BG"; then
        # display may exit nonzero after a successful paint; the solid base
        # painted above covers the real failure case.
        run_bg display -window root "$BG" \
            || echo "set-desktop-background: display exited nonzero (paint may still have landed)" >&2
    fi
}

# timeout execs a binary, so main and run_bg have to be exported into a child
# bash. Every stage is individually bounded, and the whole operation is bounded
# outright so a pathologically slow environment cannot stall wrapper startup
# no matter which stage wedges.
export -f run_bg main
timeout "$((TIMEOUT * 3 + 5))" bash -c main
exit 0
