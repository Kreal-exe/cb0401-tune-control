// cfr-trigger sends the one nl80211 vendor command this router's stock
// firmware needs to arm CFR (Channel Frequency Response - Qualcomm's
// per-peer Wi-Fi channel-state capture, the vendor equivalent of "CSI") for
// a connected client. It exists because this firmware's own tools
// (wifitool, cfg80211tool, iwpriv) only expose CFR *read*-side commands
// (get_cfr_capture_status, get_cfr_timer) and the debugfs dump reader
// (/usr/sbin/cfr_test_app) - nothing ships that can flip the capture on.
//
// The kernel-side support is very much present and fully wired down to
// firmware (confirmed by reading the symbol tables of the router's own
// umac.ko/qca_ol.ko/wifi_3_0.ko: wlan_cfr_init, ucfg_cfr_start_capture,
// target_if_cfr_start_capture, wmi_unified_send_peer_cfr_capture_cmd,
// wlan_cfg80211_cfr_params). What's missing is only the userspace trigger.
//
// # Why this isn't the "normal" CFR vendor command
//
// Current upstream QCA/hostapd (src/common/qca-vendor.h) gives CFR its own
// dedicated vendor subcommand, QCA_NL80211_VENDOR_SUBCMD_PEER_CFR_CAPTURE_CFG
// (173 / 0xad), with attributes in qca_wlan_vendor_peer_cfr_capture_attr.
// This router's build (OpenWrt 18.06-based QSDK, kernel 4.4.60, from 2023)
// predates that. Its wlan_cfg80211_vendor_commands[] table (read straight
// out of umac.ko's .rodata, cross-referenced against its relocations) has
// no entry for that subcommand at all - CFR is instead reached through the
// same generic "wifi configuration" mechanism used for a couple dozen other
// unrelated features (spectral scan, ACS, SON, add_sta_node, ...):
//
//	QCA_NL80211_VENDOR_SUBCMD_SET_WIFI_CONFIGURATION = 74 (0x4a)
//	  -> QCA_WLAN_VENDOR_ATTR_CONFIG_GENERIC_COMMAND (attr 17, u32) = 278
//	  -> QCA_WLAN_VENDOR_ATTR_CONFIG_GENERIC_DATA     (attr 19, bytes)
//	  -> QCA_WLAN_VENDOR_ATTR_CONFIG_GENERIC_LENGTH   (attr 20, u32, redundant
//	     with the attribute's own nla_len, but included for safety)
//
// "278" is the internal dispatch value found by disassembling
// wlan_cfg80211_set_wificonfiguration's jump table in umac.ko: it computes
// `index = generic_command - 200` and branches through a 214-entry table;
// index 78 (-> 278) is the one that calls wlan_cfg80211_cfr_params. This
// value is specific to this firmware build - a different QSDK release could
// use a different number, which is why -cmd exists as an override.
//
// GENERIC_DATA's payload is NOT a nested nlattr set (that was the first,
// wrong guess - it reliably produced ieee80211_ucfg_cfr_params's own
// "Invalid CFR command: 65546" log line, where 65546/0x1000a is exactly an
// nlattr header (len=10,type=1) misread as a raw u32). It's a flat,
// fixed-layout 48-byte C struct instead, found by disassembling
// ieee80211_ucfg_cfr_params directly - see the cfrOff* constants below for
// the exact field offsets and the inner-command jump table (45=start
// capture, 46=stop).
//
// # Why a hand-rolled netlink client, not `iw`
//
// `iw dev <if> vendor send <oui> <subcmd> <hex>` reaches the right command
// (confirmed: the driver logs wlan_cfg80211_set_wificonfiguration's own
// debug line back at us) but this router's `iw` build consistently attaches
// an empty NL80211_ATTR_VENDOR_DATA - verified by reading cfg80211.ko's own
// nl80211_vendor_cmd disassembly, which faithfully forwards nla_data()/
// nla_len() of whatever it received: the attribute pointer comes through
// non-NULL but with nla_len()==4 (i.e. a header with zero payload) every
// time, regardless of how the hex/file argument was passed to `iw`. Rather
// than chase that further, this talks netlink directly.
//
// Built with CGO_ENABLED=0 (see build.sh) - no libc dependency, no module
// dependencies (stdlib syscall only), same approach as ../sms-reader.
//
// Usage:
//
//	cfr-trigger -iface wl0 -mac AA:BB:CC:DD:EE:FF [-bw 3] [-periodicity 0] [-method 0] [-cmd 278] [-disable]
//
// Prints the request it sent and the kernel's ack/error reply. A "-17"/
// "-22" style errno on the reply most likely means this firmware's dispatch
// value or attribute layout has drifted from what's documented above - try
// -cmd with nearby values (locate the switch by grepping umac.ko's symbol
// table for wlan_cfg80211_cfr_params and disassembling the jump table
// feeding it, see the package doc above for the exact method).
//
// # Confirmed working end to end
//
// Verified live on the project's own CB0401: an ack with no kernel-side
// error reaches all the way down to target_if_peer_capture_event (the
// handler for the firmware's own WMI event reporting capture outcome per
// peer) - e.g. "CFR capture failed as peer is in powersave: <mac>" when
// the target device's radio was asleep. With an actively awake peer (its
// screen on, network activity happening), /usr/sbin/cfr_test_app -i wifi1
// produces a real, non-empty /tmp/cfr_dump_wifi1_*.bin: each record starts
// with the magic 0xDEADBEAF and embeds the peer's own MAC address further
// in, confirming genuine per-peer CFR data, not an empty/stub buffer.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"syscall"
)

// --- netlink / genetlink wire format -----------------------------------

const (
	netlinkGeneric = 16 // NETLINK_GENERIC

	nlmsgHdrLen = 16 // sizeof(struct nlmsghdr)
	genlHdrLen  = 4  // sizeof(struct genlmsghdr)
	nlaHdrLen   = 4  // sizeof(struct nlattr)

	nlmsgError = 2 // NLMSG_ERROR
	nlmsgDone  = 3 // NLMSG_DONE

	nlmFRequest = 0x1
	nlmFAck     = 0x4

	genlIDCtrl         = 0x10 // GENL_ID_CTRL
	ctrlCmdGetfamily   = 3
	ctrlAttrFamilyID   = 1
	ctrlAttrFamilyName = 2
	ctrlAttrVersion    = 3

	// NL80211_CMD_VENDOR - NOT 107 as in today's upstream nl80211.h.
	// This kernel's own nl80211_ops[] table (read directly out of its
	// cfg80211.ko: the entry whose .doit relocation points at
	// nl80211_vendor_cmd has raw cmd byte 103) predates four commands
	// that were later inserted earlier in the enum, shifting everything
	// from VENDOR onward by +4 in current upstream. Same category of
	// drift as the QCA generic-command attribute IDs - see package doc.
	nl80211CmdVendor        = 103
	nl80211AttrWiphy        = 1
	nl80211AttrIfindex      = 3
	nl80211AttrWdev         = 153
	nl80211AttrVendorID     = 195
	nl80211AttrVendorSubcmd = 196
	nl80211AttrVendorData   = 197

	// QCA vendor OUI, and the generic-command plumbing this firmware
	// routes CFR through - see package doc.
	qcaOUI                    = 0x001374
	qcaSubcmdSetWifiConfig    = 74
	qcaAttrConfigGenericCmd   = 17
	qcaAttrConfigGenericValue = 18
	qcaAttrConfigGenericData  = 19
	qcaAttrConfigGenericLen   = 20
	qcaCfrGenericCommandValue = 278
	qcaWifiParamsCommandValue = 200 // "wifi params" generic command: GENERIC_VALUE = param id, GENERIC_DATA = value(s)

	// ieee80211_ucfg_cfr_params (called from wlan_cfg80211_cfr_params,
	// which GENERIC_DATA's bytes are handed to verbatim) does NOT parse
	// GENERIC_DATA as a nested nlattr set - confirmed by triggering its
	// "Invalid CFR command: 65546" log line, where 65546 (0x1000a) is
	// exactly the first 4 bytes of an nlattr header (len=10,type=1) read
	// as a raw u32. It's a flat, fixed-layout C struct instead, found by
	// disassembling the function directly (see package doc):
	//
	//	offset 0x00 (u32):    inner CFR command - 45 = start capture,
	//	                      46 = stop capture (own jump table, range
	//	                      [45,77], separate from and nested inside
	//	                      the outer generic-command dispatch above)
	//	offset 0x08 (u8):     mode/flags byte (low byte of a loaded u32;
	//	                      exact bit meaning not yet pinned down - 0
	//	                      has worked in testing)
	//	offset 0x0c (u32):    periodicity in ms (0 = one-shot)
	//	offset 0x10 (u32):    capture method / path selector. If the MAC
	//	                      below resolves to a currently-associated
	//	                      peer (via ieee80211_find_wrap_node), this
	//	                      value must be one of 255 or 2 or the
	//	                      handler rejects it outright; if the MAC
	//	                      does *not* resolve to an associated peer,
	//	                      capture instead goes through the
	//	                      probe-request-based path
	//	                      (ucfg_cfr_start_capture_probe_req), which
	//	                      doesn't require an association at all.
	//	offset 0x14 (6 bytes): peer MAC address
	//	total size: 48 bytes (zero-fill the rest)
	cfrStructSize     = 48
	cfrOffCommand     = 0x00
	cfrOffMode        = 0x08
	cfrOffPeriodicity = 0x0c
	cfrOffMethod      = 0x10
	cfrOffMAC         = 0x14

	cfrCmdStartCapture = 45
	cfrCmdStopCapture  = 46
)

// nlaPut appends one netlink attribute (header + value, padded to 4 bytes)
// to buf and returns the result.
func nlaPut(buf []byte, attrType uint16, value []byte) []byte {
	hdr := make([]byte, nlaHdrLen)
	binary.LittleEndian.PutUint16(hdr, uint16(nlaHdrLen+len(value)))
	binary.LittleEndian.PutUint16(hdr[2:], attrType)
	buf = append(buf, hdr...)
	buf = append(buf, value...)
	for len(buf)%4 != 0 {
		buf = append(buf, 0)
	}
	return buf
}

func u32le(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func u64le(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

// --- a tiny netlink socket ------------------------------------------------

var debug bool

type nlSocket struct {
	fd  int
	seq uint32
}

func openNlSocket() (*nlSocket, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, netlinkGeneric)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(fd, sa); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind: %w", err)
	}
	return &nlSocket{fd: fd}, nil
}

func (s *nlSocket) close() { syscall.Close(s.fd) }

// request sends one genetlink request (msgType = family id, or
// genlIDCtrl for family lookups) and returns the payload of the first
// non-error, non-done reply message it gets back (i.e. genlmsghdr onward),
// or an error decoded from an NLMSG_ERROR reply. genlVersion is the
// genlmsghdr version field - for GETFAMILY lookups this can be 0, but for
// a real family (nl80211) it must be the family's *own* registered
// version (from CTRL_ATTR_VERSION in its GETFAMILY reply): sending 0
// there gets silently rejected as EINVAL before the command even reaches
// the driver.
func (s *nlSocket) request(msgType uint16, cmd uint8, genlVersion uint8, attrs []byte) ([]byte, error) {
	s.seq++
	seq := s.seq

	genl := []byte{cmd, genlVersion, 0, 0} // cmd, version, reserved=0,0
	body := append(genl, attrs...)

	msgLen := nlmsgHdrLen + len(body)
	msg := make([]byte, nlmsgHdrLen)
	binary.LittleEndian.PutUint32(msg, uint32(msgLen))
	binary.LittleEndian.PutUint16(msg[4:], msgType)
	binary.LittleEndian.PutUint16(msg[6:], nlmFRequest|nlmFAck)
	binary.LittleEndian.PutUint32(msg[8:], seq)
	binary.LittleEndian.PutUint32(msg[12:], 0) // pid, kernel ignores/fills
	msg = append(msg, body...)

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(s.fd, msg, 0, sa); err != nil {
		return nil, fmt.Errorf("sendto: %w", err)
	}

	// A single request can produce several reply datagrams sharing our
	// seq (e.g. a data reply followed by a trailing ACK) - collect the
	// data payload but keep reading until we see that seq's own
	// terminating ERROR/DONE, and silently discard anything belonging to
	// an earlier, already-completed request that the previous call
	// didn't fully drain.
	var result []byte
	buf := make([]byte, 65536)
	for {
		n, _, err := syscall.Recvfrom(s.fd, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("recvfrom: %w", err)
		}
		off := 0
		for off+nlmsgHdrLen <= n {
			mlen := int(binary.LittleEndian.Uint32(buf[off:]))
			mtype := binary.LittleEndian.Uint16(buf[off+4:])
			mseq := binary.LittleEndian.Uint32(buf[off+8:])
			if mlen < nlmsgHdrLen || off+mlen > n {
				break
			}
			payload := buf[off+nlmsgHdrLen : off+mlen]
			if debug {
				fmt.Fprintf(os.Stderr, "  <- nlmsg type=%d seq=%d (want %d) len=%d payload=% x\n", mtype, mseq, seq, mlen, payload)
			}
			off += (mlen + 3) &^ 3 // advance before any continue/return below
			if mseq != seq {
				continue // leftover from an earlier request - drop it
			}
			switch mtype {
			case nlmsgError:
				errno := int32(binary.LittleEndian.Uint32(payload))
				if errno == 0 {
					return result, nil // this seq's ACK - done
				}
				return nil, syscall.Errno(-errno)
			case nlmsgDone:
				return result, nil
			default:
				result = append([]byte{}, payload...)
			}
		}
	}
}

// resolveFamily looks up a generic-netlink family's numeric id and its own
// registered version by name.
func (s *nlSocket) resolveFamily(name string) (id uint16, version uint8, err error) {
	var attrs []byte
	nameBytes := append([]byte(name), 0)
	attrs = nlaPut(attrs, ctrlAttrFamilyName, nameBytes)

	payload, err := s.request(genlIDCtrl, ctrlCmdGetfamily, 0, attrs)
	if err != nil {
		return 0, 0, err
	}
	// payload = genlmsghdr(4) + attributes
	if len(payload) < genlHdrLen {
		return 0, 0, fmt.Errorf("short GETFAMILY reply")
	}
	attrBuf := payload[genlHdrLen:]
	var haveID bool
	for off := 0; off+nlaHdrLen <= len(attrBuf); {
		alen := int(binary.LittleEndian.Uint16(attrBuf[off:]))
		atype := binary.LittleEndian.Uint16(attrBuf[off+2:]) &^ 0x8000 // clear NLA_F_NESTED if set
		if alen < nlaHdrLen || off+alen > len(attrBuf) {
			break
		}
		val := attrBuf[off+nlaHdrLen : off+alen]
		switch {
		case atype == ctrlAttrFamilyID && len(val) >= 2:
			id = binary.LittleEndian.Uint16(val)
			haveID = true
		case atype == ctrlAttrVersion && len(val) >= 4:
			version = uint8(binary.LittleEndian.Uint32(val))
		}
		off += (alen + 3) &^ 3
	}
	if !haveID {
		return 0, 0, fmt.Errorf("nl80211 family id not found in GETFAMILY reply")
	}
	return id, version, nil
}

// --- main ------------------------------------------------------------------

func main() {
	iface := flag.String("iface", "", "target Wi-Fi VAP interface (e.g. wl0) - the one the peer is associated to")
	wdevHex := flag.String("wdev", "", "NL80211_ATTR_WDEV value for -iface, as printed by `iw dev` (e.g. 0x100000002) - cfg80211's vendor-cmd dispatch on this router validates the interface via wdev, not just ifindex")
	wiphy := flag.Int("wiphy", -1, "NL80211_ATTR_WIPHY index for -iface's radio (phy0=0, phy1=1, ...), as printed by `iw dev`")
	macStr := flag.String("mac", "", "peer MAC address to capture CFR for")
	periodicity := flag.Uint("periodicity", 0, "capture periodicity in ms (0 = one-shot)")
	method := flag.Uint("method", 255, "capture method / path selector at struct offset 0x10 (see package doc) - 255 or 2 needed for the probe-request path when -mac isn't a currently-associated peer")
	mode := flag.Uint("mode", 0, "mode/flags byte at struct offset 0x08 (meaning not yet fully pinned down)")
	cfrCmd := flag.Uint("cfr-cmd", cfrCmdStartCapture, "inner CFR command at struct offset 0x00 (45=start, 46=stop)")
	cmd := flag.Uint("cmd", qcaCfrGenericCommandValue, "internal generic-command dispatch value for this firmware (see package doc)")
	disable := flag.Bool("disable", false, "shorthand for -cfr-cmd 46 (stop capture)")
	rawHex := flag.String("raw-vendor-data-hex", "", "diagnostic: override the constructed vendor_data payload with these raw bytes (hex, no 0x/spaces needed), e.g. '' for zero-length")
	param := flag.Uint("param", 0, "radio-parameter mode: send SET_WIFI_CONFIGURATION with GENERIC_COMMAND=<this id> and GENERIC_VALUE=-value to -iface (a radio netdev such as wifi1) instead of a CFR peer command - the same message cfg80211tool builds for its set-params, for ids its table no longer lists (0x1194 = the CFR global periodic timer, see package doc)")
	value := flag.Uint("value", 0, "value for -param")
	flag.BoolVar(&debug, "debug", false, "print every raw netlink reply message received")
	flag.Parse()

	rawSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "raw-vendor-data-hex" {
			rawSet = true
		}
	})

	if *param != 0 {
		setRadioParam(*iface, *wiphy, uint32(*param), uint32(*value))
		return
	}
	if *iface == "" || *macStr == "" {
		fmt.Fprintln(os.Stderr, "usage: cfr-trigger -iface wl0 -mac AA:BB:CC:DD:EE:FF [-bw N] [-periodicity N] [-method N] [-cmd N] [-disable]")
		fmt.Fprintln(os.Stderr, "       cfr-trigger -iface wifi1 -param 0x1194 -value 10   (radio parameter, e.g. CFR global timer)")
		os.Exit(2)
	}

	mac, err := net.ParseMAC(*macStr)
	if err != nil || len(mac) != 6 {
		fmt.Fprintf(os.Stderr, "bad -mac %q: %v\n", *macStr, err)
		os.Exit(2)
	}

	ifi, err := net.InterfaceByName(*iface)
	if err != nil {
		fmt.Fprintf(os.Stderr, "interface %q: %v\n", *iface, err)
		os.Exit(1)
	}

	innerCmd := *cfrCmd
	if *disable {
		innerCmd = cfrCmdStopCapture
	}

	// Flat ieee80211_ucfg_cfr_params struct (see package doc for offsets).
	cfr := make([]byte, cfrStructSize)
	binary.LittleEndian.PutUint32(cfr[cfrOffCommand:], uint32(innerCmd))
	cfr[cfrOffMode] = byte(*mode)
	binary.LittleEndian.PutUint32(cfr[cfrOffPeriodicity:], uint32(*periodicity))
	binary.LittleEndian.PutUint32(cfr[cfrOffMethod:], uint32(*method))
	copy(cfr[cfrOffMAC:cfrOffMAC+6], mac)

	// Outer "generic wifi configuration" wrapper.
	var vendorData []byte
	vendorData = nlaPut(vendorData, qcaAttrConfigGenericCmd, u32le(uint32(*cmd)))
	vendorData = nlaPut(vendorData, qcaAttrConfigGenericData, cfr)
	vendorData = nlaPut(vendorData, qcaAttrConfigGenericLen, u32le(uint32(len(cfr))))
	if rawSet {
		b, err := hex.DecodeString(*rawHex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad -raw-vendor-data-hex: %v\n", err)
			os.Exit(2)
		}
		vendorData = b
		fmt.Printf("(overriding vendor_data with %d raw bytes from -raw-vendor-data-hex)\n", len(b))
	}

	var attrs []byte
	if *wiphy >= 0 {
		attrs = nlaPut(attrs, nl80211AttrWiphy, u32le(uint32(*wiphy)))
	}
	attrs = nlaPut(attrs, nl80211AttrIfindex, u32le(uint32(ifi.Index)))
	if *wdevHex != "" {
		var wdev uint64
		if _, err := fmt.Sscanf(*wdevHex, "0x%x", &wdev); err != nil {
			fmt.Fprintf(os.Stderr, "bad -wdev %q: %v\n", *wdevHex, err)
			os.Exit(2)
		}
		attrs = nlaPut(attrs, nl80211AttrWdev, u64le(wdev))
	}
	attrs = nlaPut(attrs, nl80211AttrVendorID, u32le(qcaOUI))
	attrs = nlaPut(attrs, nl80211AttrVendorSubcmd, u32le(qcaSubcmdSetWifiConfig))
	attrs = nlaPut(attrs, nl80211AttrVendorData, vendorData)

	fmt.Printf("iface=%s (ifindex %d) peer=%s cfr-cmd=%d mode=%d periodicity=%dms method=%d generic-cmd=%d\n",
		*iface, ifi.Index, mac, innerCmd, *mode, *periodicity, *method, *cmd)
	fmt.Printf("inner CFR struct (%d bytes): % x\n", len(cfr), cfr)
	fmt.Printf("vendor_data (%d bytes):       % x\n", len(vendorData), vendorData)

	sock, err := openNlSocket()
	if err != nil {
		fmt.Fprintf(os.Stderr, "netlink: %v\n", err)
		os.Exit(1)
	}
	defer sock.close()

	family, familyVersion, err := sock.resolveFamily("nl80211")
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve nl80211 family: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("nl80211 family id=%d version=%d\n", family, familyVersion)

	_, err = sock.request(family, nl80211CmdVendor, familyVersion, attrs)
	if err != nil {
		fmt.Printf("driver reply: error: %v\n", err)
		fmt.Println("(check dmesg on the router for a matching wlan_cfg80211_set_wificonfiguration/extract_generic_command_params line)")
		os.Exit(1)
	}
	fmt.Println("driver reply: ack (no error)")
}

// setRadioParam is the radio-parameter ("cfg80211tool wifiX <name> <value>")
// shape of the same vendor command. cfg80211tool.1's own table (decoded
// from the binary: 16-byte entries {name, param id, subcmd, is_set}) shows
// each parameter as one id used for both the GET (subcmd 0x4b) and SET
// (subcmd 0x4a) forms, carried in GENERIC_COMMAND with the value in
// GENERIC_VALUE. This firmware's cfg80211tool lists only get_cfr_timer
// (id 0x1194) and get_cfr_capture_status (0x11ba) - the setter entry was
// dropped from the tool, not from the driver (ucfg_cfr_set_timer is still
// exported by umac.ko), so the SET form is built here by hand.
func setRadioParam(iface string, wiphy int, param, value uint32) {
	if iface == "" {
		fmt.Fprintln(os.Stderr, "-param needs -iface <radio netdev, e.g. wifi1>")
		os.Exit(2)
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		fmt.Fprintf(os.Stderr, "interface %q: %v\n", iface, err)
		os.Exit(1)
	}
	// Radio parameters are not their own generic command: sending the
	// parameter id as GENERIC_COMMAND gets "Unsupported Generic command:
	// 4500" back (tried). They travel inside the "wifi params" generic
	// command (200, the first entry of the dispatch table cfr-trigger's
	// package doc describes), with the parameter id in GENERIC_VALUE and
	// the value(s) as a u32 array in GENERIC_DATA - the QSDK cfg80211tool
	// convention.
	var vendorData []byte
	vendorData = nlaPut(vendorData, qcaAttrConfigGenericCmd, u32le(qcaWifiParamsCommandValue))
	vendorData = nlaPut(vendorData, qcaAttrConfigGenericValue, u32le(param))
	vendorData = nlaPut(vendorData, qcaAttrConfigGenericData, u32le(value))
	vendorData = nlaPut(vendorData, qcaAttrConfigGenericLen, u32le(4))

	var attrs []byte
	if wiphy >= 0 {
		attrs = nlaPut(attrs, nl80211AttrWiphy, u32le(uint32(wiphy)))
	}
	attrs = nlaPut(attrs, nl80211AttrIfindex, u32le(uint32(ifi.Index)))
	attrs = nlaPut(attrs, nl80211AttrVendorID, u32le(qcaOUI))
	attrs = nlaPut(attrs, nl80211AttrVendorSubcmd, u32le(qcaSubcmdSetWifiConfig))
	attrs = nlaPut(attrs, nl80211AttrVendorData, vendorData)
	fmt.Printf("iface=%s (ifindex %d) set radio param 0x%x = %d\n", iface, ifi.Index, param, value)

	sock, err := openNlSocket()
	if err != nil {
		fmt.Fprintf(os.Stderr, "netlink: %v\n", err)
		os.Exit(1)
	}
	defer sock.close()
	family, familyVersion, err := sock.resolveFamily("nl80211")
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve nl80211 family: %v\n", err)
		os.Exit(1)
	}
	if _, err := sock.request(family, nl80211CmdVendor, familyVersion, attrs); err != nil {
		fmt.Printf("driver reply: error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("driver reply: ack (no error)")
}
