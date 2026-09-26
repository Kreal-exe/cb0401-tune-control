#!/bin/sh
#
# Keeps *periodic* CFR (Channel Frequency Response - Qualcomm's per-peer
# Wi-Fi channel-state capture, the vendor equivalent of "CSI") running for
# a few of the currently associated Wi-Fi clients - the router-side half of
# the RuView integration (see ../router/cfr-trigger and ../router/
# ruview-bridge for the rest and how the formats were reverse-engineered).
#
# # Periodic, not one-shot (and what it took)
#
# Earlier versions re-armed a one-shot capture per client every few
# seconds: ~1 frame per client per cycle. RuView's vital-sign estimation
# wants a real sample stream (its own ESP32 nodes send 20-100 Hz; the
# heartbeat band-pass alone needs a few Hz), so that could never do more
# than presence/motion. The firmware's periodic mode (a capture every N ms
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
# dump data at 20 Hz (which ruview-bridge then pulls over SSH), and two
# or three well-placed, awake devices are what sensing wants anyway. Up to
# MAX_PEERS are chosen every RECONCILE_SECONDS: pinned PEERS first (if
# associated), then clients not in power save, longest-associated first
# (the strongest signal is a bad criterion: on the test network it picked
# a robot vacuum), then the rest. Clients that leave (or get displaced) are stopped so the
# firmware's periodic-client slots are freed. Override any of these in
# /etc/crontabs/patches/cfr_capture.conf:
#
#   PERIODICITY_MS=50
#   MAX_PEERS=1
#   PEERS="aa:bb:cc:dd:ee:ff 11:22:33:44:55:66"
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
# POLL_SECONDS purely to rotate files (ruview-bridge takes complete files
# and deletes them; /tmp is RAM). Exactly one reader per radio at a time:
# two readers on the same radio split the stream between them (confirmed
# live - counts halved and worse). This daemon does not convert anything
# itself; that's ruview-bridge's job on the host running RuView.
#
# Modes: same --daemon/--ensure/--stop/--restart convention as
# command_watcher.sh/sms_notify.sh. No argument = one reconcile pass.
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
# 20 ms -> ~46-50 Hz per peer: RuView's own ceiling. Its per-node vitals
# detector clamps the sample rate to 50 Hz and sizes its 30 s / 15 s
# breathing/heart windows from that, so feeding more per node would
# squeeze those windows and skew the rates. The firmware goes further
# (measured: 10 ms -> 63 Hz, 5 ms -> 116 Hz, 1 ms -> ~245 Hz on one
# peer, ~560 records/s summed over three), but the SSH link to the host
# tops out around 4 MB/s (~490 records/s at 8.4 KB each). Lower this and
# ruview-bridge amplitude-averages the excess down to 50 Hz per node,
# trading bandwidth for less noise.
PERIODICITY_MS=20
# One link by default. RuView models every node as a fixed sensor at a
# known spot; several of our links (to devices that move around, sleep and
# sit wherever) made it fuse nonsense - 4-5 phantom skeletons with ids
# churning every few frames while it estimated 1 person, and a vitals
# readout that followed whichever link sent the last frame (confirmed
# live). One link to a device that stays put is what it can make sense of.
MAX_PEERS=1
PEERS=""
# shellcheck disable=SC1090
[ -f "$CONF" ] && . "$CONF"

VAPS="wl0 wl1 wl13 wl5"
RADIOS="wifi0 wifi1"
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
# later the reverse, and every switch resets that node's history in
# RuView - confirmed live), then awake clients by RSSI, then the rest.
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
    trap 'stop_readers; stop_all_captures; rm -f "$PIDFILE"; exit 0' TERM INT
    log "daemon start: periodicity=${PERIODICITY_MS}ms max_peers=$MAX_PEERS pinned='$PEERS'"
    enable_timer
    stop_all_stations
    last=0
    while :; do
        now=$(date +%s)
        if [ $((now - last)) -ge "$RECONCILE_SECONDS" ]; then
            reconcile
            last=$now
        fi
        start_readers
        sleep "$POLL_SECONDS"
        stop_readers
    done
}

stop_daemon() {
    pid=$(daemon_pid) || return 0
    kill "$pid" 2>/dev/null
    # Give its trap a moment to stop the captures, then make sure no
    # stray reader survives (a reader outliving the daemon would keep
    # writing into /tmp with nobody rotating or collecting).
    sleep 1
    killall cfr_test_app 2>/dev/null
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
