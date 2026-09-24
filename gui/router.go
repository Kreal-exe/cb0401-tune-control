package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- Config ---

var (
	routerIP         = getenv("ROUTER_IP", "192.168.31.1")
	routerPass       = getenv("ROUTER_ROOT_PASSWORD", "root")
	ntfyTopicEnv     = getenv("NTFY_TOPIC", "")
	notifyBackendEnv = getenv("NOTIFY_BACKEND", "ntfy")
	telegramTokenEnv = getenv("TELEGRAM_BOT_TOKEN", "")
	telegramChatEnv  = getenv("TELEGRAM_CHAT_ID", "")
	keyPath          string // set in main(), next to the executable
	envFilePath      string // set in main(), the .env next to the executable
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func sshOpts() []string {
	return []string{
		"-o", "StrictHostKeyChecking=no",
		// This router regenerates its dropbear host key on every boot (same
		// ramfs /etc as everything else that doesn't persist) - checking it
		// would just mean every reboot breaks the connection with "REMOTE
		// HOST IDENTIFICATION HAS CHANGED", which also makes OpenSSH refuse
		// password auth outright even with StrictHostKeyChecking=no. There's
		// no host identity here worth verifying against, so don't keep one.
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "HostKeyAlgorithms=+ssh-rsa",
		"-o", "PubkeyAcceptedAlgorithms=+ssh-rsa",
		"-o", "ConnectTimeout=5",
	}
}

// RouterError is returned for anything that should be shown to the user as
// a clean error message rather than a Go-internal one.
type RouterError struct{ msg string }

func (e *RouterError) Error() string { return e.msg }

func routerErrf(format string, a ...any) error {
	return &RouterError{fmt.Sprintf(format, a...)}
}

// -------------------------------------------------------------- SSH exec ---

// runSSHRaw runs one command on the router over SSH (key-based or
// password-based) and reports back exactly what happened, without
// interpreting it - run()/runBg() decide what any of it means.
func runSSHRaw(useKey bool, cmd string, timeout time.Duration) (stdout, stderr string, exitCode int, timedOut, notFound bool, err error) {
	var name string
	var args []string
	if useKey {
		name = "ssh"
		args = append([]string{"-i", keyPath}, sshOpts()...)
	} else {
		name = "sshpass"
		args = append([]string{"-p", routerPass, "ssh"}, sshOpts()...)
	}
	args = append(args, "root@"+routerIP, cmd)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	runErr := c.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out.String(), errb.String(), -1, true, false, nil
	}
	if runErr != nil {
		var execErr *exec.Error
		if errors.As(runErr, &execErr) {
			// e.g. sshpass isn't installed - distinct from a normal
			// nonzero exit, since the command never actually ran.
			return "", "", -1, false, true, runErr
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return out.String(), errb.String(), exitErr.ExitCode(), false, false, nil
		}
		return out.String(), errb.String(), -1, false, false, runErr
	}
	return out.String(), errb.String(), 0, false, false, nil
}

// reinstallKey re-adds our public key to authorized_keys - used after the
// key stops working (e.g. the router rebooted and dropbear came back up
// with a fresh /etc, which does happen on this platform).
func reinstallKey() {
	pubBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return
	}
	pubkey := strings.TrimSpace(string(pubBytes))
	cmd := fmt.Sprintf(
		`mkdir -p /etc/dropbear; grep -qF "%s" /etc/dropbear/authorized_keys 2>/dev/null || echo "%s" >> /etc/dropbear/authorized_keys; chmod 600 /etc/dropbear/authorized_keys`,
		pubkey, pubkey,
	)
	// Self-healing is best-effort: it must never fail the original request,
	// so any error here (including a missing sshpass binary) is swallowed.
	_, _, _, _, _, _ = runSSHRaw(false, cmd, 10*time.Second)
}

// run executes a single shell command on the router over SSH and returns
// its stdout.
//
// Tries the key first (fast, the normal path). If that fails (typically
// because the router rebooted and dropbear came back up without our key in
// authorized_keys), falls back to the persistent root password (unlike the
// key, this one is confirmed to survive a reboot) and quietly reinstalls
// the key so the next call goes through the fast path again without any
// manual intervention.
func run(cmd string, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	stdout, stderr, code, timedOut, _, err := runSSHRaw(true, cmd, timeout)
	if err != nil {
		return "", routerErrf("SSH error running %q: %v", cmd, err)
	}
	if timedOut {
		return "", routerErrf("Timed out running: %s", cmd)
	}

	keyAuthFailed := code == 255 && (strings.Contains(stderr, "Permission denied") || strings.Contains(stderr, "Connection closed"))
	if keyAuthFailed {
		stdout2, stderr2, code2, timedOut2, notFound2, err2 := runSSHRaw(false, cmd, timeout)
		if notFound2 {
			return "", routerErrf(
				"SSH key login failed and the password fallback could not run (%v). "+
					`If "sshpass" isn't installed, either install it or re-run setup to reinstall the SSH key.`, err2)
		}
		if err2 != nil {
			return "", routerErrf("SSH error running %q (password fallback): %v", cmd, err2)
		}
		if timedOut2 {
			return "", routerErrf("Timed out running (password fallback): %s", cmd)
		}
		stdout, stderr, code = stdout2, stderr2, code2
		if code == 0 || code == 1 {
			reinstallKey() // fix the key for next time, since we got in via password anyway
		}
	}

	if code != 0 && code != 1 { // 1 is often just "grep found nothing", not fatal
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(stdout)
		}
		return "", routerErrf("Error (%d): %s", code, msg)
	}
	return stdout, nil
}

// runBg executes a potentially slow command (e.g. a wifi reload) and
// tolerates it running long, rather than treating that as an error.
func runBg(cmd string, timeout time.Duration) string {
	if timeout == 0 {
		timeout = 150 * time.Second
	}
	stdout, stderr, code, timedOut, _, err := runSSHRaw(true, cmd, timeout)
	if timedOut {
		return "(command is still running on the router, the interface should come back up on its own within 1-2 minutes)"
	}
	if err == nil && code == 255 && strings.Contains(stderr, "Permission denied") {
		stdout2, stderr2, code2, timedOut2, notFound2, err2 := runSSHRaw(false, cmd, timeout)
		if timedOut2 {
			return "(command is still running on the router, the interface should come back up on its own within 1-2 minutes)"
		}
		if !notFound2 && err2 == nil {
			stdout, stderr, code = stdout2, stderr2, code2
			if code == 0 || code == 1 {
				reinstallKey()
			}
		}
	}
	return stdout + stderr
}

// ---------------------------------------------------------------- Cellular ---

const atPort = "/dev/ttyUSB2"

var atCommandQuoteRe = regexp.MustCompile("'")

// atQuery sends a list of AT commands SEQUENTIALLY within one SSH session.
//
// This mirrors a fair amount of trial and error against the real modem:
// rapid repeated opens of /dev/ttyUSB2 without proper draining leave stale
// responses from earlier commands "leaking" into later reads, so this
// kills stray readers, drains the port, and syncs with a bare AT ping
// before sending the real command(s).
func atQuery(commands []string, wait time.Duration) (string, error) {
	waitSec := wait.Seconds()
	parts := []string{
		fmt.Sprintf(`for p in $(ps w | grep '[c]at %s' | awk '{print $1}'); do kill -9 $p 2>/dev/null; done`, atPort),
		fmt.Sprintf(`for i in 1 2 3 4 5 6 7 8; do [ -z "$(ps w | grep '[c]at %s')" ] && break; sleep 0.25; done`, atPort),
		fmt.Sprintf(`cat %s > /dev/null & FLUSHPID=$!`, atPort),
		`sleep 1.5`,
		`kill $FLUSHPID 2>/dev/null`,
		`rm -f /tmp/at_sync.txt`,
		fmt.Sprintf(`cat %s > /tmp/at_sync.txt & SYNCPID=$!`, atPort),
		fmt.Sprintf(`printf 'AT\r' > %s`, atPort),
		`sleep 1.5`,
		`kill $SYNCPID 2>/dev/null`,
		`rm -f /tmp/at_resp.txt`,
		fmt.Sprintf(`cat %s > /tmp/at_resp.txt & CATPID=$!`, atPort),
		`sleep 0.4`,
	}
	for _, c := range commands {
		if atCommandQuoteRe.MatchString(c) {
			return "", fmt.Errorf("AT command can't contain a single quote: %q", c)
		}
		parts = append(parts, fmt.Sprintf(`printf '%s\r' > %s`, c, atPort))
		parts = append(parts, fmt.Sprintf(`sleep %g`, waitSec))
	}
	parts = append(parts, `kill $CATPID 2>/dev/null`, `cat /tmp/at_resp.txt`)
	cmd := strings.Join(parts, " ; ")
	timeout := time.Duration(15+float64(len(commands))*waitSec) * time.Second
	return run(cmd, timeout)
}

var qnwprefcfgRe = regexp.MustCompile(`\+QNWPREFCFG:\s*"([a-zA-Z0-9_]+)",\s*([^\r\n]+)`)

func parseQnwprefcfg(raw string) map[string]string {
	out := map[string]string{}
	for _, m := range qnwprefcfgRe.FindAllStringSubmatch(raw, -1) {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

func getModemConfig() (map[string]string, error) {
	cfg := map[string]string{}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		raw, err := atQuery([]string{`AT+QNWPREFCFG="ue_capability_band"`}, 3500*time.Millisecond)
		if err != nil {
			lastErr = err
		} else {
			cfg = parseQnwprefcfg(raw)
			// Full set is 4 keys (gw_band/lte_band/nsa_nr5g_band/nr5g_band).
			// Fewer than that means the modem hasn't finished replying yet
			// - treat that as a failure too and retry.
			if len(cfg) >= 4 {
				break
			}
		}
		time.Sleep(time.Duration(1500+attempt*1000) * time.Millisecond)
	}
	if len(cfg) == 0 && lastErr != nil {
		return nil, lastErr
	}
	for attempt := 0; attempt < 4; attempt++ {
		raw2, err := atQuery([]string{`AT+QNWPREFCFG="nr5g_disable_mode"`}, 1500*time.Millisecond)
		if err == nil {
			parsed2 := parseQnwprefcfg(raw2)
			if len(parsed2) > 0 {
				for k, v := range parsed2 {
					cfg[k] = v
				}
				break
			}
		}
		time.Sleep(time.Duration(1500+attempt*1000) * time.Millisecond)
	}
	return cfg, nil
}

func setSaEnabled(enabled bool) (map[string]string, error) {
	val := "1"
	if enabled {
		val = "0"
	}
	if _, err := atQuery([]string{fmt.Sprintf(`AT+QNWPREFCFG="nr5g_disable_mode",%s`, val)}, 1500*time.Millisecond); err != nil {
		return nil, err
	}
	return getModemConfig()
}

type bandRequest struct {
	key, value string
}

// setBands writes the requested bands then does ONE reliable readback to
// verify - see atQuery's doc comment; checking immediately after a single
// SET is unreliable on this modem (it can echo back a fragment of an
// earlier command's reply even when the SET itself worked).
func setBands(nr5gBand, nsaNr5gBand, lteBand string) (map[string]string, error) {
	var todo []bandRequest
	if nr5gBand != "" {
		todo = append(todo, bandRequest{"nr5g_band", nr5gBand})
	}
	if nsaNr5gBand != "" {
		todo = append(todo, bandRequest{"nsa_nr5g_band", nsaNr5gBand})
	}
	if lteBand != "" {
		todo = append(todo, bandRequest{"lte_band", lteBand})
	}
	if len(todo) == 0 {
		return getModemConfig()
	}

	for _, req := range todo {
		if _, err := atQuery([]string{fmt.Sprintf(`AT+QNWPREFCFG="%s",%s`, req.key, req.value)}, 2*time.Second); err != nil {
			return nil, err
		}
		time.Sleep(2 * time.Second)
	}

	final, err := getModemConfig()
	if err != nil {
		return nil, err
	}
	_ = updateBandPrefs(final["nr5g_band"], final["nsa_nr5g_band"])
	return final, nil
}

// ------------------------------------------------------------------- WiFi ---

type wifiDevice struct{ uci, iface string }

var wifiDevices = map[string]wifiDevice{
	"2.4": {"wifi0", "wl1"},
	"5":   {"wifi1", "wl0"},
}

func freqToChannel24(freq int) (int, bool) {
	if freq >= 2412 && freq <= 2472 {
		return (freq-2412)/5 + 1, true
	}
	return 0, false
}

type scannedNetwork struct {
	freq   int
	signal float64
	hasSig bool
	ssid   string
}

var (
	scanBssRe    = regexp.MustCompile(`^BSS `)
	scanFreqRe   = regexp.MustCompile(`^freq:\s*(\d+)`)
	scanSignalRe = regexp.MustCompile(`^signal:\s*(-?[\d.]+)`)
	scanSsidRe   = regexp.MustCompile(`^SSID:\s*(.*)`)
)

type channelInfo struct {
	Channel         *int             `json:"channel"`
	Count           int              `json:"count"`
	StrongestSignal *float64         `json:"strongest_signal"`
	Networks        []map[string]any `json:"networks"`
}

type wifiScanResult struct {
	Band           string         `json:"band"`
	TotalNetworks  int            `json:"total_networks"`
	Channels       []channelInfo  `json:"channels"`
	Recommendation map[string]any `json:"recommendation"`
}

func scanWifiChannels(band string) (*wifiScanResult, error) {
	dev, ok := wifiDevices[band]
	if !ok {
		return nil, routerErrf("Unknown Wi-Fi band: %s", band)
	}
	raw, err := run(fmt.Sprintf("iw dev %s scan 2>&1", dev.iface), 25*time.Second)
	if err != nil {
		return nil, err
	}

	var networks []scannedNetwork
	var cur scannedNetwork
	haveCur := false
	flush := func() {
		if haveCur && cur.freq != 0 {
			networks = append(networks, cur)
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if scanBssRe.MatchString(line) {
			flush()
			cur = scannedNetwork{}
			haveCur = true
		}
		if m := scanFreqRe.FindStringSubmatch(line); m != nil {
			cur.freq, _ = strconv.Atoi(m[1])
		}
		if m := scanSignalRe.FindStringSubmatch(line); m != nil {
			cur.signal, _ = strconv.ParseFloat(m[1], 64)
			cur.hasSig = true
		}
		if m := scanSsidRe.FindStringSubmatch(line); m != nil {
			cur.ssid = m[1]
			if cur.ssid == "" {
				cur.ssid = "(hidden)"
			}
		}
	}
	flush()

	byChannel := map[int][]scannedNetwork{}
	var unknownChannel []scannedNetwork
	for _, n := range networks {
		if band == "2.4" {
			if ch, ok := freqToChannel24(n.freq); ok {
				byChannel[ch] = append(byChannel[ch], n)
				continue
			}
			unknownChannel = append(unknownChannel, n)
		} else {
			byChannel[n.freq] = append(byChannel[n.freq], n)
		}
	}

	var keys []int
	for k := range byChannel {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	var channels []channelInfo
	addChannel := func(chPtr *int, nets []scannedNetwork) {
		var strongest *float64
		for _, n := range nets {
			if n.hasSig {
				v := n.signal
				if strongest == nil || v > *strongest {
					strongest = &v
				}
			}
		}
		sortedNets := append([]scannedNetwork(nil), nets...)
		sort.Slice(sortedNets, func(i, j int) bool {
			si, sj := -999.0, -999.0
			if sortedNets[i].hasSig {
				si = sortedNets[i].signal
			}
			if sortedNets[j].hasSig {
				sj = sortedNets[j].signal
			}
			return si > sj
		})
		var netsOut []map[string]any
		for _, n := range sortedNets {
			var sig any
			if n.hasSig {
				sig = n.signal
			}
			netsOut = append(netsOut, map[string]any{"ssid": n.ssid, "signal": sig})
		}
		channels = append(channels, channelInfo{
			Channel: chPtr, Count: len(nets), StrongestSignal: strongest, Networks: netsOut,
		})
	}
	for _, ch := range keys {
		c := ch
		addChannel(&c, byChannel[ch])
	}
	if len(unknownChannel) > 0 {
		addChannel(nil, unknownChannel)
	}

	var recommendation map[string]any
	if band == "2.4" {
		recommendation = recommend24Channel(networks)
	}

	return &wifiScanResult{
		Band: band, TotalNetworks: len(networks), Channels: channels, Recommendation: recommendation,
	}, nil
}

// signalWeight: how much a given neighboring network actually interferes -
// strong and close networks matter more than weak and distant ones.
func signalWeight(hasSignal bool, signal float64) float64 {
	if !hasSignal {
		return 0.3
	}
	if signal >= -60 {
		return 1.0
	}
	if signal >= -75 {
		return 0.5
	}
	return 0.2
}

// recommend24Channel scores each of the three genuinely non-overlapping
// channels (1/6/11) by total interference from ALL networks seen -
// including ones on neighboring channels that partially overlap in
// spectrum (not just an exact match), weighted by signal strength.
func recommend24Channel(networks []scannedNetwork) map[string]any {
	candidates := []int{1, 6, 11}
	scores := map[string]float64{}
	best := candidates[0]
	bestScore := -1.0
	for _, cand := range candidates {
		score := 0.0
		for _, n := range networks {
			ch, ok := freqToChannel24(n.freq)
			if !ok {
				continue
			}
			diff := ch - cand
			if diff < 0 {
				diff = -diff
			}
			var overlap float64
			if diff == 0 {
				overlap = 1.0
			} else {
				overlap = float64(5-diff) / 5
				if overlap < 0 {
					overlap = 0
				}
			}
			if overlap == 0 {
				continue
			}
			score += overlap * signalWeight(n.hasSig, n.signal)
		}
		rounded := roundTo(score, 2)
		scores[strconv.Itoa(cand)] = rounded
		if bestScore < 0 || rounded < bestScore {
			bestScore = rounded
			best = cand
		}
	}
	return map[string]any{"best_channel": best, "scores": scores}
}

func roundTo(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int(v*p+0.5)) / p
}

func rebootRouter() map[string]any {
	// We don't wait for a reply - the connection will drop along with the
	// reboot itself, which is expected, not an error.
	_, _ = run("reboot &", 5*time.Second)
	return map[string]any{"rebooting": true}
}

const (
	versionSpoofTarget = "/usr/share/xiaoqiang/xiaoqiang_version"
	versionSpoofFake   = "/tmp/fake_version"
)

// spoofFirmwareVersion makes the stock web updater's downgrade check see
// version "0.0.1", by bind-mounting a doctored copy of the version file
// over the real one. Xiaomi's updater refuses to flash anything with a
// version number lower than the current one - since 0.0.1 is lower than
// any real firmware, this lifts that block so you can flash an older
// firmware file through the router's own web UI (Settings -> Update ->
// Local update).
//
// This is memory-only (mount --bind of a /tmp file): nothing is written
// to flash, and it reverts on its own the next time the router reboots.
// The real firmware file on disk is never touched or removed.
func spoofFirmwareVersion() (string, error) {
	cmds := []string{
		fmt.Sprintf("umount %s 2>/dev/null; true", versionSpoofTarget),
		fmt.Sprintf("cp %s %s", versionSpoofTarget, versionSpoofFake),
		fmt.Sprintf(`sed -i "s/option ROM '.*/option ROM '0.0.1'/g" %s`, versionSpoofFake),
		fmt.Sprintf("mount --bind %s %s", versionSpoofFake, versionSpoofTarget),
		fmt.Sprintf(`grep "option ROM" %s`, versionSpoofTarget),
	}
	out, err := run(strings.Join(cmds, " ; "), 15*time.Second)
	if err != nil {
		return "", err
	}
	if !strings.Contains(out, "0.0.1") {
		return "", routerErrf("The spoof did not take (the router still reports the old version): %s", strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// Adapted from davidohne/xiaomi_cb0401 (https://github.com/davidohne/xiaomi_cb0401/tree/main/Band_Unlock),
// originally written for the CB0401 (v1) - the modem is technically a
// different revision on the V2 (Quectel R01 vs R03), but this only
// configures the modem via AT+QNWPREFCFG, it doesn't touch modem firmware,
// so the same commands apply either way. Confirmed working on a CB0401V2.
const (
	band5gPatchScript = `#!/bin/sh

[ -e "/tmp/5g_band_patch.log" ] && exit 0

HOOK_SRC="/data/custom/hooks/99-set-5g-bands"
HOOK_DEST="/etc/hotplug.d/iface/99-set-5g-bands"

[ -x "$HOOK_SRC" ] || chmod 755 "$HOOK_SRC"

mkdir -p /etc/hotplug.d/iface

if [ ! -e "$HOOK_DEST" ] || [ ! -f "$HOOK_DEST" ]; then
    ln -sf "$HOOK_SRC" "$HOOK_DEST"
fi

echo "5g band hook installed" > /tmp/5g_band_patch.log
`
	// Reads the desired bands from band_prefs.conf (written by
	// updateBandPrefs, kept in sync with whatever was last written through
	// the Cellular card) rather than hardcoding a fixed list - the
	// hardcoded version silently overwrote the user's own band/region
	// choice every time the modem happened to reconnect (any wan_2 "ifup"
	// event re-fires this hook), which is exactly what caused a region
	// preset to intermittently appear to "not take": the write succeeded,
	// but a reconnect right after re-applied the hook's stale defaults
	// before the confirmation read.
	band5gHookScript = `#!/bin/sh
[ "$ACTION" = "ifup" ] || exit 0
[ "$INTERFACE" = "wan_2" ] || exit 0
CONF="/data/custom/hooks/band_prefs.conf"
NR5G_BAND="1:3:7:28:38:75:78"
NSA_NR5G_BAND="1:3:7:28:38:75:78"
[ -f "$CONF" ] && . "$CONF"
ATPORT="/dev/ttyUSB2"
send() { printf '%s\r' "$1" >"$ATPORT"; usleep 100000; }
send 'AT+QNWPREFCFG="nr5g_disable_mode",0'
send "AT+QNWPREFCFG=\"nsa_nr5g_band\",$NSA_NR5G_BAND"
send "AT+QNWPREFCFG=\"nr5g_band\",$NR5G_BAND"
`
	band5gFirewallSnippet = `
config include 'auto_5g_band_patch'
	option type 'script'
	option path '/data/etc/crontabs/patches/5g_band_patch.sh'
	option enabled '1'
`
	band5gPatchPath = "/data/etc/crontabs/patches/5g_band_patch.sh"
	band5gHookPath  = "/data/custom/hooks/99-set-5g-bands"
	band5gPrefsPath = "/data/custom/hooks/band_prefs.conf"
)

// updateBandPrefs writes the bands the hotplug hook (see band5gHookScript)
// should reapply on every wan_2 reconnect. Called after a successful
// setBands() so the hook stays in sync with whatever was last written
// through the Cellular card - without this, the hook's own hardcoded
// fallback would silently overwrite that choice the next time the modem
// reconnects. A no-op if the hook was never installed.
func updateBandPrefs(nr5gBand, nsaNr5gBand string) error {
	installed, err := run(fmt.Sprintf("[ -f %s ] && echo 1 || echo 0", band5gHookPath), 10*time.Second)
	if err != nil || strings.TrimSpace(installed) != "1" {
		return nil
	}
	content := fmt.Sprintf("NR5G_BAND=%s\nNSA_NR5G_BAND=%s\n", nr5gBand, nsaNr5gBand)
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	_, err = run(fmt.Sprintf("echo %s | base64 -d > %s", b64, band5gPrefsPath), 10*time.Second)
	return err
}

// unlock5GBands installs a permanent hotplug hook that unlocks Standalone
// mode and extra 5G bands (n1/n3/n7/n28/n38/n75/n78) on the modem every
// time the wan_2 interface comes up (including after a reboot), then
// forces it to run once immediately.
func unlock5GBands() (string, error) {
	if _, err := run("mkdir -p /data/etc/crontabs/patches /data/custom/hooks /etc/hotplug.d/iface", 10*time.Second); err != nil {
		return "", err
	}

	writeFile := func(path, content string) error {
		b64 := base64.StdEncoding.EncodeToString([]byte(content))
		_, err := run(fmt.Sprintf("echo %s | base64 -d > %s", b64, path), 10*time.Second)
		return err
	}
	if err := writeFile(band5gPatchPath, band5gPatchScript); err != nil {
		return "", err
	}
	if err := writeFile(band5gHookPath, band5gHookScript); err != nil {
		return "", err
	}
	if _, err := run(fmt.Sprintf("chmod 755 %s %s", band5gPatchPath, band5gHookPath), 10*time.Second); err != nil {
		return "", err
	}

	already, err := run("grep -c auto_5g_band_patch /etc/config/firewall 2>/dev/null || true", 10*time.Second)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(already) == "0" || strings.TrimSpace(already) == "" {
		b64 := base64.StdEncoding.EncodeToString([]byte(band5gFirewallSnippet))
		if _, err := run(fmt.Sprintf("echo %s | base64 -d >> /etc/config/firewall", b64), 10*time.Second); err != nil {
			return "", err
		}
	}

	// Seed band_prefs.conf with whatever the modem is already configured
	// for right now (rather than the hook's own hardcoded fallback), so
	// installing the hook doesn't change bands you've already picked.
	if cfg, err := getModemConfig(); err == nil && cfg["nr5g_band"] != "" {
		_ = updateBandPrefs(cfg["nr5g_band"], cfg["nsa_nr5g_band"])
	}

	if _, err := run(fmt.Sprintf("rm -f /tmp/5g_band_patch.log; sh %s", band5gPatchPath), 10*time.Second); err != nil {
		return "", err
	}

	out, err := run(fmt.Sprintf("ACTION=ifup INTERFACE=wan_2 sh %s; echo APPLIED", band5gHookPath), 15*time.Second)
	if err != nil {
		return "", err
	}
	if !strings.Contains(out, "APPLIED") {
		return "", routerErrf("The hook did not report success: %s", strings.TrimSpace(out))
	}
	return "Bands unlocked: SA mode enabled. The hook is now permanent and reapplies your current band selection on every wan_2 interface up (including after reboot) - whatever you write from the Cellular card below stays in effect.", nil
}

var (
	wifiInfoChanRe = regexp.MustCompile(`channel (\d+) \((\d+) MHz\), width: (\d+) MHz`)
	wifiInfoSsidRe = regexp.MustCompile(`ssid (\S+)`)
	wifiInfoTxpRe  = regexp.MustCompile(`txpower ([\d.]+) dBm`)
)

func getWifiStatus() map[string]map[string]any {
	result := map[string]map[string]any{}
	for band, dev := range wifiDevices {
		info, _ := run(fmt.Sprintf("iw dev %s info 2>&1", dev.iface), 0)
		var ssid, clients any
		var channel, freqMhz, widthMhz, txpower any
		if m := wifiInfoChanRe.FindStringSubmatch(info); m != nil {
			c, _ := strconv.Atoi(m[1])
			f, _ := strconv.Atoi(m[2])
			w, _ := strconv.Atoi(m[3])
			channel, freqMhz, widthMhz = c, f, w
		}
		if m := wifiInfoSsidRe.FindStringSubmatch(info); m != nil {
			ssid = m[1]
		}
		if m := wifiInfoTxpRe.FindStringSubmatch(info); m != nil {
			txpower, _ = strconv.ParseFloat(m[1], 64)
		}
		clientsRaw, _ := run(fmt.Sprintf("iw dev %s station dump | grep -c Station || true", dev.iface), 0)
		clientsRaw = strings.TrimSpace(clientsRaw)
		if clientsRaw == "" {
			clientsRaw = "0"
		}
		clients = clientsRaw
		result[band] = map[string]any{
			"ssid": ssid, "channel": channel, "freq_mhz": freqMhz, "width_mhz": widthMhz,
			"txpower_dbm": txpower, "clients": clients, "uci_device": dev.uci,
		}
	}
	return result
}

func setWifi(band string, channel, bw string) (map[string]map[string]any, error) {
	dev, ok := wifiDevices[band]
	if !ok {
		return nil, routerErrf("Unknown Wi-Fi band: %s", band)
	}
	var cmds []string
	if channel != "" {
		cmds = append(cmds, fmt.Sprintf("uci set wireless.%s.channel='%s'", dev.uci, channel))
	}
	if bw != "" {
		cmds = append(cmds, fmt.Sprintf("uci set wireless.%s.bw='%s'", dev.uci, bw))
	}
	if len(cmds) == 0 {
		return getWifiStatus(), nil
	}
	cmds = append(cmds, "uci commit wireless")
	if _, err := run(strings.Join(cmds, " ; "), 0); err != nil {
		return nil, err
	}
	runBg(fmt.Sprintf("wifi reload %s", dev.uci), 150*time.Second)
	time.Sleep(3 * time.Second)
	return getWifiStatus(), nil
}

// ------------------------------------------------------------- System health ---

func parseProcStatCPULine(line string) (idle, total int64) {
	fields := strings.Fields(line)[1:]
	nums := make([]int64, len(fields))
	for i, f := range fields {
		nums[i], _ = strconv.ParseInt(f, 10, 64)
	}
	idle = nums[3]
	if len(nums) > 4 {
		idle += nums[4]
	}
	for _, n := range nums {
		total += n
	}
	return idle, total
}

func getSystemHealth() (map[string]any, error) {
	result := map[string]any{}

	// Two /proc/stat samples one second apart, in ONE SSH session -
	// without that, network latency between two separate calls would
	// distort the interval.
	raw, err := run("cat /proc/stat | head -1; sleep 1; cat /proc/stat | head -1", 0)
	if err != nil {
		return nil, err
	}
	var cpuLines []string
	for _, l := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.HasPrefix(l, "cpu ") {
			cpuLines = append(cpuLines, l)
		}
	}
	if len(cpuLines) >= 2 {
		idle1, total1 := parseProcStatCPULine(cpuLines[0])
		idle2, total2 := parseProcStatCPULine(cpuLines[1])
		dt := total2 - total1
		if dt > 0 {
			result["cpu_pct"] = roundTo(100*(1-float64(idle2-idle1)/float64(dt)), 1)
		}
	}

	loadRaw, err := run("cat /proc/loadavg", 0)
	if err == nil {
		parts := strings.Fields(strings.TrimSpace(loadRaw))
		if len(parts) >= 3 {
			l1, _ := strconv.ParseFloat(parts[0], 64)
			l5, _ := strconv.ParseFloat(parts[1], 64)
			l15, _ := strconv.ParseFloat(parts[2], 64)
			result["load1"], result["load5"], result["load15"] = l1, l5, l15
		}
	}

	freeRaw, err := run("free", 0)
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(freeRaw), "\n") {
			if strings.HasPrefix(line, "Mem:") {
				f := strings.Fields(line)
				if len(f) >= 3 {
					total, _ := strconv.ParseInt(f[1], 10, 64)
					used, _ := strconv.ParseInt(f[2], 10, 64)
					result["mem_total_kb"], result["mem_used_kb"] = total, used
				}
			}
			// "-/+ buffers/cache: used free" is the actually-available
			// memory (buffers/cache get reclaimed on demand), not a naive
			// subtraction.
			if strings.HasPrefix(line, "-/+ buffers/cache:") {
				f := strings.Fields(line)
				if len(f) >= 4 {
					free, _ := strconv.ParseInt(f[3], 10, 64)
					result["mem_free_real_kb"] = free
				}
			}
		}
	}

	tempsRaw, err := run("cat /sys/class/thermal/thermal_zone*/temp 2>/dev/null", 0)
	if err == nil {
		var temps []int64
		for _, t := range strings.Split(strings.TrimSpace(tempsRaw), "\n") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if v, e := strconv.ParseInt(t, 10, 64); e == nil {
				temps = append(temps, v)
			}
		}
		if len(temps) > 0 {
			var sum, max int64
			max = temps[0]
			for _, t := range temps {
				sum += t
				if t > max {
					max = t
				}
			}
			// this platform reports whole degrees Celsius directly (not milli-C)
			result["temp_c_max"] = max
			result["temp_c_avg"] = roundTo(float64(sum)/float64(len(temps)), 1)
		}
	}

	uptimeRaw, err := run("cat /proc/uptime", 0)
	if err == nil {
		fields := strings.Fields(strings.TrimSpace(uptimeRaw))
		if len(fields) > 0 {
			if v, e := strconv.ParseFloat(fields[0], 64); e == nil {
				result["uptime_sec"] = int64(v)
			}
		}
	}

	return result, nil
}

// TX power is NOT controlled by this panel - verified via both available
// paths: `iw set txpower fixed` runs without error but has zero effect
// (not even momentarily); the vendor `cfg80211tool s_txpow` returns
// EINVAL, and g_txpow/get_maxpower/get_minpower simply aren't implemented
// by this driver (they return 0). getWifiStatus() shows the real value
// read-only; there's nothing here to change it with.

// ------------------------------------------------------------ Device watch ---

const devmonDir = "/etc/crontabs/patches"

var (
	knownMacsFile  = devmonDir + "/known_macs.txt"
	notifyConfFile = devmonDir + "/notify.conf"
	macRe          = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)
)

func parseConf(raw string) map[string]string {
	conf := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		conf[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return conf
}

type notifyConfig struct {
	Backend          string `json:"backend"`
	NtfyTopic        string `json:"ntfy_topic"`
	TelegramBotToken string `json:"telegram_bot_token"`
	TelegramChatID   string `json:"telegram_chat_id"`
	SmsForward       bool   `json:"sms_forward"`
}

// getNotifyConfig reads the router's live notify.conf - the single source
// of truth for which backend (ntfy/Telegram) is active, rather than
// trusting whatever environment variables this process happened to be
// launched with (those only matter as a fallback, for a router setup.sh
// hasn't touched yet).
func getNotifyConfig() notifyConfig {
	raw, _ := run("cat "+notifyConfFile+" 2>/dev/null || true", 0)
	conf := parseConf(raw)
	pick := func(key, envDefault string) string {
		if v, ok := conf[key]; ok && v != "" {
			return v
		}
		return envDefault
	}
	backend := pick("NOTIFY_BACKEND", notifyBackendEnv)
	if backend == "" {
		backend = "ntfy"
	}
	// Defaults to on: sms_notify.sh only exists to forward SMS, so an
	// installed-but-silently-doing-nothing daemon would be a more
	// surprising default than the reverse. The GUI checkbox is there for
	// anyone who'd rather it stayed off.
	smsForward := pick("SMS_FORWARD", "1") != "0"
	return notifyConfig{
		Backend:          backend,
		NtfyTopic:        pick("NTFY_TOPIC", ntfyTopicEnv),
		TelegramBotToken: pick("TELEGRAM_BOT_TOKEN", telegramTokenEnv),
		TelegramChatID:   pick("TELEGRAM_CHAT_ID", telegramChatEnv),
		SmsForward:       smsForward,
	}
}

// setNotifyConfig replaces the router's notify.conf outright -
// device_monitor.sh and command_watcher.sh read it fresh on every cron
// run, so nothing else needs restarting for a backend switch to take
// effect.
func setNotifyConfig(backend string, telegramBotToken, telegramChatID *string, smsForward *bool) (notifyConfig, error) {
	if backend != "ntfy" && backend != "telegram" {
		return notifyConfig{}, routerErrf(`Unknown notification backend: %q (expected "ntfy" or "telegram")`, backend)
	}
	current := getNotifyConfig()
	forward := current.SmsForward
	if smsForward != nil {
		forward = *smsForward
	}
	tok := current.TelegramBotToken
	if telegramBotToken != nil {
		tok = *telegramBotToken
	}
	chat := current.TelegramChatID
	if telegramChatID != nil {
		chat = *telegramChatID
	}
	if backend == "telegram" && tok == "" {
		return notifyConfig{}, routerErrf("Telegram backend needs a bot token")
	}
	// A backend switch, or new/changed Telegram credentials, means whoever's
	// on the receiving end hasn't seen the command cheat-sheet before (or is
	// looking at a chat/topic that's never gotten one) - send it once, right
	// after the config that makes it deliverable actually lands.
	sendWelcome := backend != current.Backend ||
		(backend == "telegram" && (tok != current.TelegramBotToken || chat != current.TelegramChatID))

	forwardVal := "0"
	if forward {
		forwardVal = "1"
	}
	content := fmt.Sprintf("NOTIFY_BACKEND=%s\nNTFY_TOPIC=%s\nTELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%s\nSMS_FORWARD=%s\n",
		backend, current.NtfyTopic, tok, chat, forwardVal)
	if _, err := run("mkdir -p "+devmonDir, 0); err != nil {
		return notifyConfig{}, err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	if _, err := run(fmt.Sprintf("echo %s | base64 -d > %s && chmod 600 %s", b64, notifyConfFile, notifyConfFile), 0); err != nil {
		return notifyConfig{}, err
	}
	if sendWelcome {
		sendNotifyWelcomeMessage()
	}
	return getNotifyConfig(), nil
}

// sendNotifyWelcomeMessage sends a one-time cheat-sheet through whichever
// backend notify.conf now points at, right after it's set up or switched -
// so you see the available commands once, up front, instead of having to
// remember them or dig through the README the first time a device alert
// actually shows up. Best-effort: a failure here shouldn't fail the config
// save that triggered it (notify_common.sh may not be deployed yet if this
// is somehow called before the first full setup, for instance).
func sendNotifyWelcomeMessage() {
	msg := "Notifications are set up. When an unrecognized device joins your network, you'll get an alert here. Reply to it (or just message this chat) with:\n\n" +
		"trust - adds the last unrecognized device seen to the whitelist (no more alerts for it)\n" +
		"<MAC or IP> trust - whitelists a specific device\n" +
		"block - blocks the last unrecognized device seen\n" +
		"<MAC or IP> block - blocks a specific device\n" +
		"red alert - locks Wi-Fi to only whitelisted devices (briefly disconnects everyone, including trusted devices - there's no way around that on this hardware)\n" +
		"all clear - undoes red alert, back to normal"
	b64 := base64.StdEncoding.EncodeToString([]byte(msg))
	cmd := fmt.Sprintf(`. %s/notify_common.sh && notify "Welcome" "wave" "$(echo %s | base64 -d)"`, devmonDir, b64)
	_, _ = run(cmd, 10*time.Second)
}

type deviceEntry struct {
	Mac      string  `json:"mac"`
	IP       *string `json:"ip"`
	Hostname *string `json:"hostname"`
	Vendor   *string `json:"vendor"`    // best-effort, from the device's MAC - see oui.go
	MdnsName *string `json:"mdns_name"` // best-effort, only looked up when Hostname is empty - see mdns.go
}

type deviceMonitorState struct {
	Devices   []deviceEntry `json:"devices"`
	Whitelist []string      `json:"whitelist"`
	Notify    notifyConfig  `json:"notify"`
}

func getDeviceMonitorState() (deviceMonitorState, error) {
	leasesRaw, err := run("cat /tmp/dhcp.leases 2>/dev/null || true", 0)
	if err != nil {
		return deviceMonitorState{}, err
	}
	var devices []deviceEntry
	for _, line := range strings.Split(strings.TrimSpace(leasesRaw), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			mac := strings.ToUpper(parts[1])
			ip := parts[2]
			var hostname *string
			if parts[3] != "*" {
				h := parts[3]
				hostname = &h
			}
			entry := deviceEntry{Mac: mac, IP: &ip, Hostname: hostname}
			if org, ok := macVendor(mac); ok {
				entry.Vendor = &org
			}
			devices = append(devices, entry)
		}
	}
	fillMdnsNames(devices)

	whitelistRaw, err := run("cat "+knownMacsFile+" 2>/dev/null || true", 0)
	if err != nil {
		return deviceMonitorState{}, err
	}
	seen := map[string]bool{}
	var whitelist []string
	for _, l := range strings.Split(strings.TrimSpace(whitelistRaw), "\n") {
		l = strings.ToUpper(strings.TrimSpace(l))
		if l != "" && !seen[l] {
			seen[l] = true
			whitelist = append(whitelist, l)
		}
	}
	sort.Strings(whitelist)
	return deviceMonitorState{Devices: devices, Whitelist: whitelist, Notify: getNotifyConfig()}, nil
}

// setDeviceWhitelist fully replaces the MAC whitelist on the router
// (device_monitor.sh reads it fresh on every cron run - nothing else needs
// restarting).
func setDeviceWhitelist(macs []string) (deviceMonitorState, error) {
	seen := map[string]bool{}
	var clean []string
	for _, m := range macs {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m != "" && macRe.MatchString(m) && !seen[m] {
			seen[m] = true
			clean = append(clean, m)
		}
	}
	sort.Strings(clean)
	content := ""
	if len(clean) > 0 {
		content = strings.Join(clean, "\n") + "\n"
	}
	if _, err := run("mkdir -p "+devmonDir, 0); err != nil {
		return deviceMonitorState{}, err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	if _, err := run(fmt.Sprintf("echo %s | base64 -d > %s", b64, knownMacsFile), 0); err != nil {
		return deviceMonitorState{}, err
	}
	return getDeviceMonitorState()
}

// ------------------------------------------------------------ Root password ---

const saltAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomSalt(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	salt := make([]byte, n)
	for i, b := range raw {
		salt[i] = saltAlphabet[int(b)%len(saltAlphabet)]
	}
	return string(salt), nil
}

// setRootPassword changes the router's root password by computing a fresh
// MD5-crypt hash with the router's own `openssl` (so this doesn't need to
// reimplement crypt(3) locally) and writing it directly to
// /data/etc/shadow - NOT via the /etc/shadow symlink, which sits on this
// router's ramfs-mounted /etc and silently drops any change made through
// it on the next reboot. This is the fix that was found and confirmed to
// actually survive a reboot (unlike the naive `passwd`-style approach),
// after an earlier attempt without it quietly reverted to the factory
// password.
//
// The .env file is updated in the same call so the GUI's own self-heal
// fallback (which needs the current password to reinstall a lost SSH key)
// keeps working after this - a password change that only lives in the
// router and not here would make self-heal fail the next time it's needed.
func setRootPassword(newPassword string) error {
	if len(newPassword) < 4 {
		return routerErrf("Password should be at least 4 characters")
	}
	salt, err := randomSalt(8)
	if err != nil {
		return routerErrf("Could not generate a salt: %v", err)
	}
	pwB64 := base64.StdEncoding.EncodeToString([]byte(newPassword))
	// The password travels base64-encoded so arbitrary characters in it
	// (quotes, $, backticks, ...) can't interfere with the remote shell's
	// own quoting - openssl only ever sees it after the remote `base64 -d`
	// has decoded it back to the original bytes.
	cmd := fmt.Sprintf(
		`NEWHASH=$(openssl passwd -1 -salt %s "$(echo %s | base64 -d)") && sed -i "s#^root:[^:]*:#root:${NEWHASH}:#" /data/etc/shadow && echo PASSWORD_CHANGED`,
		salt, pwB64,
	)
	out, err := run(cmd, 10*time.Second)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "PASSWORD_CHANGED") {
		return routerErrf("The router did not confirm the change: %s", strings.TrimSpace(out))
	}
	routerPass = newPassword
	if err := setEnvValue("ROUTER_ROOT_PASSWORD", newPassword); err != nil {
		return routerErrf("Password changed on the router, but could not update .env (self-heal will use the old password until this is fixed): %v", err)
	}
	return nil
}

// --------------------------------------------------------------- Raw exec ---

// rawShell runs an arbitrary shell command - for the "advanced" tab. Use
// with care.
func rawShell(cmd string) (string, error) {
	return run(cmd, 30*time.Second)
}
