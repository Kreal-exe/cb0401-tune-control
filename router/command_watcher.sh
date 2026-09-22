#!/bin/sh
#
# Polls for commands sent back by the user (via ntfy.sh or Telegram,
# whichever notify_common.sh's config selects) and acts on them:
#   "trust"                — adds the last device seen to the whitelist (and
#                             lifts its block, if it had one).
#   "<MAC> trust" / "<IP> trust" — same, for a specific device.
#   "block"                — blocks the last device seen by device_monitor.sh
#   "<MAC> block" / "<IP> block" (either order) — blocks a specific device.
#                             If an IP is given, its current MAC is looked up
#                             in dhcp.leases and the block is applied by MAC
#                             (survives the device getting a new IP later);
#                             falls back to blocking by IP only if no MAC can
#                             be resolved.
#   "red alert"             — Wi-Fi MAC allow-list: only whitelisted devices
#                             can associate. This briefly drops ALL clients
#                             (including trusted ones) for a few seconds —
#                             it requires a full radio reload on this
#                             platform, there's no way around that.
#   "all clear"             — removes the allow-list, back to normal.
#
# Modes:
#   --daemon   long-lived listener: Telegram long polling (getUpdates with a
#              50s server-side wait) or an open ntfy stream, so a reply is
#              handled within about a second. The connection sits idle in
#              between, so there's no polling load on the router.
#   --ensure   start the daemon if it isn't running (cron runs this every
#              minute as a watchdog - it also brings the daemon back after a
#              reboot, since /tmp and the pid file don't survive one).
#   --stop     stop the daemon.
#   (none)     one-shot poll, the old behaviour; does nothing while the
#              daemon is alive.
#
# Optional component — only needed if you want to react to push
# notifications from your phone.
#
DIR=/etc/crontabs/patches
WHITELIST="$DIR/known_macs.txt"
LAST_SEEN="$DIR/last_seen.txt"
PIDFILE=/tmp/command_watcher.pid

# shellcheck disable=SC1091
. "$DIR/notify_common.sh"

mkdir -p "$DIR"
touch "$WHITELIST"

resolve_mac_from_ip() {
    grep -i " $1 " /tmp/dhcp.leases 2>/dev/null | awk '{print $2}' | tr 'a-z' 'A-Z'
}

block_mac() {
    mac="$1"
    rule="block_$(echo "$mac" | tr ':' '_')"
    uci -q delete firewall."$rule" 2>/dev/null
    uci set firewall."$rule"="rule"
    uci set firewall."$rule".name="Block $mac"
    uci set firewall."$rule".src='lan'
    uci set firewall."$rule".src_mac="$mac"
    uci set firewall."$rule".target='REJECT'
    uci commit firewall
    /etc/init.d/firewall reload >/dev/null 2>&1
    notify "Blocked" "no_entry_sign" "$mac"
}

block_ip_only() {
    ip="$1"
    rule="block_$(echo "$ip" | tr '.' '_')"
    uci -q delete firewall."$rule" 2>/dev/null
    uci set firewall."$rule"="rule"
    uci set firewall."$rule".name="Block $ip"
    uci set firewall."$rule".src='lan'
    uci set firewall."$rule".src_ip="$ip"
    uci set firewall."$rule".target='REJECT'
    uci commit firewall
    /etc/init.d/firewall reload >/dev/null 2>&1
    notify "Blocked (by IP)" "no_entry_sign" "$ip - no MAC found, this will not survive an IP change"
}

trust_mac() {
    mac="$1"
    # The GUI writes this file without a guaranteed trailing newline; make
    # sure our append starts on its own line.
    if [ -s "$WHITELIST" ] && [ -n "$(tail -c1 "$WHITELIST")" ]; then
        echo >> "$WHITELIST"
    fi
    if grep -qiF "$mac" "$WHITELIST"; then
        note="already on the whitelist"
    else
        echo "$mac" >> "$WHITELIST"
        note="added to the whitelist"
    fi

    # If this device had been blocked earlier, trusting it should undo that.
    rule="block_$(echo "$mac" | tr ':' '_')"
    if uci -q get firewall."$rule" >/dev/null 2>&1; then
        uci -q delete firewall."$rule"
        uci commit firewall
        /etc/init.d/firewall reload >/dev/null 2>&1
        note="$note, block removed"
    fi

    # During a red alert the Wi-Fi allow-list is a copy of the whitelist
    # taken when it was switched on, so refresh it (briefly drops clients,
    # same as red alert itself).
    if [ "$(uci -q get wireless.@wifi-iface[0].macfilter)" = "allow" ]; then
        apply_macfilter allow
        note="$note, red-alert allow-list refreshed"
    fi

    notify "Trusted" "white_check_mark" "$mac - $note"
}

apply_macfilter() {
    mode="$1"  # allow | disable
    for idx in 0 1; do
        if [ "$mode" = "allow" ]; then
            uci set wireless.@wifi-iface[$idx].macfilter='allow'
            uci -q delete wireless.@wifi-iface[$idx].maclist
            while IFS= read -r mac; do
                [ -z "$mac" ] && continue
                uci add_list wireless.@wifi-iface[$idx].maclist="$mac"
            done < "$WHITELIST"
        else
            uci -q delete wireless.@wifi-iface[$idx].macfilter
            uci -q delete wireless.@wifi-iface[$idx].maclist
        fi
    done
    uci commit wireless
    wifi reload > /dev/null 2>&1 &
}

# Runs the actual command once its text has been pulled out of whichever
# backend delivered it.
handle_message() {
    msg="$1"

    if echo "$msg" | grep -qi 'red[[:space:]]*alert'; then
        apply_macfilter allow
        notify "Red alert" "rotating_light" "Only whitelisted devices allowed. Reply 'all clear' to undo."
        return
    fi

    if echo "$msg" | grep -qi 'all[[:space:]]*clear'; then
        apply_macfilter disable
        notify "All clear" "white_check_mark" "Wi-Fi is back to normal."
        return
    fi

    if echo "$msg" | grep -qiw 'trust'; then
        mac=$(echo "$msg" | grep -oE '([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}' | head -1 | tr 'a-z' 'A-Z')
        ip=$(echo "$msg" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)

        if [ -z "$mac" ] && [ -z "$ip" ] && [ -s "$LAST_SEEN" ]; then
            # bare "trust" -> last device seen by the new-device alert
            mac=$(awk '{print $1}' "$LAST_SEEN")
        fi
        if [ -z "$mac" ] && [ -n "$ip" ]; then
            mac=$(resolve_mac_from_ip "$ip")
        fi

        if [ -n "$mac" ]; then
            trust_mac "$mac"
        else
            notify "Trust failed" "warning" "No MAC found for that request - use the MAC address directly."
        fi
        return
    fi

    if echo "$msg" | grep -qi 'block'; then
        mac=$(echo "$msg" | grep -oE '([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}' | head -1 | tr 'a-z' 'A-Z')
        ip=$(echo "$msg" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)

        if [ -z "$mac" ] && [ -z "$ip" ]; then
            # bare "block" -> last device seen by device_monitor.sh
            if [ -s "$LAST_SEEN" ]; then
                mac=$(awk '{print $1}' "$LAST_SEEN")
            fi
        fi

        if [ -z "$mac" ] && [ -n "$ip" ]; then
            mac=$(resolve_mac_from_ip "$ip")
        fi

        if [ -n "$mac" ]; then
            block_mac "$mac"
        elif [ -n "$ip" ]; then
            block_ip_only "$ip"
        fi
    fi
}

daemon_pid() {
    [ -f "$PIDFILE" ] || return 1
    pid=$(cat "$PIDFILE" 2>/dev/null)
    [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && grep -q command_watcher "/proc/$pid/cmdline" 2>/dev/null || return 1
    echo "$pid"
}

poll_ntfy() {
    STATE="$DIR/ntfy_since.txt"
    RESP="$DIR/ntfy_resp.json"
    [ -f "$STATE" ] || echo $(($(date +%s) - 300)) > "$STATE"
    SINCE=$(cat "$STATE")

    # Fetch to a file and check curl's own exit status first — piping
    # straight into `while read` would hide a failed request behind the
    # loop's exit code, and we'd advance $STATE past commands we never
    # actually saw (permanently losing them, since we never re-fetch a
    # range once we've moved past it).
    if curl -s -m 15 -o "$RESP" "https://ntfy.sh/$NTFY_TOPIC/json?since=$SINCE&poll=1"; then
        # The trailing `echo` guarantees the last line ends in a newline:
        # `read` in dash/ash silently DROPS a final line that isn't
        # newline-terminated (its exit status signals EOF, so the loop body
        # never runs for it) - without this, a response with no trailing
        # newline would lose exactly the one message that actually matters
        # when there's only a single result.
        (cat "$RESP"; echo) | while IFS= read -r line; do
            handle_ntfy_line "$line"
        done

        date +%s > "$STATE"
    fi
    rm -f "$RESP"
}

# One line of ntfy's JSON stream -> command, if it is one.
handle_ntfy_line() {
    line="$1"
    [ -z "$line" ] && return
    msg=$(echo "$line" | sed -n 's/.*"message":"\([^"]*\)".*/\1/p')
    [ -z "$msg" ] && return

    # ntfy is a shared pub/sub topic, so our own outgoing
    # notifications come back to us too — skip them by their exact
    # title (not by loosely matching body text, which would also
    # match the user's own "red alert" command and cause it to be
    # ignored).
    title=$(echo "$line" | sed -n 's/.*"title":"\([^"]*\)".*/\1/p')
    case "$title" in
        "New device"|"Blocked"|"Blocked (by IP)"|"Trusted"|"Trust failed"|"Red alert"|"All clear"|"Welcome")
            return ;;
    esac

    handle_message "$msg"
}

# Daemon flavour of poll_ntfy: keeps the request open and handles each
# event the moment it arrives. Reconnects every ~55s so a changed backend or
# topic in notify.conf is picked up. The resume point is the event's own
# timestamp + 1 (a command sent in the same second as the previous one could
# be missed on a reconnect - not a real concern for a human typing replies).
poll_ntfy_stream() {
    STATE="$DIR/ntfy_since.txt"
    [ -f "$STATE" ] || echo $(($(date +%s) - 300)) > "$STATE"
    SINCE=$(cat "$STATE")

    curl -s -N -m 55 "https://ntfy.sh/$NTFY_TOPIC/json?since=$SINCE" | while IFS= read -r line; do
        handle_ntfy_line "$line"
        t=$(echo "$line" | sed -n 's/.*"time":\([0-9]*\).*/\1/p')
        [ -n "$t" ] && echo $((t + 1)) > "$STATE"
    done
}

poll_telegram() {
    STATE="$DIR/telegram_offset.txt"
    RESP="$DIR/telegram_resp.json"
    [ -f "$STATE" ] || echo 0 > "$STATE"
    OFFSET=$(cat "$STATE")
    # Server-side wait (0 = answer immediately, the one-shot behaviour; the
    # daemon sets 50 so the request stays open until a message arrives).
    WAIT="${TG_WAIT:-0}"
    rc=1

    if curl -s -m "${TG_CURL_MAX:-15}" -o "$RESP" \
        "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getUpdates?offset=${OFFSET}&timeout=${WAIT}"; then
        rc=0
        MAX_UPDATE_ID=""
        # Telegram's getUpdates response is a single JSON blob, but (unlike
        # ntfy's newline-delimited stream) it isn't fully compact: it
        # inserts a real newline right after each "update_id" field, which
        # would otherwise cut update_id and the rest of that same update's
        # fields (chat id, text) onto two separate `read` lines. `tr -d`
        # collapses those away first, then our own sed re-introduces a
        # newline only where we actually want one - before each
        # "update_id" - so each full update ends up on exactly one line.
        # The trailing `echo` guarantees the last line ends in a newline
        # too: `read` in dash/ash silently DROPS a final line that isn't
        # newline-terminated (its exit status signals EOF, so the loop body
        # never runs for it) - without this, curl's response (which itself
        # has no trailing newline) would lose exactly the one update that
        # actually matters when there's only a single result.
        (tr -d '\n' < "$RESP"; echo) | sed 's/{"update_id"/\n{"update_id"/g' | while IFS= read -r line; do
            update_id=$(echo "$line" | sed -n 's/.*"update_id":\([0-9]*\).*/\1/p')
            [ -z "$update_id" ] && continue

            chat_id=$(echo "$line" | sed -n 's/.*"chat":{"id":\(-\{0,1\}[0-9]*\).*/\1/p')
            msg=$(echo "$line" | sed -n 's/.*"text":"\([^"]*\)".*/\1/p')

            echo "$update_id" >> "$DIR/.telegram_last_update_id"

            # A bot never receives its own outgoing sendMessage calls back
            # through getUpdates, so (unlike ntfy) there's no self-message
            # loop to filter out here. We do still only trust the one chat
            # this was set up for - anyone else who somehow messages the
            # bot is silently ignored.
            if [ -n "$msg" ] && [ "$chat_id" = "$TELEGRAM_CHAT_ID" ]; then
                handle_message "$msg"
            fi
        done

        MAX_UPDATE_ID=$(sort -n "$DIR/.telegram_last_update_id" 2>/dev/null | tail -1)
        rm -f "$DIR/.telegram_last_update_id"
        if [ -n "$MAX_UPDATE_ID" ]; then
            echo $((MAX_UPDATE_ID + 1)) > "$STATE"
        fi
    fi
    rm -f "$RESP"
    return $rc
}

run_daemon() {
    echo $$ > "$PIDFILE"
    trap 'rm -f "$PIDFILE"; exit 0' TERM INT
    while :; do
        # Re-read the backend config every round so a change made from the
        # GUI takes effect within a minute, without restarting anything.
        # shellcheck disable=SC1091
        . "$DIR/notify_common.sh"
        case "$NOTIFY_BACKEND" in
            telegram)
                TG_WAIT=50
                TG_CURL_MAX=60
                poll_telegram || sleep 5
                sleep 1
                ;;
            *)
                started=$(date +%s)
                poll_ntfy_stream
                # A stream that dies at once (no network) shouldn't spin.
                [ $(($(date +%s) - started)) -lt 3 ] && sleep 5
                ;;
        esac
    done
}

stop_daemon() {
    pid=$(daemon_pid) || return 0
    # The long-poll curl (and ntfy's pipeline) are children; stop them too or
    # they linger until their own timeout.
    for c in $(pgrep -P "$pid"); do kill "$c" 2>/dev/null; done
    kill "$pid" 2>/dev/null
    rm -f "$PIDFILE"
}

case "$1" in
    --daemon) run_daemon ;;
    --stop)   stop_daemon ;;
    --restart)
        stop_daemon
        sh "$0" --ensure
        ;;
    --ensure)
        # flock makes the check-then-launch atomic: the cron watchdog and a
        # just-finished --restart (or two overlapping cron ticks) can both
        # reach this within the same second, and without a lock both would
        # see "not running" and each start their own daemon - confirmed
        # live, this really happens, not just a theoretical race.
        (
            flock -n 9 || exit 0
            daemon_pid >/dev/null && exit 0
            # Detached (own session if setsid exists) so it outlives this cron job.
            if command -v setsid >/dev/null 2>&1; then
                setsid sh "$0" --daemon >/dev/null 2>&1 &
            else
                sh "$0" --daemon >/dev/null 2>&1 &
            fi
        ) 9>/tmp/command_watcher.lock
        ;;
    *)
        # One-shot poll (manual use). If the daemon is running it already
        # owns the Telegram update stream - a second getUpdates caller would
        # just fight it.
        daemon_pid >/dev/null && exit 0
        case "$NOTIFY_BACKEND" in
            telegram) poll_telegram ;;
            *)        poll_ntfy ;;
        esac
        ;;
esac
