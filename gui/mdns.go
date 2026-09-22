// One-shot reverse mDNS lookup, tried for devices that show up with no
// DHCP hostname. This runs from the GUI - your own machine - not the
// router: the GUI is itself an ordinary device on the same LAN as
// everything else here (it just also happens to manage this router over
// SSH), so it can send and receive mDNS multicast traffic directly,
// without needing anything installed on the router (which has no
// avahi/mDNS daemon and, per this project's usual constraints, nowhere to
// put one).
//
// This asks "does anyone here know a name for this IP?" the same way
// `dig -p 5353 @224.0.0.251 -x <ip>` or macOS's own `dns-sd -q` would: a
// standard DNS PTR query for `<reversed-ip>.in-addr.arpa`, sent to the
// mDNS multicast address (224.0.0.251:5353) per RFC 6762. Sending it from
// an ephemeral source port rather than 5353 itself makes it a "one-shot
// legacy unicast query" (RFC 6762 §5.1/§6.7): a compliant responder
// answers straight back to that port over unicast instead of broadcasting
// its answer to the whole multicast group, so a plain UDP read on the same
// socket we sent from is enough to catch the reply - no group membership,
// no listening daemon.
//
// A lot of devices (particularly ones using a randomized DHCP identity
// specifically to avoid being recognized) simply won't answer, or will
// answer with something just as generic - this is a best-effort hint on
// top of the DHCP hostname, not a way around a device's own privacy
// features.
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// fillMdnsNames tries an mDNS reverse lookup for every device that has no
// DHCP hostname, in parallel (each one's a couple of UDP packets and a
// short wait, not real work - doing them one at a time would mean this
// scales with how many unnamed devices you have, for no reason). A device
// that already has a hostname is left alone; there'd be nothing useful to
// add, and no reason to put more mDNS traffic on the network than needed.
func fillMdnsNames(devices []deviceEntry) {
	var wg sync.WaitGroup
	for i := range devices {
		if devices[i].Hostname != nil || devices[i].IP == nil {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if name, ok := mdnsReverseLookup(*devices[i].IP, 700*time.Millisecond); ok && name != "" {
				devices[i].MdnsName = &name
			}
		}(i)
	}
	wg.Wait()
}

// mdnsReverseLookup tries to resolve ip to a hostname via mDNS, waiting up
// to timeout for a reply. Returns ("", false) on any failure or timeout -
// callers should treat that as "no additional info available", not an
// error worth surfacing.
func mdnsReverseLookup(ip string, timeout time.Duration) (string, bool) {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil || parsedIP.To4() == nil {
		return "", false
	}
	query, err := buildPTRQuery(parsedIP.To4())
	if err != nil {
		return "", false
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return "", false
	}
	defer conn.Close()

	dst := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	if _, err := conn.WriteToUDP(query, dst); err != nil {
		return "", false
	}

	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", false // timeout or socket error - give up quietly
		}
		// Multiple mDNS responders can be chatting on this segment for
		// unrelated queries; only trust a reply that's actually from the
		// device we asked about.
		if !from.IP.Equal(parsedIP) {
			continue
		}
		if name, ok := parsePTRResponse(buf[:n]); ok {
			return name, true
		}
	}
}

// buildPTRQuery builds a minimal DNS query packet: one question, QTYPE=PTR
// (12), QCLASS=IN (1), for "<d>.<c>.<b>.<a>.in-addr.arpa" given ip a.b.c.d.
func buildPTRQuery(ip4 net.IP) ([]byte, error) {
	if len(ip4) != 4 {
		return nil, fmt.Errorf("not an IPv4 address")
	}
	name := fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", ip4[3], ip4[2], ip4[1], ip4[0])

	var b []byte
	// Header: ID=0, flags=0 (standard query), QDCOUNT=1, ANCOUNT/NSCOUNT/ARCOUNT=0.
	b = append(b, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0)
	b = append(b, encodeDNSName(name)...)
	b = append(b, 0, 12) // QTYPE  PTR
	b = append(b, 0, 1)  // QCLASS IN
	return b, nil
}

func encodeDNSName(name string) []byte {
	var b []byte
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			continue
		}
		if len(label) > 63 {
			label = label[:63]
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)
	return b
}

// parsePTRResponse pulls the first PTR record's target out of a DNS
// response and returns it with the trailing ".local."/domain suffix
// stripped down to just the hostname label - enough detail to show
// someone, not a full DNS parser (no compression-pointer support beyond
// what's needed to skip over the echoed question and any earlier answers,
// no handling of any record type but PTR).
func parsePTRResponse(msg []byte) (string, bool) {
	if len(msg) < 12 {
		return "", false
	}
	qdCount := binary.BigEndian.Uint16(msg[4:6])
	anCount := binary.BigEndian.Uint16(msg[6:8])
	if anCount == 0 {
		return "", false
	}

	off := 12
	var err error
	for i := 0; i < int(qdCount); i++ {
		off, err = skipDNSName(msg, off)
		if err != nil {
			return "", false
		}
		off += 4 // QTYPE + QCLASS
	}

	for i := 0; i < int(anCount); i++ {
		off, err = skipDNSName(msg, off)
		if err != nil {
			return "", false
		}
		if off+10 > len(msg) {
			return "", false
		}
		rtype := binary.BigEndian.Uint16(msg[off : off+2])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8 : off+10]))
		off += 10
		if off+rdlen > len(msg) {
			return "", false
		}
		if rtype == 12 { // PTR
			target, _, err := readDNSName(msg, off)
			if err != nil {
				return "", false
			}
			return cleanMdnsName(target), true
		}
		off += rdlen
	}
	return "", false
}

// cleanMdnsName turns "Kitchen-Bulb.local." into "Kitchen-Bulb".
func cleanMdnsName(name string) string {
	name = strings.TrimSuffix(name, ".")
	if i := strings.IndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	return name
}

// skipDNSName advances past a (possibly compressed) DNS name and returns
// the offset right after it, without decoding the name itself.
func skipDNSName(msg []byte, off int) (int, error) {
	for {
		if off >= len(msg) {
			return 0, fmt.Errorf("name runs past end of message")
		}
		l := int(msg[off])
		switch {
		case l == 0:
			return off + 1, nil
		case l&0xc0 == 0xc0: // compression pointer, always 2 bytes, always ends a name
			if off+2 > len(msg) {
				return 0, fmt.Errorf("truncated compression pointer")
			}
			return off + 2, nil
		default:
			off += 1 + l
		}
	}
}

// readDNSName decodes a (possibly compressed) DNS name starting at off,
// returning the decoded name and the offset right after its on-the-wire
// representation (which, for a pointer, is right after the 2-byte pointer
// itself - not wherever the pointer target ends).
func readDNSName(msg []byte, off int) (string, int, error) {
	var labels []string
	start := off
	jumped := false
	end := off
	guard := 0
	for {
		guard++
		if guard > 128 {
			return "", 0, fmt.Errorf("name too long or looped")
		}
		if start >= len(msg) {
			return "", 0, fmt.Errorf("name runs past end of message")
		}
		l := int(msg[start])
		switch {
		case l == 0:
			if !jumped {
				end = start + 1
			}
			return strings.Join(labels, "."), end, nil
		case l&0xc0 == 0xc0:
			if start+2 > len(msg) {
				return "", 0, fmt.Errorf("truncated compression pointer")
			}
			if !jumped {
				end = start + 2
			}
			ptr := int(binary.BigEndian.Uint16(msg[start:start+2]) & 0x3fff)
			if ptr >= len(msg) {
				return "", 0, fmt.Errorf("compression pointer out of range")
			}
			start = ptr
			jumped = true
		default:
			if start+1+l > len(msg) {
				return "", 0, fmt.Errorf("label runs past end of message")
			}
			labels = append(labels, string(msg[start+1:start+1+l]))
			start += 1 + l
		}
	}
}
