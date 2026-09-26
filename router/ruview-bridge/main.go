// ruview-bridge pulls raw CFR dump files off the router (as produced by
// cfr_capture_daemon.sh + the stock /usr/sbin/cfr_test_app) over SSH,
// extracts real (non-empty) CFR records the same way ../cfr-to-rvcsi does,
// and re-encodes each one as a UDP packet for RuView's own
// wifi-densepose-sensing-server (github.com/ruvnet/ruview) to ingest on
// the one UDP port it listens on for every hardware source (default 5005)
// - the receiver tells the formats apart by their first 4 magic bytes
// (v2/crates/wifi-densepose-sensing-server/src/main.rs, udp_receiver_task).
//
// Two of those formats are relevant here, selectable with -format:
//
//   - esp32 (default): RuView's own ESP32 raw-CSI frame (ADR-018, magic
//     0xC5110001; see sensing-server/src/csi.rs parse_esp32_frame). This
//     is the only format that reaches the *sensing pipeline* - per-node
//     state, presence/motion, breathing estimate, the "Sensing" tab's node
//     list. Every associated Wi-Fi peer this router captured against is
//     presented as one "node". Confirmed live: with qcs1 alone the
//     dashboard said "Live" but showed 0 nodes and zero metrics forever,
//     because of the next point.
//   - qcs1: RuView's Qualcomm CSI frame (v2/crates/wifi-densepose-hardware/
//     src/qualcomm_csi.rs). Honest about the chipset, but sensing-server
//     only stores it as a snapshot for /api/v1/csi/qualcomm/latest and
//     never feeds it to the pipeline. Kept for that endpoint.
//   - both: send each record in both encodings.
//
// # ESP32 wire format (csi.rs parse_esp32_frame, all little-endian)
//
//	0x00 u32  magic = 0xC5110001
//	0x04 u8   node_id (assigned here per peer MAC, 1, 2, 3... in order
//	          of first appearance - printed to stdout when assigned)
//	0x05 u8   n_antennas
//	0x06 u16  n_subcarriers (per antenna)
//	0x08 u32  freq_mhz
//	0x0c u32  sequence (per node)
//	0x10 i8   rssi_dbm (strongest chain's value, see encodeESP32)
//	0x11 i8   noise_floor_dbm (unknown, 0)
//	0x12 u8   ppdu_type (0 = HT/legacy)
//	0x13 u8   flags (0)
//	0x14 ...  (I,Q) int8 pairs, antenna-major
//
// The router's records are int16 I/Q with up to 2049 subcarriers per
// chain; ESP32 frames are int8 with ≤256 bins (what the pipeline was tuned
// on), so each record is decimated to ≤256 evenly spaced subcarriers per
// chain and amplitude-scaled into int8 range. That scale is chosen once
// per node from its first frame (95th-percentile amplitude -> 96) and then
// frozen: a per-frame normalisation would erase exactly the frame-to-frame
// amplitude changes the presence/motion detection is looking for.
//
// # QCS1 wire format (read directly from RuView's own source - see
// qualcomm_csi.rs CsiFrame::to_bytes/from_bytes, not guessed)
//
// 72-byte header, all fields little-endian, followed by an optional
// per-chain RSSI byte array, then interleaved (I,Q) int16 pairs (one pair
// per tx*rx*subcarrier element), then a 4-byte IEEE CRC-32 over everything
// before it:
//
//	0x00 u32   magic = 0x31534351 ("QCS1")
//	0x04 u8    version = 1
//	0x05 u8    report_kind = 1 (Csi)
//	0x06 u16   header_len = 72
//	0x08 u32   frame_len = 72 + payload_len + 4
//	0x0c u32   sequence
//	0x10 u64   timestamp_us
//	0x18 u64   device_id
//	0x20 u16   chipset (1=QCA9300, 2=QCN9074, 3=QCN9274 - none of these is
//	           this router's actual IPQ5018+QCN9000; QCN9274 is used as the
//	           closest available profile, see package doc in ../cfr-to-rvcsi)
//	0x22 u16   bandwidth_mhz
//	0x24 u32   center_freq_khz
//	0x28 u16   flags (0 here)
//	0x2a u8    tx_count
//	0x2b u8    rx_count
//	0x2c u8    element_format = 1 (ComplexI16)
//	0x2d u8    ppdu_type (2=Vht, used here)
//	0x2e u16   subcarrier_count
//	0x30 u8    rssi_count (len of the per-chain RSSI byte array)
//	0x31 u8    noise_floor_dbm (as u8; i8 semantically)
//	0x32 u16   padding = 0
//	0x34 f32   scale
//	0x38 f32   subcarrier_spacing_hz
//	0x3c u32   calibration_id
//	0x40 u32   payload_len (rssi bytes + 4*elements)
//	0x44 u32   padding = 0
//
// This program never builds or runs any of RuView's own Rust code - it
// only re-implements this one wire format in Go from reading the source,
// and talks to whatever sensing-server the user has built and started
// themselves (see ../../ruview.sh).
package main

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"
)

// --- raw CFR record parsing (mirrors ../cfr-to-rvcsi/main.go) -------------

var (
	magicHeader = [4]byte{0xaf, 0xbe, 0xad, 0xde} // 0xDEADBEAF on disk
)

const (
	offField1         = 0x04
	offMAC            = 0x2c
	fixedHdrLen       = 0x32
	successPayloadOff = 0xc0
	chainRSSIOff      = 0x40 // 4x int32 LE, per-chain RSSI-looking values (see ../cfr-to-rvcsi doc)
)

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

type rawRecord struct {
	mac          string
	rssiPerChain []int8
	payload      []byte // IQ region only, from successPayloadOff to end
}

func splitRecords(buf []byte) []rawRecord {
	var starts []int
	for i := 0; ; {
		idx := indexOf(buf[i:], magicHeader[:])
		if idx < 0 {
			break
		}
		starts = append(starts, i+idx)
		i += idx + 4
	}
	var records []rawRecord
	for n, start := range starts {
		end := len(buf)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		body := buf[start:end]
		if len(body) < fixedHdrLen {
			continue
		}
		tail := body[fixedHdrLen:]
		if isAllZero(tail) {
			continue // capture-failure record (see cfr-trigger's own doc)
		}
		if len(body) <= successPayloadOff {
			continue
		}
		mac := net.HardwareAddr(body[offMAC : offMAC+6]).String()
		var rssi []int8
		if len(body) >= chainRSSIOff+16 {
			for c := 0; c < 4; c++ {
				v := int32(binary.LittleEndian.Uint32(body[chainRSSIOff+c*4:]))
				if v >= -128 && v <= 127 {
					rssi = append(rssi, int8(v))
				}
			}
		}
		records = append(records, rawRecord{
			mac:          mac,
			rssiPerChain: rssi,
			payload:      append([]byte{}, body[successPayloadOff:]...),
		})
	}
	return records
}

// --- ESP32 raw-CSI encoder (see package doc) ---------------------------

const (
	esp32Magic         = 0xC5110001
	esp32MaxSubcarrier = 256
	// Mean amplitude each node is scaled to. RuView's per-frame heuristics
	// use absolute thresholds (motion saturates at a within-frame spread of
	// 25, variance at 10 - they grow with amplitude squared), tuned on ESP32
	// levels: its own simulator emits a mean of ~15 with ~25% spread. At the
	// old scaling (95th percentile -> 96) every frame read as heavy motion,
	// the procedural skeleton "walked" nonstop and its tracker spawned 3-5
	// phantom figures (confirmed live).
	esp32TargetMean = 15.0
	// Frames averaged per node before its chain gains are frozen.
	esp32GainFrames = 20
)

// node is what one router-side peer looks like to RuView: a numbered
// ESP32 "node" with its own sequence counter and frozen amplitude scale.
type node struct {
	id  uint8
	seq uint32
	// Per-chain gains mapping each receive chain's mean amplitude to
	// esp32TargetMean. The chains come with different AGC gains, and
	// concatenated as-is those offsets look like huge frequency
	// selectivity to RuView. Estimated over the first esp32GainFrames
	// frames, then frozen: a per-frame normalisation would erase exactly
	// the amplitude changes that motion and breathing produce.
	gains     []float64
	chainSum  []float64
	gainCount int
	// Frame shape fixed by the first frame sent for this node (chains,
	// subcarriers per chain); anything else is skipped - see esp32Samples
	// for why a shape change is fatal to the node in RuView.
	chains, perChain int
}

var nodes = map[string]*node{}

func nodeFor(mac string, rssiPerChain []int8) *node {
	if n, ok := nodes[mac]; ok {
		return n
	}
	if len(nodes) >= 255 {
		return nil
	}
	n := &node{id: uint8(len(nodes) + 1)}
	nodes[mac] = n
	fmt.Printf("node %d = peer %s (per-chain RSSI-looking values in its first capture: %v)\n", n.id, mac, rssiPerChain)
	return n
}

type polar struct{ amp, phase float64 }

// Where the channel actually is inside this router's CFR records -
// measured on live captures (mean amplitude per entry: noise sits around
// 5-15, tones at several hundred; frame-to-frame profile correlation 0.99
// on tones vs 0.0-0.4 elsewhere), not taken from documentation:
//
//   - 5 GHz radio, 8388-byte records = 2049 int16 I/Q pairs: one leading
//     pair, then 4 blocks of 512, one per receive chain.
//   - 2.4 GHz radio, 1060-byte records = 217 pairs: one leading pair,
//     then 2 blocks of 108.
//
// In every block exactly 52 entries carry signal: 26 + 26 around one null
// - the 20 MHz OFDM tone grid (-26..-1, +1..+26, DC empty). The firmware
// measures the channel on the peer's 20 MHz (duplicate) response frames,
// so the rest of the buffer is noise whatever bandwidth argument the
// capture is started with (tried 0-3: no additional stable tones).
// Feeding the whole buffer, as earlier versions did, buried 52 real tones
// per chain under 460 noise entries and wrecked every per-frame statistic
// RuView computes (its signal-quality metric saw a coefficient of
// variation of ~2 instead of the ~0.15 it expects).
type cfrLayout struct {
	pairs, chains, block, first int
	toneLo, toneHi, dc          int // tone window [toneLo, toneHi) relative to each block, minus the DC null
}

var cfrLayouts = []cfrLayout{
	{pairs: 2049, chains: 4, block: 512, first: 1, toneLo: 133, toneHi: 186, dc: 159},
	{pairs: 217, chains: 2, block: 108, first: 1, toneLo: 53, toneHi: 106, dc: 79},
}

func esp32Samples(rec rawRecord) (int, int, []polar, error) {
	n := len(rec.payload) / 4 // (I,Q) int16 pairs
	if n == 0 {
		return 0, 0, nil, fmt.Errorf("empty payload")
	}
	for _, l := range cfrLayouts {
		if n != l.pairs {
			continue
		}
		kept := l.toneHi - l.toneLo - 1
		samples := make([]polar, 0, l.chains*kept)
		for c := 0; c < l.chains; c++ {
			base := l.first + c*l.block
			for t := l.toneLo; t < l.toneHi; t++ {
				if t == l.dc {
					continue
				}
				k := base + t
				i := float64(int16(binary.LittleEndian.Uint16(rec.payload[k*4:])))
				q := float64(int16(binary.LittleEndian.Uint16(rec.payload[k*4+2:])))
				samples = append(samples, polar{math.Hypot(i, q), math.Atan2(q, i)})
			}
		}
		return l.chains, kept, samples, nil
	}
	// Any other record size is skipped rather than decimated as before:
	// RuView locks each node onto the densest subcarrier grid it has ever
	// seen and silently drops every frame on a sparser one, so a single
	// odd-sized record (sent as up to 256 bins) switched the node off for
	// good - its stream froze on the last accepted frame while this bridge
	// kept sending (confirmed live: node stale after ~200 s, a synthetic
	// node on the same port still accepted).
	return 0, 0, nil, fmt.Errorf("unmapped CFR record size (%d I/Q pairs) - skipped", n)
}

// encodeESP32 builds one output frame from consecutive records of one
// peer (usually just one). With several, amplitudes are averaged per
// sample and the newest record's phase is kept: every CFR snapshot has
// its own random phase offset, so a complex average would cancel out,
// while an amplitude average only lowers noise. That is what capturing
// faster than RuView's 50 Hz ceiling buys (see maxNodeHz).
func encodeESP32(group []rawRecord, nd *node, freqMHz uint16) ([]byte, error) {
	rec := group[len(group)-1]
	rxCount, kept, samples, err := esp32Samples(rec)
	if err != nil {
		return nil, err
	}
	if nd.chains == 0 {
		nd.chains, nd.perChain = rxCount, kept
	} else if rxCount != nd.chains || kept != nd.perChain {
		return nil, fmt.Errorf("frame shape %dx%d differs from this node's %dx%d - skipped", rxCount, kept, nd.chains, nd.perChain)
	}
	used := 1
	for _, o := range group[:len(group)-1] {
		rc, kc, other, err := esp32Samples(o)
		if err != nil || rc != rxCount || kc != kept {
			continue
		}
		for i := range samples {
			samples[i].amp += other[i].amp
		}
		used++
	}
	if used > 1 {
		for i := range samples {
			samples[i].amp /= float64(used)
		}
	}
	if nd.gainCount < esp32GainFrames {
		if len(nd.chainSum) != rxCount {
			nd.chainSum = make([]float64, rxCount)
			nd.gainCount = 0
		}
		nd.gainCount++
		nd.gains = make([]float64, rxCount)
		for c := 0; c < rxCount; c++ {
			sum := 0.0
			for _, p := range samples[c*kept : (c+1)*kept] {
				sum += p.amp
			}
			nd.chainSum[c] += sum / float64(kept)
			mean := nd.chainSum[c] / float64(nd.gainCount)
			if mean <= 0 {
				mean = 1
			}
			nd.gains[c] = esp32TargetMean / mean
		}
	}
	for i := range samples {
		c := i / kept
		if c < len(nd.gains) {
			samples[i].amp *= nd.gains[c]
		}
	}

	out := make([]byte, 20, 20+len(samples)*2)
	binary.LittleEndian.PutUint32(out[0:], esp32Magic)
	out[4] = nd.id
	out[5] = byte(rxCount)
	binary.LittleEndian.PutUint16(out[6:], uint16(kept))
	binary.LittleEndian.PutUint32(out[8:], uint32(freqMHz))
	nd.seq++
	binary.LittleEndian.PutUint32(out[12:], nd.seq)
	// Chain 0 reads 0 on this router while chains 1-3 carry real dBm
	// values (e.g. [0 -71 -76 -79], seen live) - report the strongest
	// chain that has one; 0 would be an implausible RSSI to RuView
	// (is_plausible_rssi) and get ignored.
	var rssi int8
	for _, v := range rec.rssiPerChain {
		if v < 0 && (rssi == 0 || v > rssi) {
			rssi = v
		}
	}
	out[16] = byte(rssi)
	// out[17..19]: noise floor unknown, PPDU type 0 (HT/legacy), no flags.
	for _, s := range samples {
		a := math.Min(127, s.amp)
		out = append(out,
			byte(int8(math.Round(a*math.Cos(s.phase)))),
			byte(int8(math.Round(a*math.Sin(s.phase)))))
	}
	return out, nil
}

// --- QCS1 encoder -----------------------------------------------------

const (
	qcs1Magic     = 0x31534351
	qcs1Version   = 1
	qcs1HeaderLen = 72
	qcs1ReportCSI = 1

	chipsetQcn9274    = 3
	elementComplexI16 = 1
	ppduVht           = 2
)

var seq uint32

func encodeQCS1(rec rawRecord, deviceID uint64, bandwidthMHz uint16, centerFreqKHz uint32) ([]byte, error) {
	n := len(rec.payload) / 4 // (I,Q) int16 pairs
	if n == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	rxCount := 1
	if n%4 == 0 {
		rxCount = 4
	}
	subcarrierCount := n / rxCount
	txCount := 1

	rssi := rec.rssiPerChain
	if len(rssi) > rxCount {
		rssi = rssi[:rxCount]
	}

	payloadLen := len(rssi) + n*4
	frameLen := qcs1HeaderLen + payloadLen + 4

	out := make([]byte, qcs1HeaderLen)
	binary.LittleEndian.PutUint32(out[0x00:], qcs1Magic)
	out[0x04] = qcs1Version
	out[0x05] = qcs1ReportCSI
	binary.LittleEndian.PutUint16(out[0x06:], qcs1HeaderLen)
	binary.LittleEndian.PutUint32(out[0x08:], uint32(frameLen))
	seq++
	binary.LittleEndian.PutUint32(out[0x0c:], seq)
	binary.LittleEndian.PutUint64(out[0x10:], uint64(time.Now().UnixMicro()))
	binary.LittleEndian.PutUint64(out[0x18:], deviceID)
	binary.LittleEndian.PutUint16(out[0x20:], chipsetQcn9274)
	binary.LittleEndian.PutUint16(out[0x22:], bandwidthMHz)
	binary.LittleEndian.PutUint32(out[0x24:], centerFreqKHz)
	binary.LittleEndian.PutUint16(out[0x28:], 0) // flags
	out[0x2a] = byte(txCount)
	out[0x2b] = byte(rxCount)
	out[0x2c] = elementComplexI16
	out[0x2d] = ppduVht
	binary.LittleEndian.PutUint16(out[0x2e:], uint16(subcarrierCount))
	out[0x30] = byte(len(rssi))
	out[0x31] = 0 // noise_floor_dbm, unknown
	binary.LittleEndian.PutUint16(out[0x32:], 0)
	putF32(out[0x34:], 1.0) // scale

	// subcarrier_spacing_hz: confirmed live that sensing-server rejects a
	// zero value here ("non-finite or non-positive numeric metadata/value"),
	// so this must be a genuine positive number - 312500 Hz (312.5 kHz) is
	// the standard 802.11 OFDM subcarrier spacing (a/g/n); not calculated
	// from this router's actual bandwidth/subcarrier-count combination,
	// just a plausible non-zero placeholder that satisfies validation.
	putF32(out[0x38:], 312500)
	binary.LittleEndian.PutUint32(out[0x3c:], 0) // calibration_id
	binary.LittleEndian.PutUint32(out[0x40:], uint32(payloadLen))
	binary.LittleEndian.PutUint32(out[0x44:], 0)

	buf := make([]byte, 0, frameLen)
	buf = append(buf, out...)
	for _, r := range rssi {
		buf = append(buf, byte(r))
	}
	buf = append(buf, rec.payload[:n*4]...)

	crc := crc32.ChecksumIEEE(buf)
	crcBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(crcBytes, crc)
	buf = append(buf, crcBytes...)
	return buf, nil
}

func putF32(b []byte, v float32) {
	binary.LittleEndian.PutUint32(b, f32bits(v))
}

func f32bits(v float32) uint32 {
	return math.Float32bits(v)
}

// --- SSH plumbing (shell out to the system ssh, same approach as ../../gui
// and every other tool in this repo - see the main README) ---------------

type router struct {
	keyPath string
	host    string
}

func (r *router) run(cmd string) (string, error) {
	args := []string{
		"-i", r.keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "PubkeyAcceptedKeyTypes=+ssh-rsa",
		"-o", "HostKeyAlgorithms=+ssh-rsa",
		"-o", "ConnectTimeout=5",
		r.host, cmd,
	}
	out, err := exec.Command("ssh", args...).Output()
	return string(out), err
}

// dumpFile is one finished cfr_test_app output file.
type dumpFile struct {
	name string
	data []byte
}

// takeCompleted fetches and deletes every finished dump file in one ssh
// round-trip, oldest first. The newest file of each radio is skipped:
// cfr_capture_daemon.sh has a cfr_test_app writing into it right now.
// Taking it early lost the rest of that file (the reader kept writing
// into the deleted inode), which is why earlier versions saw noticeably
// fewer frames than the firmware reported capturing. One ssh call also
// replaces the old "ls, then fetch" pair, each call to this router costs
// a few hundred ms.
func (r *router) takeCompleted() ([]dumpFile, error) {
	remote := `cd /tmp || exit 1
for rd in wifi0 wifi1; do ls cfr_dump_${rd}_*.bin 2>/dev/null | sort | sed '$d'; done > .ruview_take
if [ -s .ruview_take ]; then tar cf - $(cat .ruview_take) && rm -f $(cat .ruview_take); fi`
	args := []string{
		"-i", r.keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "PubkeyAcceptedKeyTypes=+ssh-rsa",
		"-o", "HostKeyAlgorithms=+ssh-rsa",
		"-o", "ConnectTimeout=5",
		r.host, remote,
	}
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []dumpFile
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, fmt.Errorf("tar stream: %w", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return files, fmt.Errorf("tar entry %s: %w", hdr.Name, err)
		}
		files = append(files, dumpFile{name: path.Base(hdr.Name), data: data})
	}
	// cfr_test_app names files by start time, so name order is capture order.
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func main() {
	keyPath := flag.String("key", "", "path to router SSH private key (router_key)")
	host := flag.String("host", "root@192.168.31.1", "router SSH target")
	udpAddr := flag.String("udp", "127.0.0.1:5005", "sensing-server UDP address")
	rotationSeconds := flag.Float64("rotation", 2, "seconds each router-side dump file covers (cfr_capture_daemon.sh POLL_SECONDS)")
	maxNodeHz := flag.Float64("max-node-hz", 50, "frames per second per node sent to RuView at most; faster captures are amplitude-averaged down to this (RuView's vitals clamp each node to 50 Hz and size their windows from it)")
	bandwidthMHz := flag.Uint("bandwidth-mhz", 80, "bandwidth of the captured radio, for the QCS1 header")
	centerFreqMHz := flag.Uint("center-freq-mhz", 5180, "center frequency of the captured radio (channel 36 = 5180), for the frame header")
	format := flag.String("format", "esp32", "UDP frame format: esp32 (feeds RuView's sensing pipeline), qcs1 (Qualcomm snapshot endpoint only), or both - see package doc")
	flag.Parse()
	if *format != "esp32" && *format != "qcs1" && *format != "both" {
		fmt.Fprintf(os.Stderr, "-format must be esp32, qcs1 or both (got %q)\n", *format)
		os.Exit(2)
	}

	if *keyPath == "" {
		fmt.Fprintln(os.Stderr, "usage: ruview-bridge -key /path/to/router_key [-host root@192.168.31.1] [-udp 127.0.0.1:5005]")
		os.Exit(2)
	}

	r := &router{keyPath: *keyPath, host: *host}
	conn, err := net.Dial("udp", *udpAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "udp dial %s: %v\n", *udpAddr, err)
		os.Exit(1)
	}
	defer conn.Close()

	// Whatever is already in the router's /tmp is backlog from before this
	// program started (confirmed live: hundreds of files after hours of
	// the daemon running unattended). Nobody will ever read it, and /tmp
	// is RAM, so drop it rather than replaying stale data.
	if _, err := r.run("rm -f /tmp/cfr_dump_wifi*.bin"); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup of pre-existing dump files: %v\n", err)
	}
	rotation := time.Duration(*rotationSeconds * float64(time.Second))
	fmt.Printf("ruview-bridge: pulling finished captures from %s, forwarding to %s, at most %.0f frames/s per node\n",
		*host, *udpAddr, *maxNodeHz)

	// Fetching runs on its own goroutine so it overlaps with the paced
	// sending below. Done one after the other, every batch took its own
	// capture span to send plus the download time (~2 s per 8 MB over
	// this router's SSH), so the bridge fell further behind with every
	// batch (confirmed live: batches grew 2 -> 4 -> 6 -> 8 files).
	batches := make(chan []dumpFile, 8)
	go func() {
		for {
			files, err := r.takeCompleted()
			if err != nil {
				fmt.Fprintf(os.Stderr, "fetch: %v\n", err)
				time.Sleep(time.Second)
				continue
			}
			if len(files) == 0 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			batches <- files
		}
	}()

	for files := range batches {
		units, span, rawCount := planFrames(files, rotation, *maxNodeHz)

		// Replay evenly across the time the files cover, in capture order
		// across all nodes: sensing-server measures each node's sample
		// rate from arrival times, and its vital-sign windows depend on
		// that being right.
		// Send slightly faster than real time so small timing drift can
		// never build a backlog, and much faster if batches are already
		// queued up behind this one.
		budget := span * 95 / 100
		if q := len(batches); q > 0 {
			budget = span / time.Duration(q+1)
		}
		gap := time.Duration(0)
		if len(units) > 1 {
			gap = budget / time.Duration(len(units))
		}
		sent := map[string]int{}
		skipped := map[string]int{} // reason -> count, reported in the batch line
		for i, u := range units {
			if i > 0 && gap > 0 {
				time.Sleep(gap)
			}
			if *format != "qcs1" {
				nd := nodeFor(u.mac, u.group[len(u.group)-1].rssiPerChain)
				if nd == nil {
					fmt.Fprintf(os.Stderr, "peer %s: more than 255 distinct peers, ESP32 node ids exhausted\n", u.mac)
				} else if pkt, err := encodeESP32(u.group, nd, uint16(*centerFreqMHz)); err != nil {
					skipped[u.mac+": "+err.Error()]++
				} else if _, err := conn.Write(pkt); err != nil {
					fmt.Fprintf(os.Stderr, "send %s: %v\n", u.mac, err)
				} else {
					sent[u.mac]++
				}
			}
			if *format != "esp32" {
				pkt, err := encodeQCS1(u.group[len(u.group)-1], macToUint64(u.mac), uint16(*bandwidthMHz), uint32(*centerFreqMHz)*1000)
				if err == nil {
					_, err = conn.Write(pkt)
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "qcs1 %s: %v\n", u.mac, err)
				} else if *format == "qcs1" {
					sent[u.mac]++
				}
			}
		}
		// One summary line per batch instead of one per frame (at ~50 Hz
		// per node the per-frame log grew by megabytes per hour).
		var parts []string
		for mac, n := range sent {
			id := 0
			if nd := nodes[mac]; nd != nil {
				id = int(nd.id)
			}
			parts = append(parts, fmt.Sprintf("node%d(%s)=%.1fHz", id, mac, float64(n)/span.Seconds()))
		}
		sort.Strings(parts)
		fmt.Printf("%s batch: %d file(s), %d records -> %d frames over %.1fs: %s\n",
			time.Now().Format("15:04:05"), len(files), rawCount, len(units), span.Seconds(), strings.Join(parts, " "))
		for why, n := range skipped {
			fmt.Printf("  skipped %d: %s\n", n, why)
		}
	}
}

// frameUnit is one frame to send: one or more consecutive records of a peer.
type frameUnit struct {
	mac   string
	group []rawRecord
	pos   float64 // position on the capture timeline, in units of files
}

func radioOf(name string) string {
	rest := strings.TrimPrefix(name, "cfr_dump_")
	if i := strings.Index(rest, "_"); i > 0 {
		return rest[:i]
	}
	return rest
}

// planFrames turns a batch of dump files into timeline-ordered frames. Each
// file covers one rotation period of one radio; files of different radios
// cover the same wall-clock time, so the batch spans (most files of any
// one radio) x rotation. Per peer and file, records beyond maxHz are
// folded into equal consecutive groups that encodeESP32 averages.
func planFrames(files []dumpFile, rotation time.Duration, maxHz float64) ([]frameUnit, time.Duration, int) {
	fileIdx := map[string]int{}
	var units []frameUnit
	raw := 0
	capPerFile := int(maxHz * rotation.Seconds())
	if capPerFile < 1 {
		capPerFile = 1
	}
	for _, f := range files {
		radio := radioOf(f.name)
		idx := fileIdx[radio]
		fileIdx[radio] = idx + 1
		byPeer := map[string][]rawRecord{}
		var order []string
		for _, rec := range splitRecords(f.data) {
			if _, ok := byPeer[rec.mac]; !ok {
				order = append(order, rec.mac)
			}
			byPeer[rec.mac] = append(byPeer[rec.mac], rec)
			raw++
		}
		for _, mac := range order {
			recs := byPeer[mac]
			bins := len(recs)
			if bins > capPerFile {
				bins = capPerFile
			}
			for k := 0; k < bins; k++ {
				lo, hi := k*len(recs)/bins, (k+1)*len(recs)/bins
				units = append(units, frameUnit{
					mac:   mac,
					group: recs[lo:hi],
					pos:   float64(idx) + (float64(k)+0.5)/float64(bins),
				})
			}
		}
	}
	sort.SliceStable(units, func(i, j int) bool { return units[i].pos < units[j].pos })
	most := 0
	for _, n := range fileIdx {
		if n > most {
			most = n
		}
	}
	return units, rotation * time.Duration(most), raw
}

func macToUint64(mac string) uint64 {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return 0
	}
	var v uint64
	for _, b := range hw {
		v = v<<8 | uint64(b)
	}
	return v
}
