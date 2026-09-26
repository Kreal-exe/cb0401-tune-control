package main

// Raw CFR records as the router's stock /usr/sbin/cfr_test_app writes them
// to /tmp/cfr_dump_wifi{0,1}_*.bin (armed by router/cfr_capture_daemon.sh).
// Everything here was reverse-engineered on the CB0401's own hardware; see
// router/cfr-trigger for how capture is switched on.

import (
	"encoding/binary"
	"math"
	"net"
)

var magicHeader = [4]byte{0xaf, 0xbe, 0xad, 0xde} // 0xDEADBEAF on disk

const (
	offMAC            = 0x2c // peer MAC address (6 bytes)
	fixedHdrLen       = 0x32
	successPayloadOff = 0xc0 // I/Q pairs start here in a successful record
	chainRSSIOff      = 0x40 // 4 x int32 LE per-chain RSSI (chain 0 reads 0 on this router)
)

type rawRecord struct {
	mac     string
	rssi    int8   // strongest chain's RSSI in dBm, 0 if unknown
	payload []byte // I/Q region, from successPayloadOff to the end
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

// splitRecords cuts one dump file into records and drops the empty ones
// (a failed capture - e.g. the peer was in power save - leaves a header
// with an all-zero body).
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
		if len(body) <= successPayloadOff || isAllZero(body[fixedHdrLen:]) {
			continue
		}
		var rssi int8
		if len(body) >= chainRSSIOff+16 {
			for c := 0; c < 4; c++ {
				v := int32(binary.LittleEndian.Uint32(body[chainRSSIOff+c*4:]))
				if v < 0 && v >= -128 && (rssi == 0 || int8(v) > rssi) {
					rssi = int8(v)
				}
			}
		}
		records = append(records, rawRecord{
			mac:     net.HardwareAddr(body[offMAC : offMAC+6]).String(),
			rssi:    rssi,
			payload: body[successPayloadOff:],
		})
	}
	return records
}

// Where the channel actually is inside a record, measured on live captures
// (tones sit at several hundred counts with a 0.975-0.998 frame-to-frame
// profile correlation; everything else is noise around 5-15):
//
//   - 5 GHz radio: 8388-byte records = 2049 int16 I/Q pairs, one leading
//     pair, then 4 blocks of 512 - one per receive chain.
//   - 2.4 GHz radio: 1060-byte records = 217 pairs, one leading pair, then
//     2 blocks of 108.
//
// In every block exactly 52 entries carry signal: the 20 MHz OFDM tone grid
// (26 + 26 around the DC null). Other record sizes haven't been seen often
// enough to map and are skipped.
type cfrLayout struct {
	pairs, chains, block, first int
	toneLo, toneHi, dc          int
}

var cfrLayouts = []cfrLayout{
	{pairs: 2049, chains: 4, block: 512, first: 1, toneLo: 133, toneHi: 186, dc: 159},
	{pairs: 217, chains: 2, block: 108, first: 1, toneLo: 53, toneHi: 106, dc: 79},
}

// toneAmplitudes returns the per-tone amplitudes of one record, chain by
// chain (chains x 52 values), or nil for an unmapped record size. Phase is
// ignored: every CFR snapshot carries its own random phase offset.
func toneAmplitudes(rec rawRecord) []float64 {
	n := len(rec.payload) / 4
	for _, l := range cfrLayouts {
		if n != l.pairs {
			continue
		}
		out := make([]float64, 0, l.chains*(l.toneHi-l.toneLo-1))
		for c := 0; c < l.chains; c++ {
			base := l.first + c*l.block
			for t := l.toneLo; t < l.toneHi; t++ {
				if t == l.dc {
					continue
				}
				k := (base + t) * 4
				i := float64(int16(binary.LittleEndian.Uint16(rec.payload[k:])))
				q := float64(int16(binary.LittleEndian.Uint16(rec.payload[k+2:])))
				out = append(out, math.Hypot(i, q))
			}
		}
		return out
	}
	return nil
}
