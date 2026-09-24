// sms-reader is a tiny, purpose-built reader for exactly one table in the
// stock firmware's own SMS database (/data/etc/mobile/xqSMS.db, maintained
// by /usr/sbin/mobile - the router's proprietary mobile-management daemon,
// which itself is compiled/encrypted Lua we can't extend or hook into
// directly). There's no sqlite3 CLI on this router and adding one would
// mean either a multi-MB dependency or trusting a foreign binary's libc
// ABI against this firmware's musl/uClibc userland - both a poor fit for a
// ~20MB persistent partition (see README's "No Docker/Entware/Node.js").
//
// This implements only the minimal slice of the SQLite file format needed
// to read rows of a small, known, single-page table:
//   - the file header (page size)
//   - sqlite_master on page 1, to find SMS_MESSAGE's root page
//   - a single table b-tree LEAF page (no interior pages, no overflow -
//     both would mean the table grew far beyond what a small local SMS
//     inbox realistically holds; this fails closed and says so rather than
//     misreading data, see readTableLeaf's checks)
//   - each row's record: MSG_ID (INTEGER PRIMARY KEY, so it's the cell's
//     rowid, not stored in the record body), STATE, TIMESTAMP,
//     CONTACT_PHONE, CONTENT
//
// Built with CGO_ENABLED=0 so it's a static, syscall-only binary - it
// doesn't link against the router's libc at all, so it isn't sensitive to
// which libc this firmware actually uses.
//
// Usage: sms-reader <path-to-xqSMS.db> [after-msg-id]
//
// Without a second argument: prints only the single newest row. With one,
// prints every row whose MSG_ID is greater than it, oldest first, one per
// line - so a caller polling this on an interval can forward each message
// that arrived since its last check, not just the latest (which would
// silently drop any earlier one from the same interval).
//
// Output, tab-separated, one line per row:
//
//	<msg_id>\t<state>\t<timestamp>\t<contact_phone>\t<content, with \t \n \r \\ escaped>
//
// Prints nothing and exits 0 if there's nothing to report. Exits non-zero
// with a message on stderr if the file doesn't parse as expected (missing
// table, multi-page tree, overflow payload, etc.) - all cases where
// guessing would risk showing a corrupted or wrong message instead of
// just skipping.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type record struct {
	rowid  int64
	values []recordValue
}

type recordValue struct {
	serial int64
	data   []byte // raw bytes for TEXT/BLOB; nil otherwise
	isInt  bool
	intVal int64
}

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: sms-reader <path-to-sqlite-db> [after-msg-id]")
		os.Exit(2)
	}
	afterID := int64(-1) // -1: no filter, means "just the newest one"
	if len(os.Args) == 3 {
		v, err := strconv.ParseInt(os.Args[2], 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sms-reader: after-msg-id must be an integer:", err)
			os.Exit(2)
		}
		afterID = v
	}
	if err := run(os.Args[1], afterID); err != nil {
		fmt.Fprintln(os.Stderr, "sms-reader:", err)
		os.Exit(1)
	}
}

func run(path string, afterID int64) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if len(buf) < 100 || string(buf[0:16]) != "SQLite format 3\x00" {
		return fmt.Errorf("%s is not a SQLite database", path)
	}
	pageSize := int(binary.BigEndian.Uint16(buf[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || len(buf)%pageSize != 0 {
		return fmt.Errorf("unexpected page size %d for a %d-byte file", pageSize, len(buf))
	}

	rootPage, err := findTableRootPage(buf, pageSize, "SMS_MESSAGE")
	if err != nil {
		return err
	}

	rows, err := readTableLeaf(buf, pageSize, rootPage)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil // empty table: nothing to print, not an error
	}

	var toPrint []record
	if afterID < 0 {
		best := rows[0]
		for _, r := range rows[1:] {
			if r.rowid > best.rowid {
				best = r
			}
		}
		toPrint = []record{best}
	} else {
		for _, r := range rows {
			if r.rowid > afterID {
				toPrint = append(toPrint, r)
			}
		}
		sort.Slice(toPrint, func(i, j int) bool { return toPrint[i].rowid < toPrint[j].rowid })
	}

	for _, r := range toPrint {
		// SMS_MESSAGE(MSG_ID INTEGER PRIMARY KEY, STATE INTEGER, TIMESTAMP
		// INTEGER, CONTACT_PHONE TEXT, CONTENT TEXT) - MSG_ID is the rowid
		// itself (never stored in the record body for an INTEGER PRIMARY
		// KEY column). SQLite still emits a header entry for it (serial
		// type 0 = NULL, zero bytes of body) even though its real value is
		// the cell's rowid, not anything stored here - so this is 5 header
		// entries for 5 declared columns, and values[0] is that
		// placeholder we skip.
		if len(r.values) != 5 {
			return fmt.Errorf("SMS_MESSAGE row %d has %d columns, expected 5 (schema changed?)", r.rowid, len(r.values))
		}
		state := r.values[1].intVal
		timestamp := r.values[2].intVal
		phone := escapeField(string(r.values[3].data))
		content := escapeField(string(r.values[4].data))
		fmt.Printf("%d\t%d\t%d\t%s\t%s\n", r.rowid, state, timestamp, phone, content)
	}
	return nil
}

func escapeField(s string) string {
	// The stock firmware sometimes stores a stray non-UTF-8 byte (seen live:
	// an alphanumeric sender "HotSpot" saved as "HotSpot\xa1"). Telegram
	// rejects any request containing invalid UTF-8 outright ("strings must
	// be encoded in UTF-8"), so one such message would never be deliverable.
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\t", "\\t")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}

// readVarint reads a SQLite variable-length integer (up to 9 bytes) and
// returns its value plus how many bytes it consumed.
func readVarint(b []byte) (int64, int) {
	var v int64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return v, i
		}
		c := b[i]
		v = (v << 7) | int64(c&0x7f)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	// 9th byte contributes all 8 bits.
	if len(b) >= 9 {
		v = (v << 8) | int64(b[8])
		return v, 9
	}
	return v, len(b)
}

// findTableRootPage scans sqlite_master (always page 1, a table leaf page
// itself for any database this small) for a table with the given name and
// returns its root page number.
func findTableRootPage(buf []byte, pageSize int, table string) (int, error) {
	// Page 1 is special: it's still one full `pageSize`-byte page (offsets
	// inside it - the cell pointer array, cell content - are all relative
	// to the page's own start, i.e. absolute file offset 0), but the
	// b-tree page header itself starts 100 bytes in, after the file
	// header. Every other page has no such offset.
	rows, err := parseLeafPage(buf[0:pageSize], 100)
	if err != nil {
		return 0, fmt.Errorf("parsing sqlite_master: %w", err)
	}
	for _, r := range rows {
		// sqlite_master(type, name, tbl_name, rootpage, sql)
		if len(r.values) != 5 {
			continue
		}
		typ := string(r.values[0].data)
		tblName := string(r.values[2].data)
		if typ == "table" && strings.EqualFold(tblName, table) {
			return int(r.values[3].intVal), nil
		}
	}
	return 0, fmt.Errorf("table %s not found in sqlite_master", table)
}

// readTableLeaf reads page number `page` (1-indexed, as SQLite numbers
// them) and returns its rows. Only page type 0x0d (table b-tree leaf) is
// supported - an interior page (0x05) would mean the table spans multiple
// pages, which a small local SMS inbox isn't expected to do; we say so
// explicitly rather than only reading part of the table.
func readTableLeaf(buf []byte, pageSize, page int) ([]record, error) {
	if page < 1 || page*pageSize > len(buf) {
		return nil, fmt.Errorf("page %d out of range", page)
	}
	start := (page - 1) * pageSize
	data := buf[start : start+pageSize]
	return parseLeafPage(data, 0)
}

// parseLeafPage reads a table b-tree leaf page. `page` is the FULL page
// (pageSize bytes), and `headerStart` is where the 8-byte b-tree page
// header begins within it - 0 for every page except page 1, where it's
// 100 (right after the file header). Cell pointers and cell content are
// always relative to the start of `page` itself (offset 0), regardless of
// where the b-tree header starts - that's what makes page 1 special.
func parseLeafPage(page []byte, headerStart int) ([]record, error) {
	if headerStart+8 > len(page) {
		return nil, fmt.Errorf("page too short")
	}
	hdr := page[headerStart:]
	pageType := hdr[0]
	if pageType == 0x05 {
		return nil, fmt.Errorf("table spans multiple pages (interior page found) - not supported")
	}
	if pageType != 0x0d {
		return nil, fmt.Errorf("unexpected b-tree page type 0x%02x", pageType)
	}
	numCells := int(binary.BigEndian.Uint16(hdr[3:5]))
	cellPtrArray := hdr[8:]

	var rows []record
	for i := 0; i < numCells; i++ {
		off := int(binary.BigEndian.Uint16(cellPtrArray[i*2 : i*2+2]))
		if off < 0 || off >= len(page) {
			return nil, fmt.Errorf("cell %d pointer out of range", i)
		}
		cell := page[off:]
		payloadLen, n1 := readVarint(cell)
		rowid, n2 := readVarint(cell[n1:])
		payloadStart := n1 + n2
		if payloadStart+int(payloadLen) > len(cell) {
			return nil, fmt.Errorf("row %d's payload would need an overflow page - not supported", rowid)
		}
		payload := cell[payloadStart : payloadStart+int(payloadLen)]

		rec, err := parseRecord(payload)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", rowid, err)
		}
		rec.rowid = rowid
		rows = append(rows, rec)
	}
	return rows, nil
}

func parseRecord(payload []byte) (record, error) {
	headerLen, n := readVarint(payload)
	if int(headerLen) > len(payload) {
		return record{}, fmt.Errorf("record header longer than payload")
	}
	header := payload[n:headerLen]
	body := payload[headerLen:]

	var values []recordValue
	pos := 0
	bodyPos := 0
	for pos < len(header) {
		serial, adv := readVarint(header[pos:])
		pos += adv
		v := recordValue{serial: serial}
		switch {
		case serial == 0:
			v.isInt, v.intVal = true, 0
		case serial >= 1 && serial <= 6:
			n := map[int64]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}[serial]
			if bodyPos+n > len(body) {
				return record{}, fmt.Errorf("integer column runs past record body")
			}
			v.isInt, v.intVal = true, decodeBigEndianInt(body[bodyPos:bodyPos+n])
			bodyPos += n
		case serial == 7:
			bodyPos += 8 // IEEE754 float, not needed for this schema
		case serial == 8:
			v.isInt, v.intVal = true, 0
		case serial == 9:
			v.isInt, v.intVal = true, 1
		case serial >= 12 && serial%2 == 0: // BLOB
			n := int((serial - 12) / 2)
			if bodyPos+n > len(body) {
				return record{}, fmt.Errorf("blob column runs past record body")
			}
			v.data = body[bodyPos : bodyPos+n]
			bodyPos += n
		case serial >= 13 && serial%2 == 1: // TEXT
			n := int((serial - 13) / 2)
			if bodyPos+n > len(body) {
				return record{}, fmt.Errorf("text column runs past record body")
			}
			v.data = body[bodyPos : bodyPos+n]
			bodyPos += n
		default:
			return record{}, fmt.Errorf("unsupported serial type %d", serial)
		}
		values = append(values, v)
	}
	return record{values: values}, nil
}

func decodeBigEndianInt(b []byte) int64 {
	var v int64
	for i, c := range b {
		if i == 0 && c&0x80 != 0 {
			v = -1 // sign-extend
		}
		v = (v << 8) | int64(c)
	}
	return v
}
