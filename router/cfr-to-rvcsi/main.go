// cfr-to-rvcsi converts a raw CFR dump file, as produced by the stock
// /usr/sbin/cfr_test_app (reading /sys/kernel/debug/qdf/cfrwifi{0,1}/cfr_dump0
// on this router), into a `.rvcsi` capture file - the JSONL container format
// RuView's rvcsi-adapter-file crate reads (github.com/ruvnet/rvcsi,
// crates/rvcsi-adapter-file/src/format.rs): a CaptureHeader JSON line
// followed by one rvcsi_core::CsiFrame JSON line per frame. This is the
// integration point RuView's own code actually has - there is no separate
// "QCS1 codec" or Qualcomm-specific adapter in the rvcsi crates as of this
// writing (docs/ADRs describe one as aspirational; the code doesn't have
// it), so a file-based handoff via the File adapter is the concrete, working
// path today. Feed the result to RuView with e.g.
// `rvcsi replay --file out.rvcsi` (see the rvcsi-cli crate) or open it with
// the `RvCsi.open({source: "file", target: "out.rvcsi"})` TS/Rust API.
//
// # Record format (reverse-engineered on this router's own hardware)
//
// Every record starts with a 4-byte little-endian magic 0xDEADBEAF; a record
// spans to the next one's magic, or EOF. Confirmed against three real
// captures: two firmware-side capture-failure events ("peer is in
// powersave" - short, 148 bytes, no payload past the MAC) and one genuine
// successful capture (8388 bytes, triggered against an actively-awake
// laptop, i.e. not in Wi-Fi powersave - see cfr-trigger's own doc comment
// for why that distinction matters at the firmware level):
//
//	offset 0x00 (u32 LE):  header magic, 0xDEADBEAF
//	offset 0x04 (u32 LE):  unidentified (identical across all records seen)
//	offset 0x08 (u32 LE):  unidentified (identical across all records seen)
//	offset 0x0c (u32 LE):  128 in every record seen, success or failure -
//	                       not a byte length (the real payload is far
//	                       larger); more likely a tone/subcarrier-group
//	                       count or similar fixed capability value
//	offset 0x10..0x2c:     zero in every record seen (reserved)
//	offset 0x2c (6 bytes): peer MAC address
//
// A *failed* capture ends right here (padded to 148 bytes with zeros,
// occasionally followed by 4 bytes that happen to read as 0xBEAFDEAD - a
// coincidence of that fixed short length, not a real footer, see
// splitRecords). A *successful* capture continues with more structure
// identified by inspection but not yet fully decoded field-by-field -
// several small per-chain values that look like signed dBm RSSI, a run of
// 0xFFFFFFFF (unused-chain markers?), what appears to be the AP's own BSSID,
// and further counters - before offset successPayloadOffset, where the
// byte pattern changes to a long, dense run of small alternating-sign
// values consistent with real I/Q components. That's the boundary this
// program treats as "payload start" for a successful record; bytes between
// the MAC and that boundary are skipped rather than misread as IQ noise.
//
// Payload interpretation past that point: consecutive little-endian int16
// (I, Q) pairs, one pair per subcarrier - the same convention RuView's own
// nexmon adapter uses for its wire format (18-byte header + int16 I/Q),
// chosen for consistency with a real, working RuView adapter rather than
// invented from nothing. The resulting subcarrier count (~2000 on the one
// successful 80 MHz capture seen) is implausibly high for a single
// antenna/tone-set, so this is very likely several receive chains and/or
// repeated tone groups concatenated rather than one flat subcarrier array -
// documented as an open question, not a solved one. Records whose payload
// is entirely zero (failed captures) are logged to stderr and are NOT
// written as CsiFrame lines (an all-zero-amplitude frame would fail
// rvcsi-core's own quality checks anyway).
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"time"
)

var (
	magicHeader = [4]byte{0xaf, 0xbe, 0xad, 0xde} // 0xDEADBEAF, little-endian on disk
	magicFooter = [4]byte{0xad, 0xde, 0xaf, 0xbe} // 0xBEAFDEAD, little-endian on disk
)

const (
	offMagic  = 0x00
	offField1 = 0x04
	offField2 = 0x08
	offLength = 0x0c
	offMAC    = 0x2c
	fixedHdrLen = 0x32 // offMAC + 6 - present in every record, success or failure
	footerLen   = 4

	// Where the dense, IQ-like byte run starts in a *successful* capture -
	// see the package doc. Everything between fixedHdrLen and this offset
	// is real but not yet decoded (per-chain RSSI, a second MAC, counters)
	// and is skipped rather than treated as payload.
	successPayloadOffset = 0xc0
)

type rawRecord struct {
	field1  uint32
	field2  uint32
	length  uint32
	mac     net.HardwareAddr
	payload []byte
}

// splitRecords scans buf for magicHeader-prefixed records. A record spans
// from its header magic to the *next* header magic, or EOF for the last
// one in the file - not to a footer magic. That was the original
// (wrong) assumption, based on two capture-failure records ("peer is in
// powersave", zero payload) where it happened to hold: each of those was
// exactly fixedHdrLen+footerLen bytes long, so "the next record's header
// magic" and "4 bytes that happen to read as 0xBEAFDEAD" landed on the
// same offset by coincidence. A real, successful capture (confirmed live -
// see package doc) has no footer magic anywhere in it at all; go looking
// for one there hangs the parser on a false "truncated" error.
func splitRecords(buf []byte) ([]rawRecord, error) {
	var records []rawRecord
	starts := []int{}
	for i := 0; ; {
		idx := indexOf(buf[i:], magicHeader[:])
		if idx < 0 {
			break
		}
		starts = append(starts, i+idx)
		i += idx + 4
	}
	for n, start := range starts {
		end := len(buf)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		body := buf[start:end]
		if len(body) < fixedHdrLen {
			return records, fmt.Errorf("record at offset %#x: too short (%d bytes)", start, len(body))
		}
		// A capture-failure record (zero payload) still carries the
		// look-alike "footer" bytes right after its fixed header block;
		// trim them off if present so they don't get treated as (fake)
		// payload for those records specifically.
		payloadEnd := len(body)
		if payloadEnd >= fixedHdrLen+footerLen {
			tail := body[payloadEnd-footerLen:]
			if tail[0] == magicFooter[0] && tail[1] == magicFooter[1] && tail[2] == magicFooter[2] && tail[3] == magicFooter[3] {
				payloadEnd -= footerLen
			}
		}
		payloadStart := fixedHdrLen
		if isAllZero(body[fixedHdrLen:payloadEnd]) {
			// A failed capture: nothing past the MAC is real, so leave
			// payloadStart at fixedHdrLen - the caller's own isAllZero
			// check on this (still all-zero) slice is what actually
			// causes it to be skipped, not this branch.
		} else if payloadEnd > successPayloadOffset {
			// A successful capture: skip the not-yet-decoded metadata
			// block between the MAC and where the IQ-like byte run
			// starts (see package doc).
			payloadStart = successPayloadOffset
		}

		rec := rawRecord{
			field1:  binary.LittleEndian.Uint32(body[offField1:]),
			field2:  binary.LittleEndian.Uint32(body[offField2:]),
			length:  binary.LittleEndian.Uint32(body[offLength:]),
			mac:     net.HardwareAddr(append([]byte{}, body[offMAC:offMAC+6]...)),
			payload: append([]byte{}, body[payloadStart:payloadEnd]...),
		}
		records = append(records, rec)
	}
	return records, nil
}

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

// --- rvcsi_core::CsiFrame / AdapterKind / ValidationStatus, mirrored -------
// (see github.com/ruvnet/rvcsi, crates/rvcsi-core/src/{frame,adapter,ids}.rs
// - field names, types and serde shape copied verbatim so the JSON this
// program emits is byte-for-byte what those Rust types deserialize.)

type csiFrame struct {
	FrameID            uint64    `json:"frame_id"`
	SessionID          uint64    `json:"session_id"`
	SourceID           string    `json:"source_id"`
	AdapterKind        string    `json:"adapter_kind"`
	TimestampNs        uint64    `json:"timestamp_ns"`
	Channel            uint16    `json:"channel"`
	BandwidthMHz       uint16    `json:"bandwidth_mhz"`
	RSSIdBm            *int16    `json:"rssi_dbm"`
	NoiseFloordBm      *int16    `json:"noise_floor_dbm"`
	AntennaIndex       *uint8    `json:"antenna_index"`
	TxChain            *uint8    `json:"tx_chain"`
	RxChain            *uint8    `json:"rx_chain"`
	SubcarrierCount    uint16    `json:"subcarrier_count"`
	IValues            []float32 `json:"i_values"`
	QValues            []float32 `json:"q_values"`
	Amplitude          []float32 `json:"amplitude"`
	Phase              []float32 `json:"phase"`
	Validation         string    `json:"validation"`
	QualityScore       float32   `json:"quality_score"`
	QualityReasons     []string  `json:"quality_reasons,omitempty"`
	CalibrationVersion *string   `json:"calibration_version"`
}

type adapterProfile struct {
	AdapterKind               string   `json:"adapter_kind"`
	Chip                      *string  `json:"chip"`
	FirmwareVersion           *string  `json:"firmware_version"`
	DriverVersion             *string  `json:"driver_version"`
	SupportedChannels         []uint16 `json:"supported_channels"`
	SupportedBandwidthsMHz    []uint16 `json:"supported_bandwidths_mhz"`
	ExpectedSubcarrierCounts  []uint16 `json:"expected_subcarrier_counts"`
	SupportsLiveCapture       bool     `json:"supports_live_capture"`
	SupportsInjection         bool     `json:"supports_injection"`
	SupportsMonitorMode       bool     `json:"supports_monitor_mode"`
}

type validationPolicy struct {
	MinSubcarriers          uint16     `json:"min_subcarriers"`
	MaxSubcarriers          uint16     `json:"max_subcarriers"`
	RSSIdBmBounds           [2]int16   `json:"rssi_dbm_bounds"`
	StrictMonotonicTime     bool       `json:"strict_monotonic_time"`
	DegradeInsteadOfReject  bool       `json:"degrade_instead_of_reject"`
	MinQuality              float32    `json:"min_quality"`
}

type captureHeader struct {
	RvcsiCaptureVersion uint32           `json:"rvcsi_capture_version"`
	SessionID           uint64           `json:"session_id"`
	SourceID            string           `json:"source_id"`
	AdapterProfile      adapterProfile   `json:"adapter_profile"`
	ValidationPolicy    validationPolicy `json:"validation_policy"`
	CalibrationVersion  *string          `json:"calibration_version"`
	RuntimeConfigJSON   string           `json:"runtime_config_json"`
	CreatedUnixNs       uint64           `json:"created_unix_ns"`
}

func strp(s string) *string { return &s }

func main() {
	in := flag.String("in", "", "raw CFR dump file (from cfr_test_app / cfr_dump0)")
	out := flag.String("out", "", "output .rvcsi file (created or appended to)")
	sourceID := flag.String("source-id", "cb0401-cfr", "source_id for the header and every frame")
	channel := flag.Uint("channel", 36, "WiFi channel the capture was taken on (not embedded in the raw record - pass what you configured)")
	bandwidth := flag.Uint("bandwidth-mhz", 80, "bandwidth in MHz the capture was taken on")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: cfr-to-rvcsi -in cfr_dump.bin -out out.rvcsi [-source-id X] [-channel N] [-bandwidth-mhz N]")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", *in, err)
		os.Exit(1)
	}

	records, err := splitRecords(raw)
	if err != nil && len(records) == 0 {
		fmt.Fprintf(os.Stderr, "parsing %s: %v\n", *in, err)
		os.Exit(1)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (kept %d well-formed record(s) before it)\n", err, len(records))
	}
	fmt.Fprintf(os.Stderr, "%s: %d record(s) found\n", *in, len(records))

	// Header line, only written if the file is new/empty.
	writeHeader := true
	if st, statErr := os.Stat(*out); statErr == nil && st.Size() > 0 {
		writeHeader = false
	}

	f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opening %s: %v\n", *out, err)
		os.Exit(1)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	if writeHeader {
		hdr := captureHeader{
			RvcsiCaptureVersion: 1,
			SessionID:           uint64(time.Now().UnixNano()),
			SourceID:            *sourceID,
			AdapterProfile: adapterProfile{
				AdapterKind:              "Atheros",
				Chip:                     strp("IPQ5018+QCN9000 (QCA QSDK offload stack)"),
				FirmwareVersion:          nil,
				DriverVersion:            nil,
				SupportedChannels:        []uint16{},
				SupportedBandwidthsMHz:   []uint16{20, 40, 80, 160},
				ExpectedSubcarrierCounts: []uint16{},
				SupportsLiveCapture:      true,
				SupportsInjection:        false,
				SupportsMonitorMode:      false,
			},
			ValidationPolicy: validationPolicy{
				MinSubcarriers:         1,
				MaxSubcarriers:         4096,
				RSSIdBmBounds:          [2]int16{-110, 0},
				StrictMonotonicTime:    false,
				DegradeInsteadOfReject: true,
				MinQuality:             0.25,
			},
			CalibrationVersion: nil,
			RuntimeConfigJSON:  "{}",
			CreatedUnixNs:      uint64(time.Now().UnixNano()),
		}
		b, _ := json.Marshal(hdr)
		w.Write(b)
		w.WriteByte('\n')
	}

	frameID := uint64(0)
	sessionID := uint64(time.Now().UnixNano())
	written := 0
	skippedEmpty := 0
	for _, rec := range records {
		if isAllZero(rec.payload) {
			skippedEmpty++
			fmt.Fprintf(os.Stderr, "  skipping empty-payload record for peer %s (declared length %d, actual payload %d bytes all zero - firmware-side capture failure, not real CFR data)\n",
				rec.mac, rec.length, len(rec.payload))
			continue
		}

		n := len(rec.payload) / 4 // int16 I + int16 Q per subcarrier
		iValues := make([]float32, n)
		qValues := make([]float32, n)
		amplitude := make([]float32, n)
		phase := make([]float32, n)
		for k := 0; k < n; k++ {
			iRaw := int16(binary.LittleEndian.Uint16(rec.payload[k*4:]))
			qRaw := int16(binary.LittleEndian.Uint16(rec.payload[k*4+2:]))
			iv := float32(iRaw)
			qv := float32(qRaw)
			iValues[k] = iv
			qValues[k] = qv
			amplitude[k] = float32(math.Sqrt(float64(iv*iv + qv*qv)))
			phase[k] = float32(math.Atan2(float64(qv), float64(iv)))
		}

		frame := csiFrame{
			FrameID:         frameID,
			SessionID:       sessionID,
			SourceID:        *sourceID,
			AdapterKind:     "Atheros",
			TimestampNs:     uint64(time.Now().UnixNano()),
			Channel:         uint16(*channel),
			BandwidthMHz:    uint16(*bandwidth),
			RSSIdBm:         nil,
			NoiseFloordBm:   nil,
			AntennaIndex:    nil,
			TxChain:         nil,
			RxChain:         nil,
			SubcarrierCount: uint16(n),
			IValues:         iValues,
			QValues:         qValues,
			Amplitude:       amplitude,
			Phase:           phase,
			Validation:      "Pending", // the runtime's validate_frame decides Accepted/Degraded/Rejected
			QualityScore:    0,
			CalibrationVersion: nil,
		}
		b, _ := json.Marshal(frame)
		w.Write(b)
		w.WriteByte('\n')
		frameID++
		written++
	}
	w.Flush()

	fmt.Fprintf(os.Stderr, "wrote %d CsiFrame line(s) to %s (%d empty/failed record(s) skipped)\n", written, *out, skippedEmpty)
}
