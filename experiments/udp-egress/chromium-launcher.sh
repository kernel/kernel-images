#!/bin/sh
set -eu
while [ ! -f /run/udp-egress-ready ]; do sleep 0.1; done
exec ip netns exec browser-udp /usr/local/bin/chromium-launcher "$@"
