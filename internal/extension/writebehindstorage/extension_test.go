package writebehindstorage

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/extensioncapabilities"
	"go.opentelemetry.io/collector/extension/extensiontest"
	"go.opentelemetry.io/collector/extension/xextension/storage"
)

// hostWith is a component.Host whose GetExtensions returns exts.
type hostWith map[component.ID]component.Component

func (h hostWith) GetExtensions() map[component.ID]component.Component { return h }

// fakeStorageExt is a storage.Extension that records the GetClient arguments.
type fakeStorageExt struct {
	component.StartFunc
	component.ShutdownFunc
	kind   component.Kind
	id     component.ID
	name   string
	client storage.Client
}

func (f *fakeStorageExt) GetClient(_ context.Context, kind component.Kind, id component.ID, name string) (storage.Client, error) {
	f.kind, f.id, f.name = kind, id, name
	if f.client == nil {
		f.client = newFakeClient()
	}
	return f.client, nil
}

func newExt(t *testing.T, cfg *Config) *writeBehind {
	t.Helper()
	ext, err := NewFactory().Create(context.Background(), extensiontest.NewNopSettings(componentType), cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return ext.(*writeBehind)
}

func TestConfigValidate(t *testing.T) {
	def := NewFactory().CreateDefaultConfig().(*Config)
	if err := def.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if def.Storage != component.MustNewID("file_storage") || def.FlushAfter.Seconds() != 15 || def.MaxBufferBytes != 4194304 {
		t.Fatalf("defaults = %+v, want file_storage / 15s / 4194304", def)
	}

	for name, mutate := range map[string]func(*Config){
		"zero storage":       func(c *Config) { c.Storage = component.ID{} },
		"zero flush_after":   func(c *Config) { c.FlushAfter = 0 },
		"zero max_buffer":    func(c *Config) { c.MaxBufferBytes = 0 },
		"negative max_buf":   func(c *Config) { c.MaxBufferBytes = -1 },
		"negative flush_aft": func(c *Config) { c.FlushAfter = -1 },
	} {
		c := *def
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
}

func TestStartFailsWithoutWrappedStorage(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)

	err := newExt(t, cfg).Start(context.Background(), hostWith{})
	if err == nil || !strings.Contains(err.Error(), `write_behind_storage: storage extension "file_storage" not found`) {
		t.Fatalf("Start with empty host: err = %v", err)
	}

	plain := hostWith{component.MustNewID("file_storage"): struct {
		component.StartFunc
		component.ShutdownFunc
	}{}}
	err = newExt(t, cfg).Start(context.Background(), plain)
	if err == nil || !strings.Contains(err.Error(), "is not a storage extension") {
		t.Fatalf("Start with non-storage extension: err = %v", err)
	}
}

func TestGetClientForwardsArgumentsUnchanged(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	fs := &fakeStorageExt{}
	ext := newExt(t, cfg)
	if err := ext.Start(context.Background(), hostWith{component.MustNewID("file_storage"): fs}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ext.Shutdown(context.Background()) })

	id := component.MustNewID("otlp_http")
	c, err := ext.GetClient(context.Background(), component.KindExporter, id, "metrics")
	if err != nil || c == nil {
		t.Fatalf("GetClient: %v, %v", c, err)
	}
	if fs.kind != component.KindExporter || fs.id != id || fs.name != "metrics" {
		t.Fatalf("wrapped GetClient got (%v, %v, %q), want (exporter, otlp_http, \"metrics\")", fs.kind, fs.id, fs.name)
	}
}

func TestGetClientBeforeStartFails(t *testing.T) {
	ext := newExt(t, NewFactory().CreateDefaultConfig().(*Config))
	if _, err := ext.GetClient(context.Background(), component.KindExporter, component.MustNewID("otlp_http"), "metrics"); err == nil {
		t.Fatal("GetClient before Start = nil error, want error")
	}
}

// The collector orders extension start/stop by declared dependencies, not by
// service.extensions (v0.162.0 computeOrder topo-sorts a map). Declaring the
// wrapped storage makes it start before and stop after this extension.
func TestDependsOnWrappedStorage(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Storage = component.MustNewIDWithName("file_storage", "queue")
	var ext any = newExt(t, cfg)
	dep, ok := ext.(extensioncapabilities.Dependent)
	if !ok {
		t.Fatal("write_behind_storage does not implement extensioncapabilities.Dependent")
	}
	if got := dep.Dependencies(); len(got) != 1 || got[0] != cfg.Storage {
		t.Fatalf("Dependencies() = %v, want [%v]", got, cfg.Storage)
	}
}
