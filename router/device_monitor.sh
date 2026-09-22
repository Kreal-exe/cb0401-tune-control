#!/bin/sh
#
# One-shot baseline scan of /tmp/dhcp.leases, run once by setup.sh right
# after the dnsmasq hook (dhcp_notify.sh) is wired up - see its own header
# comment for why this only runs once now rather than on a cron timer. It
# marks whatever's already connected at that point as "recently seen" so
# those devices don't immediately look like a fresh reconnect to the hook
# the moment it goes live, without ever sending a notification of its own.
#
# Also usable any time as a manual re-scan (e.g. `sh device_monitor.sh`
# over SSH) if you want to re-mark everything currently connected as seen.
#
DIR=/etc/crontabs/patches
WHITELIST="$DIR/known_macs.txt"
NOTIFIED="$DIR/notified_macs.txt"

touch "$WHITELIST" "$NOTIFIED"

[ -f /tmp/dhcp.leases ] || exit 0

now=$(date +%s)
while read -r epoch mac ip host clientid; do
    [ -z "$mac" ] && continue
    mac_u=$(echo "$mac" | tr 'a-z' 'A-Z')
    grep -qiF "$mac_u" "$WHITELIST" && continue

    # Same "MAC timestamp" format dhcp_notify.sh reads (see its header
    # comment for why this is a time window, not a permanent mark).
    grep -v "^$mac_u " "$NOTIFIED" >"$NOTIFIED.tmp" 2>/dev/null
    mv "$NOTIFIED.tmp" "$NOTIFIED" 2>/dev/null || : >"$NOTIFIED"
    echo "$mac_u $now" >>"$NOTIFIED"
done </tmp/dhcp.leases
