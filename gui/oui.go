// Vendor lookup for the Device monitor card, for when DHCP hostname is
// missing or unhelpful. This runs entirely in the GUI (on your own
// machine), not the router - there's no storage or dependency reason to
// keep it off the router, but it's the wrong place for it: it's a plain
// static lookup table, so it belongs wherever's most convenient to embed
// it, and the GUI already assembles the device list.
//
// ouidata/oui.tsv.gz is generated from IEEE's three public MAC address
// block registries (MA-L, 24-bit; MA-M, 28-bit; MA-S, 36-bit prefixes):
//
//	https://standards-oui.ieee.org/oui/oui.csv
//	https://standards-oui.ieee.org/oui28/mam.csv
//	https://standards-oui.ieee.org/oui36/oui36.csv
//
// Combined into one `<hex-prefix>\t<org name>` file, sorted, one entry per
// line, gzip-compressed. To refresh it: re-fetch those three CSVs, take
// columns 2 (Assignment) and 3 (Organization Name) from each, tab-join and
// concatenate, gzip, and replace this file - it's static data with no
// runtime dependency either way, this project just happens to have last
// generated it this way. IEEE reassigns and adds blocks continuously, so
// don't expect this to stay perfectly current - it's a best-effort hint,
// not authoritative.
package main

import (
	"bufio"
	"compress/gzip"
	"embed"
	"strings"
	"sync"
)

//go:embed ouidata/oui.tsv.gz
var ouiDataGz embed.FS

var (
	ouiOnce sync.Once
	ouiMap  map[string]string // hex prefix (6, 7, or 9 chars, uppercase) -> org name
)

func loadOuiData() {
	ouiMap = make(map[string]string, 33000)
	f, err := ouiDataGz.Open("ouidata/oui.tsv.gz")
	if err != nil {
		return // best-effort - a missing/corrupt asset just means no vendor hints
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 256), 1024)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, '\t')
		if i < 0 {
			continue
		}
		ouiMap[line[:i]] = line[i+1:]
	}
}

// macVendor returns a best-effort manufacturer name for a MAC address
// ("AA:BB:CC:DD:EE:FF" or bare hex, case-insensitive), trying the most
// specific IEEE registry block first (36-bit, then 28-bit, then 24-bit).
//
// Returns ("", false) for a locally-administered ("randomized"/private)
// address rather than attempting a lookup - the whole point of that bit is
// that the address carries no real vendor information, so a lookup
// "succeeding" there would only ever be misleading coincidence, never a
// real answer.
func macVendor(mac string) (string, bool) {
	ouiOnce.Do(loadOuiData)

	hex := make([]byte, 0, 12)
	for _, c := range mac {
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'F':
			hex = append(hex, byte(c))
		case c >= 'a' && c <= 'f':
			hex = append(hex, byte(c-'a'+'A'))
		}
	}
	if len(hex) < 6 {
		return "", false
	}

	// The second-least-significant bit of the first octet is the
	// U/L (universal/local) bit - set for a locally administered address,
	// which includes every MAC-randomization scheme in modern OSes.
	firstByte := hexNibble(hex[0])<<4 | hexNibble(hex[1])
	if firstByte&0x02 != 0 {
		return "", false
	}

	for _, n := range []int{9, 7, 6} {
		if len(hex) < n {
			continue
		}
		if org, ok := ouiMap[string(hex[:n])]; ok {
			return org, true
		}
	}
	return "", false
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 0
	}
}
