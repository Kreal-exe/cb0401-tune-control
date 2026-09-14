#!/bin/sh
#
# Shared by device_monitor.sh and command_watcher.sh: loads the active
# notification backend's config and provides notify() to send an alert
# through whichever one is configured.
#
# Config file (written by setup.sh/setup.ps1): /etc/crontabs/patches/notify.conf
#
#   NOTIFY_BACKEND=ntfy
#   NTFY_TOPIC=cb0401v2-xxxxxxxxxxxxxxxx
#
# or:
#
#   NOTIFY_BACKEND=telegram
#   TELEGRAM_BOT_TOKEN=123456:AAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
#   TELEGRAM_CHAT_ID=12345678
#
DIR=/etc/crontabs/patches
CONF="$DIR/notify.conf"

NOTIFY_BACKEND="ntfy"
NTFY_TOPIC=""
TELEGRAM_BOT_TOKEN=""
TELEGRAM_CHAT_ID=""
# shellcheck disable=SC1090
[ -f "$CONF" ] && . "$CONF"

# $1=title  $2=tag (ntfy-only, ignored for telegram)  $3=body
notify() {
    title="$1"
    body="$3"
    case "$NOTIFY_BACKEND" in
        telegram)
            curl -s -m 10 -X POST "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
                --data-urlencode "chat_id=${TELEGRAM_CHAT_ID}" \
                --data-urlencode "text=${title}
${body}" \
                > /dev/null 2>&1
            ;;
        *)
            curl -s -m 10 -H "Title: $title" -H "Tags: $2" -d "$body" "https://ntfy.sh/$NTFY_TOPIC" > /dev/null 2>&1
            ;;
    esac
}
