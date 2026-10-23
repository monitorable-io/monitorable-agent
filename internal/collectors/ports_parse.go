package collectors

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// TCP socket states as printed in the st column of /proc/net/tcp{,6}. Only these
// two are ever counted; TIME_WAIT/CLOSE_WAIT/etc. are deliberately ignored.
const (
	tcpStateEstablished uint8 = 0x01
	tcpStateListen      uint8 = 0x0A
)

// tcpRow is one socket-table line reduced to what the Ports panel needs. Remote
// addresses, inodes and UIDs are never kept: they identify peers and processes.
type tcpRow struct {
	Local     netip.Addr
	LocalPort uint16
	State     uint8
}

// listenerFacts summarizes every LISTEN row sharing one local port.
type listenerFacts struct {
	Port     uint16
	Loopback bool // true only when EVERY LISTEN row for the port binds a loopback address
	V4, V6   bool // which file (tcp / tcp6) carries LISTEN rows for the port
}

// parseProcNetTCP parses /proc/net/tcp (v6=false) or /proc/net/tcp6 (v6=true).
// The header (first field "sl") is skipped and a malformed line is dropped, so a
// torn read yields the rows that did parse. Two errors are returned: a reader
// error, and a table with data lines of which none parse (an unknown format must
// not read as "no listeners"). A header-only or empty table is not an error.
func parseProcNetTCP(r io.Reader, v6 bool) ([]tcpRow, error) {
	var rows []tcpRow
	dataLines := 0
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// sl local_address rem_address st ...
		f := strings.Fields(sc.Text())
		if len(f) == 0 || f[0] == "sl" {
			continue
		}
		dataLines++
		if len(f) < 4 {
			continue
		}
		addr, port, ok := parseLocalAddr(f[1], v6)
		if !ok {
			continue
		}
		st, err := strconv.ParseUint(f[3], 16, 8)
		if err != nil {
			continue
		}
		rows = append(rows, tcpRow{Local: addr, LocalPort: port, State: uint8(st)})
	}
	if err := sc.Err(); err != nil {
		return rows, err
	}
	if dataLines > 0 && len(rows) == 0 {
		return nil, fmt.Errorf("no parseable rows in %d data lines", dataLines)
	}
	return rows, nil
}

// parseLocalAddr decodes "HEXADDR:HEXPORT". The kernel prints the address as
// native-endian 32-bit words: one for IPv4, four for IPv6. The IPv4-mapped form
// (::ffff:a.b.c.d) is kept as is; isLoopback needs it.
func parseLocalAddr(s string, v6 bool) (netip.Addr, uint16, bool) {
	hexAddr, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return netip.Addr{}, 0, false
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	want := 8
	if v6 {
		want = 32
	}
	if len(hexAddr) != want {
		return netip.Addr{}, 0, false
	}
	raw, err := hex.DecodeString(hexAddr)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	// Each 4-byte word was printed as a little-endian integer; flip every word.
	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(raw[i:], binary.LittleEndian.Uint32(raw[i:]))
	}
	if v6 {
		return netip.AddrFrom16([16]byte(raw)), uint16(port), true
	}
	return netip.AddrFrom4([4]byte(raw)), uint16(port), true
}

// isLoopback reports 127.0.0.0/8, ::1 and the IPv4-mapped ::ffff:127.0.0.0/8.
func isLoopback(a netip.Addr) bool {
	return a.Unmap().IsLoopback()
}

// summarizeSockets merges both tables by local port. established counts every
// port's ESTABLISHED rows (any local address, either file); the caller
// intersects it with listeners. A [::] wildcard socket also accepts IPv4
// clients, which appear in tcp6 as ::ffff:a.b.c.d, so counting by port alone
// is what makes those peers visible.
func summarizeSockets(v4, v6 []tcpRow) (listeners map[uint16]*listenerFacts, established map[uint16]int) {
	listeners = make(map[uint16]*listenerFacts)
	established = make(map[uint16]int)
	add := func(rows []tcpRow, isV6 bool) {
		for _, r := range rows {
			switch r.State {
			case tcpStateListen:
				f := listeners[r.LocalPort]
				if f == nil {
					f = &listenerFacts{Port: r.LocalPort, Loopback: true}
					listeners[r.LocalPort] = f
				}
				f.Loopback = f.Loopback && isLoopback(r.Local)
				if isV6 {
					f.V6 = true
				} else {
					f.V4 = true
				}
			case tcpStateEstablished:
				established[r.LocalPort]++
			}
		}
	}
	add(v4, false)
	add(v6, true)
	return listeners, established
}
