#!/bin/sh
#
# Keeps *periodic* CFR (Channel Frequency Response - Qualcomm's per-peer
# Wi-Fi channel-state capture, the vendor equivalent of "CSI") running for
# a few of the currently associated Wi-Fi clients - the router-side half of
# the motion map (../sensing.sh; see ../router/cfr-trigger for how capture
# is switched on and ../sensing/cfr.go for the record format).
#
# # Periodic, not one-shot (and what it took)
#
# Earlier versions re-armed a one-shot capture per client every few
# seconds: ~1 frame per client per cycle, too sparse to see movement as it
# happens. The firmware's periodic mode (a capture every N ms
# per peer, driven by the firmware itself with QoS-null/ACK exchanges,
# nothing the peer has to cooperate with) refused to start: "Global
# periodic timer is not enabled, configure global cfr timer". The setter
# for that timer exists in the kernel (ucfg_cfr_set_timer) but Xiaomi's
# cfg80211tool build only kept get_cfr_timer; cfr-trigger's -param mode
# sends the missing set form (radio parameter 0x1194, value 1 = enabled)
# over the same vendor channel - see cfr-trigger's package doc. With the
# timer on, the stock `wlanconfig <vap> cfr start <mac> <bw> <ms> <type>`
# works as documented, and measured live on this router:
#
#   periodicity 1 ms -> ~245 Hz, 5 ms -> ~116 Hz, 10 ms -> ~63 Hz,
#   20 ms -> ~46 Hz, 50 ms -> ~15-20 Hz, 200 ms -> ~5 Hz per peer
#   (8388 bytes per record at 80 MHz; the <bw> argument did not shrink it;
#   one cfr_test_app output file stops at 1000 records, so the reader
#   rotation below must stay well under that)
#
# Starting an already-started peer is a harmless no-op (rc 0, no kernel
# error), stopping a non-started one logs "periodic cfr not started" and
# nothing else; the kernel prints per-capture stats on stop. Single-shot
# arms are rejected while a periodic capture runs on the radio, so this
# daemon never issues them.
#
# # Which clients
#
# Not all of them: every periodic peer costs airtime and ~170 KB/s of
# dump data at 20 Hz on 5 GHz (which the motion map pulls over SSH), and a
# few well-placed, awake, stationary devices are what sensing wants. Up to
# MAX_PEERS are chosen every RECONCILE_SECONDS: pinned PEERS first (if
# associated), then clients not in power save, longest-associated first
# (the strongest signal is a bad criterion: on the test network it picked
# a robot vacuum), then the rest. Clients that leave (or get displaced) are stopped so the
# firmware's periodic-client slots are freed. Override any of these in
# /etc/crontabs/patches/cfr_capture.conf:
#
#   PERIODICITY_MS=50
#   MAX_PEERS=4
#   PEERS="aa:bb:cc:dd:ee:ff 11:22:33:44:55:66"
#   BAND=2.4   # 2.4, 5 or both (default): which radio(s) to capture on
#
# Station discovery uses `wlanconfig <vap> list sta`, not `iw dev <vap>
# station dump` - confirmed live that this router's `iw` build reports zero
# stations on every VAP even with multiple real, actively-communicating
# clients present - wlanconfig (the QCA-native tool) reports them
# correctly. Its PSMODE column is the last one, RSSI the sixth.
#
# # Reader
#
# The captured records only reach userspace through /usr/sbin/cfr_test_app
# (the stock debugfs reader), which writes /tmp/cfr_dump_wifi{0,1}_<ts>.bin
# for as long as it runs. One reader per radio is restarted every
# POLL_SECONDS purely to rotate files (the motion map takes complete files
# and deletes them; /tmp is RAM). Exactly one reader per radio at a time:
# two readers on the same radio split the stream between them (confirmed
# live - counts halved and worse). This daemon doesn't analyse anything
# itself; the motion map does, on the computer running sensing.sh.
#
# Modes: same --daemon/--ensure/--stop/--restart convention as
# command_watcher.sh/sms_notify.sh. No argument = one reconcile pass.
# Not started from cron: sensing.sh runs it with --ensure while the map
# runs and --stop on exit (which also switches the timer off).
#
DIR=/etc/crontabs/patches
TRIGGER="$DIR/cfr-trigger"
READER=/usr/sbin/cfr_test_app
PIDFILE=/tmp/cfr_capture_daemon.pid
LOG=/tmp/cfr_capture_daemon.log
STATE=/tmp/cfr_periodic_armed.txt # "vap mac" per line: what this daemon has started (RAM, like the firmware's own state)
CONF="$DIR/cfr_capture.conf"
POLL_SECONDS=2       # reader window = dump file rotation period
RECONCILE_SECONDS=10 # how often the peer selection is re-evaluated
# Backlog cap. The dump files go to /tmp, which is RAM (tmpfs) on this
# router: 405 MB total, ~175 MB free, no swap. At 20 ms one client alone
# writes ~390 KB/s, so with nobody collecting (the motion map not running -
# e.g. the Mac asleep or rebooted) RAM ran out in about 8 minutes and the
# router went down - it rebooted every ~13 minutes on 2026-09-26 while this
# daemon kept capturing with no consumer. Now: never keep more than
# MAX_BACKLOG_KB of uncollected files (oldest are dropped), and if the
# backlog stays over the cap for NO_CONSUMER_SECONDS, nobody is reading -
# stop capturing altogether and switch the timer off.
MAX_BACKLOG_KB=16384
NO_CONSUMER_SECONDS=60
# 20 ms -> ~46-50 captures/s per peer, the rate the motion metric was
# validated at. The firmware goes further (measured: 10 ms -> 63 Hz,
# 5 ms -> 116 Hz, 1 ms -> ~245 Hz on one peer), but the SSH link to the
# host tops out around 4 MB/s (~490 records/s at 8.4 KB each on 5 GHz;
# 2.4 GHz records are ~1 KB).
PERIODICITY_MS=20
# Movement is seen along each link's path, so several stationary, awake
# devices in different directions cover more of the home (sensing.sh
# --links changes it; the firmware has an unknown cap on periodic peers -
# "max periodic cfr clients reached" - 4 worked in testing).
MAX_PEERS=4
PEERS=""
BAND=both
# shellcheck disable=SC1090
[ -f "$CONF" ] && . "$CONF"

ALL_VAPS="wl0 wl1 wl13 wl5"
ALL_RADIOS="wifi0 wifi1"
# VAPs and radios for BAND, from each VAP's live frequency and parent radio
# rather than hard-coded (on the test router wifi0 = 2.4 GHz with wl1/wl13,
# wifi1 = 5 GHz with wl0/wl5).
VAPS=""
RADIOS=""
for v in $ALL_VAPS; do
    ghz=$(iwconfig "$v" 2>/dev/null | sed -n 's/.*Frequency:\([0-9]\).*/\1/p')
    case "$BAND:$ghz" in
    both:* | 2.4:2 | 5:5) ;;
    *) continue ;;
    esac
    VAPS="$VAPS $v"
    r=$(cat "/sys/class/net/$v/parent" 2>/dev/null)
    case " $RADIOS " in *" $r "*) ;; *) [ -n "$r" ] && RADIOS="$RADIOS $r" ;; esac
done
[ -n "$RADIOS" ] || RADIOS="$ALL_RADIOS"
TIMER_PARAM=0x1194 # CFR global periodic timer enable, see cfr-trigger's package doc

log() { echo "$(date '+%F %T') $*" >>"$LOG"; }

enable_timer() {
    for r in $RADIOS; do
        cur=$(cfg80211tool "$r" get_cfr_timer 2>/dev/null | sed -n 's/.*get_cfr_timer://p')
        [ "$cur" = "1" ] && continue
        if [ -x "$TRIGGER" ] && "$TRIGGER" -iface "$r" -param "$TIMER_PARAM" -value 1 >>"$LOG" 2>&1; then
            log "enabled CFR periodic timer on $r"
        else
            log "could not enable CFR periodic timer on $r - periodic capture will fail there"
        fi
    done
}

# Every associated station: "vap mac rssi psmode assoc_seconds".
# ASSOCTIME (column 20, "H:MM:SS") is how long the station has been
# associated - the best stationarity hint available: a TV or a desktop
# stays connected for days, phones and laptops come and go.
candidates() {
    for vap in $VAPS; do
        wlanconfig "$vap" list sta 2>/dev/null |
            awk -v v="$vap" 'NR>1 && /^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}[[:space:]]/ {
                n = split($20, t, ":"); secs = (n == 3) ? t[1] * 3600 + t[2] * 60 + t[3] : 0
                print v, tolower($1), $6, $NF, secs
            }'
    done
}

# Up to MAX_PEERS lines of "vap mac": pinned first, then whatever is
# already capturing and still associated (hysteresis - RSSI and power-save
# state wobble from one listing to the next, and re-ranking on every pass
# made the selection churn: a peer stopped, another started, ten seconds
# later the reverse, and every switch resets that link's baseline -
# confirmed live), then awake clients, longest associated first, then the rest.
select_peers() {
    c=/tmp/cfr_candidates.$$
    candidates >"$c"
    {
        for m in $PEERS; do awk -v m="$(echo "$m" | tr 'A-F' 'a-f')" '$2==m' "$c"; done
        # ...but only while awake: a peer in power save yields nothing
        # (firmware: "CFR capture failed as peer is in powersave") and
        # would otherwise hold its slot forever (confirmed live).
        [ -f "$STATE" ] && while read -r _v m; do [ -n "$m" ] && awk -v m="$m" '$2==m && $4==0' "$c"; done <"$STATE"
        awk '$4==0' "$c" | sort -k5,5nr
        awk '$4!=0' "$c" | sort -k5,5nr
    } | awk '!seen[$2]++ {print $1, $2}' | head -n "$MAX_PEERS"
    rm -f "$c"
}

reconcile() {
    want=$(select_peers)
    [ -f "$STATE" ] || : >"$STATE"
    while read -r vap mac; do
        [ -n "$mac" ] || continue
        printf '%s\n' "$want" | grep -q " $mac\$" && continue
        wlanconfig "$vap" cfr stop "$mac" >>"$LOG" 2>&1
        log "stopped periodic capture for $mac on $vap"
    done <"$STATE"
    printf '%s\n' "$want" | while read -r vap mac; do
        [ -n "$mac" ] || continue
        grep -q " $mac\$" "$STATE" && continue
        # <bw>=0, <type>=0: the values verified live; bw did not change
        # the record size, type 0 is the QoS-null based method that works.
        if wlanconfig "$vap" cfr start "$mac" 0 "$PERIODICITY_MS" 0 >>"$LOG" 2>&1; then
            log "started periodic capture every ${PERIODICITY_MS} ms for $mac on $vap"
        else
            log "failed to start periodic capture for $mac on $vap"
        fi
    done
    printf '%s\n' "$want" >"$STATE"
}

stop_all_captures() {
    [ -f "$STATE" ] || return 0
    while read -r vap mac; do
        [ -n "$mac" ] && wlanconfig "$vap" cfr stop "$mac" >>"$LOG" 2>&1
    done <"$STATE"
    : >"$STATE"
}

READERS=""
start_readers() {
    READERS=""
    for radio in $RADIOS; do
        [ -x "$READER" ] || continue
        "$READER" -i "$radio" >/dev/null 2>&1 &
        READERS="$READERS $!"
    done
}
stop_readers() {
    # shellcheck disable=SC2086
    [ -n "$READERS" ] && kill $READERS 2>/dev/null
    READERS=""
}

disable_timer() {
    for r in $ALL_RADIOS; do
        [ -x "$TRIGGER" ] && "$TRIGGER" -iface "$r" -param "$TIMER_PARAM" -value 0 >>"$LOG" 2>&1
    done
    log "CFR periodic timer switched off"
}

# Prints the dump backlog size in KB after trimming it to MAX_BACKLOG_KB,
# dropping the oldest files first.
prune_backlog() {
    total=$(du -k /tmp/cfr_dump_wifi*.bin 2>/dev/null | awk '{s += $1} END {print s + 0}')
    over=$total
    while [ "$total" -gt "$MAX_BACKLOG_KB" ]; do
        oldest=$(ls -tr /tmp/cfr_dump_wifi*.bin 2>/dev/null | head -n 1)
        [ -n "$oldest" ] || break
        rm -f "$oldest"
        total=$(du -k /tmp/cfr_dump_wifi*.bin 2>/dev/null | awk '{s += $1} END {print s + 0}')
    done
    echo "$over"
}

daemon_pid() {
    [ -f "$PIDFILE" ] || return 1
    pid=$(cat "$PIDFILE" 2>/dev/null)
    [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && grep -q cfr_capture_daemon "/proc/$pid/cmdline" 2>/dev/null || return 1
    echo "$pid"
}

# The firmware offers no way to list its periodic-capture peers (`wlanconfig
# cfr list` prints nothing even with captures running - confirmed live),
# and a previous daemon instance that died without its trap (power loss
# on the host side can't do that, but a kill -9 or a crash can) leaves its
# peers capturing forever, silently eating periodic-client slots and
# airtime (confirmed live: two such orphans kept streaming at full rate
# after a restart). So start from a known state: stop every associated
# station once; the ones that weren't capturing just log "periodic cfr
# not started" in dmesg.
stop_all_stations() {
    candidates | while read -r vap mac _rest; do
        wlanconfig "$vap" cfr stop "$mac" >/dev/null 2>&1
    done
    : >"$STATE"
}

run_daemon() {
    echo $$ >"$PIDFILE"
    trap 'stop_readers; stop_all_captures; disable_timer; rm -f "$PIDFILE"; exit 0' TERM INT
    log "daemon start: band=$BAND vaps=$VAPS radios=$RADIOS periodicity=${PERIODICITY_MS}ms max_peers=$MAX_PEERS pinned='$PEERS'"
    enable_timer
    stop_all_stations
    last=0
    overflow_since=0
    while :; do
        now=$(date +%s)
        if [ $((now - last)) -ge "$RECONCILE_SECONDS" ]; then
            reconcile
            last=$now
        fi
        start_readers
        sleep "$POLL_SECONDS"
        stop_readers
        backlog=$(prune_backlog)
        now=$(date +%s)
        if [ "$backlog" -gt "$MAX_BACKLOG_KB" ]; then
            [ "$overflow_since" -gt 0 ] || overflow_since=$now
            if [ $((now - overflow_since)) -ge "$NO_CONSUMER_SECONDS" ]; then
                log "nobody has collected the captures for ${NO_CONSUMER_SECONDS}s (backlog ${backlog} KB) - stopping"
                stop_all_captures
                disable_timer
                rm -f /tmp/cfr_dump_wifi*.bin "$PIDFILE"
                exit 0
            fi
        else
            overflow_since=0
        fi
    done
}

stop_daemon() {
    if pid=$(daemon_pid); then
        kill "$pid" 2>/dev/null
        # Give its trap a moment to stop the captures and the timer.
        sleep 2
    else
        # No daemon (or its pidfile is gone): still make sure nothing is
        # left capturing - every associated station and the timer.
        stop_all_stations
        disable_timer
    fi
    # Make sure no stray reader survives (it would keep writing into /tmp
    # with nobody rotating or collecting).
    killall cfr_test_app 2>/dev/null
    rm -f "$PIDFILE" /tmp/cfr_dump_wifi*.bin
}

case "$1" in
--daemon) run_daemon ;;
--stop) stop_daemon ;;
--restart)
    stop_daemon
    sh "$0" --ensure
    ;;
--ensure)
    (
        flock -n 9 || exit 0
        daemon_pid >/dev/null && exit 0
        if command -v setsid >/dev/null 2>&1; then
            setsid sh "$0" --daemon >/dev/null 2>&1 &
        else
            sh "$0" --daemon >/dev/null 2>&1 &
        fi
    ) 9>/tmp/cfr_capture_daemon.lock
    ;;
*)
    enable_timer
    reconcile
    ;;
esac
