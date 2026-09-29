package addressprobe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func listen(t *testing.T, network, addr string, h http.Handler) *httptest.Server {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("%s unavailable here: %v", network, err)
	}
	s := httptest.NewUnstartedServer(h)
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	return s
}

func ok(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }

func TestClientsArePinnedToTheirFamily(t *testing.T) {
	v4 := listen(t, "tcp4", "127.0.0.1:0", http.HandlerFunc(ok))
	v6 := listen(t, "tcp6", "[::1]:0", http.HandlerFunc(ok))
	c4, c6 := newClient("tcp4"), newClient("tcp6")
	post := func(c *http.Client, url string) error {
		resp, err := c.Post(url, "", nil)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := post(c4, v4.URL); err != nil {
		t.Fatalf("tcp4 client → IPv4 server: %v", err)
	}
	if err := post(c6, v6.URL); err != nil {
		t.Fatalf("tcp6 client → IPv6 server: %v", err)
	}
	if post(c4, v6.URL) == nil {
		t.Fatal("tcp4 client reached the IPv6 server")
	}
	if post(c6, v4.URL) == nil {
		t.Fatal("tcp6 client reached the IPv4 server")
	}
}

func TestRoundSendsAuthenticatedProbe(t *testing.T) {
	var got atomic.Value
	srv := listen(t, "tcp4", "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	p := New(Config{Endpoint: srv.URL + "/", APIKey: "k", Interval: time.Hour}, zap.NewNop())
	p.round(context.Background())
	if g, _ := got.Load().(string); g != "POST /v1/probe Bearer k" {
		t.Fatalf("server saw %q", g)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var followed atomic.Bool
	target := listen(t, "tcp4", "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Store(true) }))
	srv := listen(t, "tcp4", "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	New(Config{Endpoint: srv.URL, APIKey: "k", Interval: time.Hour}, zap.NewNop()).round(context.Background())
	if followed.Load() {
		t.Fatal("redirect was followed: the key could leave the configured endpoint")
	}
}

func TestLogsOnStateChangeOnly(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	srv := listen(t, "tcp4", "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	core, logs := observer.New(zap.DebugLevel)
	p := New(Config{Endpoint: srv.URL, APIKey: "secret-key", Interval: time.Hour}, zap.New(core))

	p.round(context.Background()) // IPv4 ok (first result), IPv6 fails (first result): 2 lines
	p.round(context.Background()) // nothing changed: 0 lines
	status.Store(http.StatusUnauthorized)
	p.round(context.Background()) // IPv4 → unauthorized: 1 warning
	p.round(context.Background()) // unchanged: 0 lines

	if n := logs.Len(); n != 3 {
		t.Fatalf("got %d log lines, want 3: %v", n, logs.All())
	}
	if w := logs.FilterLevelExact(zap.WarnLevel).Len(); w != 1 {
		t.Fatalf("got %d warnings, want 1", w)
	}
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "secret-key") {
			t.Fatalf("API key in log message: %q", e.Message)
		}
		for k, v := range e.ContextMap() {
			if strings.Contains(fmt.Sprint(v), "secret-key") {
				t.Fatalf("API key in log field %q: %v", k, v)
			}
		}
	}
}

func TestProbeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://ingest.monitorable.net":  "https://ingest.monitorable.net/v1/probe",
		"https://ingest.monitorable.net/": "https://ingest.monitorable.net/v1/probe",
	} {
		if got := probeURL(in); got != want {
			t.Errorf("probeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJitterStaysWithinTenPercent(t *testing.T) {
	if lo, hi := jitter(time.Hour, 0), jitter(time.Hour, 1); lo != 54*time.Minute || hi != 66*time.Minute {
		t.Fatalf("jitter range = [%v, %v], want [54m, 66m]", lo, hi)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New(Config{Endpoint: "http://127.0.0.1:1", APIKey: "k", Interval: time.Hour}, zap.NewNop()).Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
