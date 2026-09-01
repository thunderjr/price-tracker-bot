#!/bin/sh
# Chromium must run headful to get past Mercado Livre's anti-bot page, so give
# it a virtual display to draw into.
set -e

DISPLAY_NUM="${DISPLAY#:}"
rm -f "/tmp/.X${DISPLAY_NUM}-lock"

Xvfb "$DISPLAY" -screen 0 1440x900x24 -nolisten tcp -noreset &
XVFB_PID=$!
trap 'kill "$XVFB_PID" 2>/dev/null || true' EXIT INT TERM

# Wait for the display before handing over, otherwise the first Chromium launch
# races Xvfb and dies with "unable to open X display".
i=0
while [ ! -e "/tmp/.X11-unix/X${DISPLAY_NUM}" ]; do
  i=$((i + 1))
  if [ "$i" -gt 100 ]; then
    echo "entrypoint: Xvfb failed to start on $DISPLAY" >&2
    exit 1
  fi
  sleep 0.1
done

exec "$@"
