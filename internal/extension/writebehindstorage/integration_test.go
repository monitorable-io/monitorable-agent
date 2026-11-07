package writebehindstorage

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/storage/filestorage"
	"go.etcd.io/bbolt"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configretry"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/extension/extensiontest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// These tests run the real exporterhelper persistent queue on the real
// file_storage extension through write_behind_storage, wired as the shipped
// distribution config wires them (exporter otlp_http, signal metrics, so the
// bbolt file is exporter_otlp_http__metrics).

var (
	fsID  = component.MustNewID("file_storage")
	wbID  = component.MustNewID("write_behind_storage")
	expID = component.MustNewID("otlp_http")
)

const queueFile = "exporter_otlp_http__metrics"

type stack struct {
	fs   extension.Extension
	wb   *writeBehind
	exp  exporter.Metrics
	host hostWith
}

// startStack starts file_storage then write_behind_storage (the order of
// service.extensions) and an exporter whose persistent queue uses the
// wrapper. push is the exporter's send function.
func startStack(t *testing.T, dir string, flushAfter time.Duration, logger *zap.Logger, push func(context.Context, pmetric.Metrics) error) *stack {
	t.Helper()
	ctx := context.Background()

	fsCfg := filestorage.NewFactory().CreateDefaultConfig().(*filestorage.Config)
	fsCfg.Directory = dir
	fsCfg.Compaction.Directory = dir
	fsCfg.FSync = true
	fsCfg.Timeout = 2 * time.Second
	fs, err := filestorage.NewFactory().Create(ctx, extensiontest.NewNopSettings(fsID.Type()), fsCfg)
	if err != nil {
		t.Fatalf("create file_storage: %v", err)
	}

	wbSet := extensiontest.NewNopSettings(componentType)
	wbSet.Logger = logger
	wbCfg := &Config{Storage: fsID, FlushAfter: flushAfter, MaxBufferBytes: 4 << 20}
	wbExt, err := NewFactory().Create(ctx, wbSet, wbCfg)
	if err != nil {
		t.Fatalf("create write_behind_storage: %v", err)
	}
	wb := wbExt.(*writeBehind)

	host := hostWith{fsID: fs, wbID: wb}
	if err := fs.Start(ctx, host); err != nil {
		t.Fatalf("start file_storage: %v", err)
	}
	if err := wb.Start(ctx, host); err != nil {
		t.Fatalf("start write_behind_storage: %v", err)
	}

	q := exporterhelper.NewDefaultQueueConfig()
	q.NumConsumers = 2
	q.QueueSize = 1440
	q.StorageID = &wbID
	retry := configretry.NewDefaultBackOffConfig()
	retry.InitialInterval = 20 * time.Millisecond
	retry.MaxInterval = 50 * time.Millisecond
	retry.MaxElapsedTime = 0 // retry forever, as shipped
	set := exportertest.NewNopSettings(expID.Type())
	set.ID = expID
	exp, err := exporterhelper.NewMetrics(ctx, set, &struct{}{}, push,
		exporterhelper.WithQueue(configoptional.Some(q)),
		exporterhelper.WithRetry(retry))
	if err != nil {
		t.Fatalf("create exporter: %v", err)
	}
	if err := exp.Start(ctx, host); err != nil {
		t.Fatalf("start exporter: %v", err)
	}
	return &stack{fs: fs, wb: wb, exp: exp, host: host}
}

// stop shuts down in service order: pipelines first, then extensions in
// reverse start order.
func (s *stack) stop(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := s.exp.Shutdown(ctx); err != nil {
		t.Errorf("exporter shutdown: %v", err)
	}
	if err := s.wb.Shutdown(ctx); err != nil {
		t.Errorf("write_behind_storage shutdown: %v", err)
	}
	if err := s.fs.Shutdown(ctx); err != nil {
		t.Errorf("file_storage shutdown: %v", err)
	}
}

// queueClient returns the one write-behind client the exporter's queue holds.
func (s *stack) queueClient(t *testing.T) *client {
	t.Helper()
	s.wb.mu.Lock()
	defer s.wb.mu.Unlock()
	if len(s.wb.clients) != 1 {
		t.Fatalf("write_behind_storage handed out %d clients, want 1", len(s.wb.clients))
	}
	return s.wb.clients[0].(*client)
}

func metricWithValue(v int64) pmetric.Metrics {
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("test.value")
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(v)
	return md
}

func valueOf(md pmetric.Metrics) int64 {
	return md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().At(0).IntValue()
}

// diskKeys opens the queue file read-only (after the stack released it) and
// returns its keys.
func diskKeys(t *testing.T, dir string) []string {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(dir, queueFile), 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open %s: %v", queueFile, err)
	}
	defer db.Close()
	var keys []string
	if err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("default"))
		if b == nil {
			return errors.New("bucket \"default\" missing")
		}
		return b.ForEach(func(k, _ []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	return keys
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIntegrationQueueThroughWriteBehindLeavesNoItemsOnDisk(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	pushed := 0
	s := startStack(t, dir, 15*time.Second, zap.NewNop(), func(context.Context, pmetric.Metrics) error {
		mu.Lock()
		pushed++
		mu.Unlock()
		return nil
	})
	for i := range 20 {
		if err := s.exp.ConsumeMetrics(context.Background(), metricWithValue(int64(i))); err != nil {
			t.Fatalf("ConsumeMetrics #%d: %v", i, err)
		}
	}
	waitFor(t, "20 pushes", 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return pushed == 20
	})

	// Steady state: every batch was offered, sent and deleted inside the
	// window, so nothing has been written to the file yet.
	c := s.queueClient(t)
	c.mu.Lock()
	flushes := c.flushes
	c.mu.Unlock()
	if flushes != 0 {
		t.Fatalf("flushes before shutdown = %d, want 0 (steady state writes nothing)", flushes)
	}

	s.stop(t)
	if keys := diskKeys(t, dir); len(keys) != 1 || keys[0] != metadataKey {
		t.Fatalf("keys on disk after a clean run = %v, want only [%s]", keys, metadataKey)
	}
}

func TestIntegrationQueueRecoversItemsOlderThanWindow(t *testing.T) {
	dir := t.TempDir()
	core, logs := observer.New(zap.DebugLevel)
	failing := func(context.Context, pmetric.Metrics) error { return errors.New("backend down") }
	s := startStack(t, dir, 200*time.Millisecond, zap.New(core), failing)
	for i := range 5 {
		if err := s.exp.ConsumeMetrics(context.Background(), metricWithValue(int64(i))); err != nil {
			t.Fatalf("ConsumeMetrics #%d: %v", i, err)
		}
	}

	// Wait until every item has aged past the window and been flushed (the
	// metadata key alone may stay dirty: consumers rewrite it on every read).
	c := s.queueClient(t)
	waitFor(t, "the 5 items to be flushed", 10*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.flushes == 0 {
			return false
		}
		for k, e := range c.entries {
			if e.dirty && k != metadataKey {
				return false
			}
		}
		return true
	})
	for _, e := range logs.FilterMessage("write_behind_storage flush").All() {
		ctxMap := e.ContextMap()
		t.Logf("flush: reason=%v keys=%v bytes=%v took=%v", ctxMap["reason"], ctxMap["keys"], ctxMap["bytes"], ctxMap["took"])
	}

	// Hard kill: close the bbolt file underneath everything, so nothing more
	// reaches it, then let the old stack wind down against the closed file.
	c.mu.Lock()
	inner := c.wrapped
	c.mu.Unlock()
	if err := inner.Close(context.Background()); err != nil {
		t.Fatalf("closing the bbolt client: %v", err)
	}
	ctx := context.Background()
	_ = s.exp.Shutdown(ctx)
	_ = s.wb.Shutdown(ctx)
	_ = s.fs.Shutdown(ctx)

	// Restart on the same directory with a healthy backend.
	var mu sync.Mutex
	got := map[int64]bool{}
	s2 := startStack(t, dir, 15*time.Second, zap.NewNop(), func(_ context.Context, md pmetric.Metrics) error {
		mu.Lock()
		got[valueOf(md)] = true
		mu.Unlock()
		return nil
	})
	defer s2.stop(t)
	waitFor(t, "the 5 batches to arrive after restart", 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 5
	})
	for i := range int64(5) {
		if !got[i] {
			t.Fatalf("batch %d not recovered; got %v", i, got)
		}
	}
}
