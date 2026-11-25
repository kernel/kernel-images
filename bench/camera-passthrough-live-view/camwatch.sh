#!/bin/bash
# Publishes whether a page in Chrome is using the camera (prototype).
STATE=/var/www/camera-state.json
last=""
while true; do
  chrome=false
  for p in $(pgrep -u kernel chrom); do
    if ls -l /proc/$p/fd 2>/dev/null | grep -q '/dev/video0'; then chrome=true; break; fi
  done
  s="{\"wanted\":$chrome}"
  if [ "$s" != "$last" ]; then echo "$s" > $STATE.tmp && mv $STATE.tmp $STATE; chmod 644 $STATE; last="$s"; echo "$(date +%T) $s"; fi
  sleep 0.3
done
