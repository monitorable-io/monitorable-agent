// Package addressprobe announces the agent to the backend over each IP family.
//
// Metrics travel over whichever family the OS prefers (IPv6 on dual-stack
// hosts), so the backend only ever sees one of the server's addresses. Once
// at startup and then hourly, the prober sends an empty authenticated
// POST /v1/probe over IPv4 and over IPv6; the backend records each request's
// source address (spec 2026-09-29 dual-family IPs).
package addressprobe

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	requestTimeout = 10 * time.Second
	maxStartDelay  = time.Minute
)

// Config is the probe's configuration; an empty Endpoint means "don't probe"
// and is handled by the caller.
type Config struct {
	Endpoint string
	APIKey   string
	Interval time.Duration
}

type result int

const (
	resultNone result = iota
	resultOK
	resultFailed
	resultUnauthorized
	resultNotFound
)

type family struct {
	name    string // "IPv4" / "IPv6", for logs
	network string // "tcp4" / "tcp6"
	client  *http.Client
	last    result
}

// Prober sends the per-family probes. Not safe for concurrent use: Run owns it.
type Prober struct {
	url      string
	apiKey   string
	interval time.Duration
	logger   *zap.Logger
	families []*family
	rand     func() float64
}

// New builds a Prober for cfg.
func New(cfg Config, logger *zap.Logger) *Prober {
	return &Prober{
		url:      probeURL(cfg.Endpoint),
		apiKey:   cfg.APIKey,
		interval: cfg.Interval,
		logger:   logger,
		families: []*family{
			{name: "IPv4", network: "tcp4", client: newClient("tcp4")},
			{name: "IPv6", network: "tcp6", client: newClient("tcp6")},
		},
		rand: rand.Float64,
	}
}

// Run probes after a random start delay (0–60 s, so a fleet-wide restart
// doesn't synchronise), then every interval ± 10 %, until ctx is done.
func (p *Prober) Run(ctx context.Context) {
	wait := time.Duration(p.rand() * float64(maxStartDelay))
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		p.round(ctx)
		wait = jitter(p.interval, p.rand())
	}
}

func (p *Prober) round(ctx context.Context) {
	for _, f := range p.families {
		res, detail := p.probe(ctx, f)
		if res == f.last {
			continue
		}
		f.last = res
		fields := []zap.Field{zap.String("family", f.name)}
		if detail != "" {
			fields = append(fields, zap.String("detail", detail))
		}
		switch res {
		case resultOK:
			p.logger.Debug("address probe succeeded", fields...)
		case resultUnauthorized:
			p.logger.Warn("address probe rejected: the API key is not valid", fields...)
		case resultNotFound:
			p.logger.Debug("address probe: backend has no /v1/probe yet", fields...)
		default:
			// A host without IPv6 fails every IPv6 probe; that is normal.
			p.logger.Debug("address probe failed", fields...)
		}
	}
}

func (p *Prober) probe(ctx context.Context, f *family) (result, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, nil)
	if err != nil {
		return resultFailed, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	resp, err := f.client.Do(req)
	if err != nil {
		return resultFailed, err.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resultOK, ""
	case resp.StatusCode == http.StatusUnauthorized:
		return resultUnauthorized, ""
	case resp.StatusCode == http.StatusNotFound:
		return resultNotFound, ""
	default:
		return resultFailed, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
}

// newClient returns a client whose connections use only network ("tcp4" or
// "tcp6"), with the exporter's TLS roots and proxy handling, a 10 s timeout,
// and no redirects (the key must never leave the configured endpoint).
func newClient(network string) *http.Client {
	d := &net.Dialer{Timeout: requestTimeout}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	return &http.Client{
		Transport: tr,
		Timeout:   requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func probeURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/probe"
}

// jitter spreads d over [0.9d, 1.1d]; r is uniform in [0, 1].
func jitter(d time.Duration, r float64) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*r))
}
