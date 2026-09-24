#!/bin/sh
#
# Forwards incoming SMS to your notification backend (ntfy.sh/Telegram).
#
# The router's stock SMS handling (/usr/sbin/mobile) is compiled/encrypted
# Lua we can't hook into directly, and there's no hotplug/ubus event for
# "new SMS" the way dnsmasq gives us one for DHCP leases (see
# dhcp_notify.sh) - so this polls, unlike that one. It's kept cheap enough
# not to matter: every few seconds, one ~1.7MB static binary (sms-reader,
# see router/sms-reader/) does a plain read of the small SQLite file the
# stock daemon already maintains (/data/etc/mobile/xqSMS.db) - no network
# call, no AT port, nothing that could race with anything else this
# toolkit does. There's no sqlite3 CLI on this router (and adding one would
# mean a multi-MB dependency or a foreign binary's libc against this
# firmware's musl/uClibc userland - see sms-reader's own doc comment for
# why a tiny purpose-built reader made more sense).
#
# Modes: same --daemon/--ensure/--stop/--restart convention as
# command_watcher.sh - a persistent loop, a once-a-minute cron watchdog
# that also revives it after a reboot, one-shot manual use otherwise.
#
DIR=/etc/crontabs/patches
BIN="$DIR/sms-reader"
DB=/data/etc/mobile/xqSMS.db
STATE="$DIR/sms_last_id.txt"
REPLY_MAP="$DIR/sms_reply_map.txt"
PIDFILE=/tmp/sms_notify.pid
POLL_SECONDS=4

# shellcheck disable=SC1091
. "$DIR/notify_common.sh"

# sms-reader escapes \, tab, \n and \r in the content/phone fields so each
# row fits on one tab-separated output line (see its own doc comment) -
# undo that before putting the text in an actual notification.
unescape() {
    printf '%s' "$1" | sed 's/\\n/\n/g; s/\\t/\t/g; s/\\r/\r/g; s/\\\\/\\/g'
}

check_once() {
    [ "$SMS_FORWARD" = "0" ] && return 0
    [ -f "$DB" ] || return 0
    [ -x "$BIN" ] || return 0

    last=0
    [ -f "$STATE" ] && last=$(cat "$STATE")
    case "$last" in '' | *[!0-9]*) last=0 ;; esac

    out=$("$BIN" "$DB" "$last" 2>/tmp/sms_reader_err.log) || {
        # Fails closed: log and skip this round rather than guess at
        # something we couldn't actually parse. A transient failure (e.g.
        # caught mid-write) self-heals on the next poll rather than
        # retrying the same read in a tight loop.
        return 0
    }
    [ -z "$out" ] && return 0

    printf '%s\n' "$out" | while IFS="$(printf '\t')" read -r id _state _ts phone content; do
        [ -z "$id" ] && continue
        phone_clean=$(unescape "$phone")
        content_clean=$(unescape "$content")
        if notify "SMS from ${phone_clean:-unknown}" "envelope" "$content_clean"; then
            echo "$id" >"$STATE"
            # Remember which Telegram message this SMS became, so
            # command_watcher.sh can tell a reply to it apart from an
            # ordinary command and send it back to this same number - see
            # its send_sms_reply(). Only meaningful for a real numeric
            # sender (an alphanumeric SMSC alias like "Telekom" can't
            # receive an SMS back) and only on Telegram (ntfy has no
            # reply-to-a-specific-message of its own to hook into).
            if [ "$NOTIFY_BACKEND" = "telegram" ] && [ -n "$NOTIFY_LAST_MSGID" ] && [ -n "$phone_clean" ]; then
                echo "$NOTIFY_LAST_MSGID $phone_clean" >>"$REPLY_MAP"
                tail -n 200 "$REPLY_MAP" >"$REPLY_MAP.tmp" 2>/dev/null && mv "$REPLY_MAP.tmp" "$REPLY_MAP"
            fi
        else
            case "$NOTIFY_LAST_HTTP" in
            429 | 5?? | 000 | "")
                # Transient (network blip, rate limit, backend hiccup) -
                # stop here without advancing STATE, so this same message
                # is retried next poll instead of being silently skipped.
                break
                ;;
            *)
                # Permanent (e.g. Telegram 400 rejecting the payload):
                # retrying can never succeed, and stopping here would block
                # every later SMS behind this one forever (seen live). Skip
                # it, and say so in the log.
                echo "$(date +%s) skipped SMS id=$id: HTTP $NOTIFY_LAST_HTTP" >>/tmp/sms_notify_skipped.log
                echo "$id" >"$STATE"
                ;;
            esac
        fi
    done
}

daemon_pid() {
    [ -f "$PIDFILE" ] || return 1
    pid=$(cat "$PIDFILE" 2>/dev/null)
    [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && grep -q sms_notify "/proc/$pid/cmdline" 2>/dev/null || return 1
    echo "$pid"
}

run_daemon() {
    echo $$ >"$PIDFILE"
    trap 'rm -f "$PIDFILE"; exit 0' TERM INT
    while :; do
        check_once
        sleep "$POLL_SECONDS"
    done
}

stop_daemon() {
    pid=$(daemon_pid) || return 0
    kill "$pid" 2>/dev/null
    rm -f "$PIDFILE"
}

case "$1" in
--daemon) run_daemon ;;
--stop) stop_daemon ;;
--restart)
    stop_daemon
    sh "$0" --ensure
    ;;
--ensure)
    # flock makes the check-then-launch atomic: the cron watchdog and a
    # just-finished --restart (or two overlapping cron ticks) can both
    # reach this within the same second, and without a lock both would
    # see "not running" and each start their own daemon - confirmed live,
    # this really happens, not just a theoretical race.
    (
        flock -n 9 || exit 0
        daemon_pid >/dev/null && exit 0
        if command -v setsid >/dev/null 2>&1; then
            setsid sh "$0" --daemon >/dev/null 2>&1 &
        else
            sh "$0" --daemon >/dev/null 2>&1 &
        fi
    ) 9>/tmp/sms_notify.lock
    ;;
*)
    # One-shot poll (manual use / initial baseline seed at setup time).
    check_once
    ;;
esac
