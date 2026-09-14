#!/bin/bash
#
# start.sh — the one command to run, on macOS or Linux, whether this is the
# very first run or the hundredth.
#
# Does a quick check for whether setup has already been done (does the SSH
# key exist and actually work against the router right now?):
#   - Not set up yet -> runs the full setup.sh (opens SSH, configures
#     notifications, cleans up telemetry, then launches the GUI).
#   - Already set up -> skips straight to launching the GUI, without
#     redoing the SSH/notification/cleanup steps every time.
#
set -e
cd "$(dirname "${BASH_SOURCE[0]}")"

ROUTER_IP="${ROUTER_IP:-192.168.31.1}"
KEY_PATH="gui/router_key"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -o ConnectTimeout=5 -o BatchMode=yes)

if [ -f "$KEY_PATH" ] && ssh "${SSH_OPTS[@]}" -i "$KEY_PATH" "root@$ROUTER_IP" true 2>/dev/null; then
  exec ./gui/start_gui.sh
else
  exec ./setup.sh
fi
