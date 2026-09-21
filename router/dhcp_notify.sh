#!/bin/sh
#
# Instant new-device notification, event-driven instead of polled: wired up
# as dnsmasq's own --dhcp-script hook (via `dhcp.@dnsmasq[0].dhcpscript` in
# UCI - see setup.sh/.ps1), so this only runs once per actual DHCP lease
# event rather than scanning /tmp/dhcp.leases on a timer. Zero added load
# between events, and no notification delay waiting for the next tick.
#
# IMPORTANT: dnsmasq's own wrapper (/usr/lib/dnsmasq/dhcp-script.sh) invokes
# this by SOURCING it (". \"$USER_DHCPSCRIPT\" \"$@\""), not executing it, so
# it can run inside the same shell that still needs to fire dnsmasq's own
# hotplug.dhcp ubus event afterward for anything else listening. Never
# `exit` here - that would kill the wrapper before it gets to do that. Use
# `return` for every early-out instead (confirmed live that `return` in a
# sourced script only unwinds the source, not the caller).
#
# Args, dnsmasq's own dhcp-script convention: $1=action(add/old/del) $2=mac
# $3=ip $4=hostname (empty or "*" if the client didn't send one).
#
[ "$1" = "add" ] || return 0

DIR=/etc/crontabs/patches
WHITELIST="$DIR/known_macs.txt"
NOTIFIED="$DIR/notified_macs.txt"
LAST_SEEN="$DIR/last_seen.txt"

# shellcheck disable=SC1091
. "$DIR/notify_common.sh"

touch "$WHITELIST" "$NOTIFIED"

mac_u=$(echo "$2" | tr 'a-z' 'A-Z')
[ -n "$mac_u" ] || return 0
grep -qiF "$mac_u" "$WHITELIST" && return 0
grep -qiF "$mac_u" "$NOTIFIED" && return 0

host_short="$4"
[ -z "$host_short" ] || [ "$host_short" = "*" ] && host_short="unnamed"

notify "New device" "warning" "$mac_u | $3 | $host_short
Reply: trust / block / red alert"

echo "$mac_u" >> "$NOTIFIED"
printf '%s %s\n' "$mac_u" "$3" > "$LAST_SEEN"

# The reply to this alert ("trust" / "block" / ...) is handled by the
# long-polling listener in command_watcher.sh. Make sure it's up right now
# rather than waiting for its once-a-minute cron watchdog. Backgrounded and
# silenced so it can't hold up dnsmasq.
( sh "$DIR/command_watcher.sh" --ensure >/dev/null 2>&1 & )
