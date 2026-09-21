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
# Meant to run from cron every couple of minutes (setup.sh installs it that
# way). Optional component — only needed if you want to react to push
# notifications from your phone.
#
DIR=/etc/crontabs/patches
WHITELIST="$DIR/known_macs.txt"
LAST_SEEN="$DIR/last_seen.txt"

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
            [ -z "$line" ] && continue
            msg=$(echo "$line" | sed -n 's/.*"message":"\([^"]*\)".*/\1/p')
            [ -z "$msg" ] && continue

            # ntfy is a shared pub/sub topic, so our own outgoing
            # notifications come back to us too — skip them by their exact
            # title (not by loosely matching body text, which would also
            # match the user's own "red alert" command and cause it to be
            # ignored).
            title=$(echo "$line" | sed -n 's/.*"title":"\([^"]*\)".*/\1/p')
            case "$title" in
                "New device"|"Blocked"|"Blocked (by IP)"|"Trusted"|"Trust failed"|"Red alert"|"All clear"|"Welcome")
                    continue ;;
            esac

            handle_message "$msg"
        done

        date +%s > "$STATE"
    fi
    rm -f "$RESP"
}

poll_telegram() {
    STATE="$DIR/telegram_offset.txt"
    RESP="$DIR/telegram_resp.json"
    [ -f "$STATE" ] || echo 0 > "$STATE"
    OFFSET=$(cat "$STATE")

    if curl -s -m 15 -o "$RESP" \
        "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getUpdates?offset=${OFFSET}&timeout=0"; then
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
}

case "$NOTIFY_BACKEND" in
    telegram) poll_telegram ;;
    *)        poll_ntfy ;;
esac
