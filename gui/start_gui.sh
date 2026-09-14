#!/bin/bash
#
# Builds (if needed) and starts the router admin GUI, then opens it in the
# browser. No Python, no venv, no pip - just a Go binary.
#
# Usage: ./start_gui.sh
#
set -e
cd "$(dirname "$0")"

BIN="./cb0401-tune-control"
PORT=5757
PIDFILE="/tmp/cb0401_tune_control_gui.pid"

if [ ! -x "$BIN" ]; then
  command -v go >/dev/null 2>&1 || {
    echo "ERROR: no compiled GUI found at $BIN, and 'go' isn't installed to build one."
    echo "Either install Go (https://go.dev/dl/) and re-run this script, or copy a"
    echo "prebuilt binary from dist/ (see build.sh) to $BIN."
    exit 1
  }
  echo "Building the GUI (one-time)..."
  go build -o "$BIN" .
fi

# If something is already listening on the port (e.g. a stale process from
# a previous run), stop it first so we don't hit "Address already in use"
# or end up talking to old code.
if [ -f "$PIDFILE" ]; then
  OLDPID=$(cat "$PIDFILE" 2>/dev/null || true)
  if [ -n "$OLDPID" ] && kill -0 "$OLDPID" 2>/dev/null; then
    echo "Stopping previous instance (PID $OLDPID)..."
    kill "$OLDPID" 2>/dev/null || true
    sleep 1
    kill -9 "$OLDPID" 2>/dev/null || true
  fi
fi
LSOF_PID=$(lsof -ti tcp:$PORT 2>/dev/null || true)
if [ -n "$LSOF_PID" ]; then
  echo "Port $PORT is in use (PID $LSOF_PID) — freeing it..."
  kill -9 $LSOF_PID 2>/dev/null || true
fi

echo "Starting the GUI on http://127.0.0.1:$PORT ..."
"$BIN" > /tmp/cb0401_tune_control_gui.log 2>&1 &
BIN_PID=$!
echo $BIN_PID > "$PIDFILE"

# wait until the server actually comes up before opening the browser
for i in $(seq 1 20); do
  if curl -s -o /dev/null "http://127.0.0.1:$PORT/"; then
    break
  fi
  sleep 0.3
done

open "http://127.0.0.1:$PORT" 2>/dev/null || xdg-open "http://127.0.0.1:$PORT" 2>/dev/null || true

# Stay attached to the GUI process instead of returning to the shell prompt
# right away - closing this terminal or hitting Ctrl+C stops the GUI too,
# rather than leaving it running invisibly in the background.
echo "GUI is running (PID $BIN_PID), log: /tmp/cb0401_tune_control_gui.log"
echo "Press Ctrl+C to stop it."
trap 'kill "$BIN_PID" 2>/dev/null; exit 0' INT TERM
wait "$BIN_PID"
