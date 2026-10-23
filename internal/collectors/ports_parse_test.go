package collectors

import (
	"net/netip"
	"os"
	"strings"
	"testing"
)

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestParseProcNetTCP_V4(t *testing.T) {
	rows, err := parseProcNetTCP(openFixture(t, "proc_net_tcp_v4.txt"), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []tcpRow{
		{netip.MustParseAddr("0.0.0.0"), 22, tcpStateListen},
		{netip.MustParseAddr("127.0.0.1"), 5432, tcpStateListen},
		{netip.MustParseAddr("10.0.0.15"), 22, tcpStateEstablished},
		{netip.MustParseAddr("10.0.0.15"), 22, tcpStateEstablished},
		{netip.MustParseAddr("10.0.0.15"), 22, 0x06},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestParseProcNetTCP_V6(t *testing.T) {
	rows, err := parseProcNetTCP(openFixture(t, "proc_net_tcp_v6.txt"), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []tcpRow{
		{netip.IPv6Unspecified(), 443, tcpStateListen},
		{netip.MustParseAddr("::ffff:10.0.0.5"), 443, tcpStateEstablished},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestParseProcNetTCP_SkipsMalformedLines(t *testing.T) {
	in := "  sl  local_address rem_address   st\n" +
		"   0: 00000000:0016 00000000:0000 0A 00000000:00000000\n" +
		"   1: 0100007F:15\n" + // truncated
		"   2: ZZZZZZZZ:0016 00000000:0000 0A\n" + // bad hex address
		"   3: 0100007F:1538 00000000:0000 0A 00000000:00000000\n"
	rows, err := parseProcNetTCP(strings.NewReader(in), false)
	if err != nil {
		t.Fatalf("malformed lines must not error: %v", err)
	}
	if len(rows) != 2 || rows[0].LocalPort != 22 || rows[1].LocalPort != 5432 {
		t.Fatalf("rows = %+v, want ports 22 and 5432", rows)
	}
}

func row(addr string, port uint16, state uint8) tcpRow {
	return tcpRow{netip.MustParseAddr(addr), port, state}
}

func TestSummarizeSockets_ScopeAndFamilies(t *testing.T) {
	v4 := []tcpRow{
		row("0.0.0.0", 22, tcpStateListen),
		row("127.0.0.1", 5432, tcpStateListen),
		row("127.0.0.1", 6379, tcpStateListen),
		row("10.0.0.5", 6379, tcpStateListen),
	}
	v6 := []tcpRow{row("::", 22, tcpStateListen)}
	l, _ := summarizeSockets(v4, v6)

	if f := l[22]; f == nil || f.Loopback || !f.V4 || !f.V6 {
		t.Errorf("22 = %+v, want non-loopback V4+V6", f)
	}
	if f := l[5432]; f == nil || !f.Loopback || !f.V4 || f.V6 {
		t.Errorf("5432 = %+v, want loopback V4 only", f)
	}
	if f := l[6379]; f == nil || f.Loopback || !f.V4 || f.V6 {
		t.Errorf("6379 = %+v, want non-loopback (mixed binds) V4 only", f)
	}
	if len(l) != 3 {
		t.Errorf("got %d listeners, want 3", len(l))
	}
}

func TestSummarizeSockets_CountsEstablishedOnly(t *testing.T) {
	v4 := []tcpRow{
		row("0.0.0.0", 22, tcpStateListen),
		row("10.0.0.5", 22, tcpStateEstablished),
		row("10.0.0.5", 22, tcpStateEstablished),
		row("10.0.0.5", 22, 0x06), // TIME_WAIT
		row("10.0.0.5", 22, 0x06),
		row("10.0.0.5", 22, 0x06),
		row("10.0.0.5", 22, 0x08), // CLOSE_WAIT
		row("10.0.0.5", 9000, tcpStateEstablished),
	}
	l, est := summarizeSockets(v4, nil)
	if est[22] != 2 {
		t.Errorf("established[22] = %d, want 2", est[22])
	}
	if est[9000] != 1 {
		t.Errorf("established[9000] = %d, want 1 (caller filters to listeners)", est[9000])
	}
	if _, ok := l[9000]; ok {
		t.Error("a port with no LISTEN row must not be a listener")
	}
}

func TestSummarizeSockets_V6OnlyWildcardCountsMappedPeers(t *testing.T) {
	v6 := []tcpRow{
		row("::", 443, tcpStateListen),
		row("::ffff:10.0.0.5", 443, tcpStateEstablished),
	}
	l, est := summarizeSockets(nil, v6)
	if f := l[443]; f == nil || f.V4 || !f.V6 || f.Loopback {
		t.Errorf("443 = %+v, want V6 only, non-loopback", f)
	}
	if est[443] != 1 {
		t.Errorf("established[443] = %d, want 1", est[443])
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1":        true,
		"127.5.5.5":        true,
		"::1":              true,
		"::ffff:127.0.0.1": true,
		"10.0.0.1":         false,
		"::":               false,
		"0.0.0.0":          false,
		"fe80::1":          false,
	} {
		if got := isLoopback(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isLoopback(%s) = %v, want %v", addr, got, want)
		}
	}
}
