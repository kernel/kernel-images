#!/bin/sh
set -eu

if [ "${DISPLAY_BACKEND:-x11}" = "wayland" ]; then
    export NEKO_DESKTOP_WAYLAND=true
    export NEKO_CAPTURE_VIDEO_WAYLAND=true
    export NEKO_CAPTURE_VIDEO_WAYLAND_RECORDER=/usr/local/bin/weston-capture
fi

exec /usr/bin/neko serve --server.static /var/www --server.bind 0.0.0.0:8080
