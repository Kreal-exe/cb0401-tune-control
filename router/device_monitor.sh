#!/bin/sh
#
# Checks /tmp/dhcp.leases for MAC addresses not in the whitelist and sends a
# push notification (via ntfy.sh or Telegram, whichever notify_common.sh's
# config selects) when it finds one. Meant to run from cron every few
# minutes (setup.sh installs it that way).
#
# Optional component — only needed if you want new-device push alerts.
#
DIR=/etc/crontabs/patches
WHITELIST="$DIR/known_macs.txt"
NOTIFIED="$DIR/notified_macs.txt"
LAST_SEEN="$DIR/last_seen.txt"

# shellcheck disable=SC1091
. "$DIR/notify_common.sh"

touch "$WHITELIST" "$NOTIFIED"

[ -f /tmp/dhcp.leases ] || exit 0

while read -r epoch mac ip host clientid; do
    [ -z "$mac" ] && continue
    mac_u=$(echo "$mac" | tr 'a-z' 'A-Z')
    grep -qiF "$mac_u" "$WHITELIST" && continue
    grep -qiF "$mac_u" "$NOTIFIED" && continue

    host_short="$host"
    [ "$host_short" = "*" ] && host_short="unnamed"

    notify "New device" "warning" "$mac_u | $ip | $host_short
Reply: trust / block / red alert"

    echo "$mac_u" >> "$NOTIFIED"
    printf '%s %s\n' "$mac_u" "$ip" > "$LAST_SEEN"
done < /tmp/dhcp.leases
