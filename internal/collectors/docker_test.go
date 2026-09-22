package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	ctypes "github.com/moby/moby/api/types/container"
	client "github.com/moby/moby/client"
)

// discardLogger keeps test output pristine — Debug/Warn logs are exercised by
// dedicated tests below, not by every other test in this file.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeAPI is a programmable dockerAPI for tests.
type fakeAPI struct {
	listErr    error
	items      []ctypes.Summary
	statsBody  string
	statsErr   error
	inspect    map[string]ctypes.InspectResponse
	inspectErr error
	closed     int
}

func (f *fakeAPI) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	return client.ContainerListResult{Items: f.items}, nil
}

func (f *fakeAPI) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if f.inspectErr != nil {
		return client.ContainerInspectResult{}, f.inspectErr
	}
	return client.ContainerInspectResult{Container: f.inspect[id]}, nil
}

func (f *fakeAPI) ContainerStats(_ context.Context, _ string, _ client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	if f.statsErr != nil {
		return client.ContainerStatsResult{}, f.statsErr
	}
	return client.ContainerStatsResult{Body: io.NopCloser(strings.NewReader(f.statsBody))}, nil
}

func (f *fakeAPI) Close() error { f.closed++; return nil }

func newCollectorWithAPI(endpoint string, api dockerAPI) (*DockerCollector, *int) {
	calls := 0
	d := NewDockerCollector(endpoint, nil, discardLogger())
	d.newAPI = func(string) (dockerAPI, error) {
		calls++
		return api, nil
	}
	return d, &calls
}

const sampleStats = `{"cpu_stats":{"cpu_usage":{"total_usage":2000000000},"system_cpu_usage":10000000000,"online_cpus":2},"memory_stats":{"usage":100,"limit":200},"networks":{"eth0":{"rx_bytes":1000,"tx_bytes":500},"eth1":{"rx_bytes":24,"tx_bytes":12}}}`
const sampleStats2 = `{"cpu_stats":{"cpu_usage":{"total_usage":3000000000},"system_cpu_usage":20000000000,"online_cpus":2},"memory_stats":{"usage":100,"limit":200},"networks":{"eth0":{"rx_bytes":2024,"tx_bytes":1012}}}`

func runningSummary(id, name, image string) ctypes.Summary {
	return ctypes.Summary{ID: id, Names: []string{"/" + name}, Image: image, State: ctypes.StateRunning}
}

func TestCollectPresent(t *testing.T) {
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img:1")},
		statsBody: sampleStats,
		inspect: map[string]ctypes.InspectResponse{
			"c1": {RestartCount: 2, State: &ctypes.State{Status: ctypes.StateRunning, StartedAt: "2026-07-07T10:00:00Z"}},
		},
	}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if res.Status != DockerPresent {
		t.Fatalf("Status = %v, want DockerPresent", res.Status)
	}
	if len(res.Containers) != 1 {
		t.Fatalf("Containers = %+v", res.Containers)
	}
	c := res.Containers[0]
	if c.ID != "c1" || c.State != "running" || c.RestartCount != 2 || c.StartedAtUnixMs == 0 {
		t.Errorf("snapshot = %+v", c)
	}
	if c.CPUPercent == nil || c.MemUsage == nil || *c.MemUsage != 100 || c.MemLimit == nil || *c.MemLimit != 200 {
		t.Errorf("stats fields = %+v", c)
	}
	if c.NetRxRate == nil || *c.NetRxRate != 0 { // first sight → 0 rate
		t.Errorf("NetRxRate = %v, want 0 on first sight", c.NetRxRate)
	}
	if c.ExitCode != nil || c.FinishedAtUnixMs != 0 || c.Health != "" {
		t.Errorf("unexpected exit/health fields: %+v", c)
	}
}

func TestCollectHealthFromSummary(t *testing.T) {
	s := runningSummary("c1", "app", "img:1")
	s.Health = &ctypes.HealthSummary{Status: ctypes.Unhealthy}
	api := &fakeAPI{items: []ctypes.Summary{s}, statsBody: sampleStats,
		inspect: map[string]ctypes.InspectResponse{"c1": {State: &ctypes.State{Status: ctypes.StateRunning}}}}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if res.Containers[0].Health != "unhealthy" {
		t.Errorf("Health = %q, want unhealthy", res.Containers[0].Health)
	}
}

func TestCollectSkipsCreatedAndRemoving(t *testing.T) {
	api := &fakeAPI{items: []ctypes.Summary{
		{ID: "c1", Names: []string{"/a"}, State: ctypes.StateCreated},
		{ID: "c2", Names: []string{"/b"}, State: ctypes.StateRemoving},
	}}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if len(res.Containers) != 0 {
		t.Fatalf("Containers = %+v, want empty", res.Containers)
	}
}

func TestCollectExitedWithinRetention(t *testing.T) {
	finished := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	api := &fakeAPI{
		items: []ctypes.Summary{{ID: "c1", Names: []string{"/job"}, Image: "busybox", State: ctypes.StateExited}},
		inspect: map[string]ctypes.InspectResponse{
			"c1": {RestartCount: 0, State: &ctypes.State{Status: ctypes.StateExited, ExitCode: 1, FinishedAt: finished}},
		},
	}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if len(res.Containers) != 1 {
		t.Fatalf("Containers = %+v, want 1", res.Containers)
	}
	c := res.Containers[0]
	if c.State != "exited" || c.ExitCode == nil || *c.ExitCode != 1 || c.FinishedAtUnixMs == 0 {
		t.Errorf("snapshot = %+v", c)
	}
	if c.CPUPercent != nil || c.MemUsage != nil || c.NetRxRate != nil {
		t.Errorf("exited container has stats fields: %+v", c)
	}
}

func TestCollectExitedPastRetentionSkipped(t *testing.T) {
	finished := time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339Nano)
	api := &fakeAPI{
		items: []ctypes.Summary{{ID: "c1", Names: []string{"/job"}, State: ctypes.StateExited}},
		inspect: map[string]ctypes.InspectResponse{
			"c1": {State: &ctypes.State{Status: ctypes.StateExited, ExitCode: 0, FinishedAt: finished}},
		},
	}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	if res := d.Collect(context.Background()); len(res.Containers) != 0 {
		t.Fatalf("Containers = %+v, want empty", res.Containers)
	}
}

func TestCollectExitedInspectErrorSkipped(t *testing.T) {
	api := &fakeAPI{
		items:      []ctypes.Summary{{ID: "c1", Names: []string{"/job"}, State: ctypes.StateExited}},
		inspectErr: errors.New("boom"),
	}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	if res := d.Collect(context.Background()); len(res.Containers) != 0 {
		t.Fatalf("Containers = %+v, want empty (cannot bound retention)", res.Containers)
	}
}

func TestCollectStatsErrorKeepsContainer(t *testing.T) {
	api := &fakeAPI{
		items:    []ctypes.Summary{runningSummary("c1", "app", "img:1")},
		statsErr: errors.New("boom"),
		inspect:  map[string]ctypes.InspectResponse{"c1": {State: &ctypes.State{Status: ctypes.StateRunning}}},
	}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if len(res.Containers) != 1 {
		t.Fatalf("Containers = %+v, want 1 (kept without stats)", res.Containers)
	}
	if res.Containers[0].CPUPercent != nil || res.Containers[0].MemUsage != nil {
		t.Errorf("stats fields set despite stats error: %+v", res.Containers[0])
	}
}

func TestCollectNetRateSecondCycle(t *testing.T) {
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img:1")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": {State: &ctypes.State{Status: ctypes.StateRunning}}},
	}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	d.Collect(context.Background()) // first sight: retains rx=1024, tx=512 (summed)
	api.statsBody = sampleStats2    // rx=2024, tx=1012
	res := d.Collect(context.Background())
	c := res.Containers[0]
	if c.NetRxRate == nil || *c.NetRxRate <= 0 {
		t.Fatalf("NetRxRate = %v, want > 0 on second cycle", c.NetRxRate)
	}
	if c.CPUPercent == nil || *c.CPUPercent <= 0 { // delta 1e9 over 1e10 sys, 2 cpus → 20%
		t.Fatalf("CPUPercent = %v, want > 0 on second cycle", c.CPUPercent)
	}
}

func TestCollectZeroContainersMarshalsEmptyArray(t *testing.T) {
	api := &fakeAPI{}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	data, err := json.Marshal(res.Containers)
	if err != nil || string(data) != "[]" {
		t.Fatalf("Marshal = %s, %v; want [] (never null)", data, err)
	}
}

func TestCollectAbsent(t *testing.T) {
	api := &fakeAPI{listErr: &os.SyscallError{Syscall: "connect", Err: fs.ErrNotExist}}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if res.Status != DockerAbsent {
		t.Fatalf("Status = %v, want DockerAbsent", res.Status)
	}
	if api.closed == 0 {
		t.Error("expected client to be closed after a failed list")
	}
}

func TestCollectNoPermission(t *testing.T) {
	// Socket path must exist so classify() can distinguish EACCES from "absent".
	sock := t.TempDir() + "/docker.sock"
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{listErr: fs.ErrPermission}
	d, _ := newCollectorWithAPI("unix://"+sock, api)
	res := d.Collect(context.Background())
	if res.Status != DockerNoPermission {
		t.Fatalf("Status = %v, want DockerNoPermission", res.Status)
	}
}

func TestCollectFlappingRecreatesClient(t *testing.T) {
	api := &fakeAPI{listErr: fs.ErrNotExist}
	d, calls := newCollectorWithAPI("unix:///nonexistent.sock", api)

	d.Collect(context.Background()) // absent → client torn down
	if *calls != 1 {
		t.Fatalf("newAPI calls = %d, want 1", *calls)
	}
	api.listErr = nil
	api.items = []ctypes.Summary{{ID: "c1", Names: []string{"/x"}, Image: "i", State: ctypes.StateRunning}}
	api.statsBody = sampleStats
	res := d.Collect(context.Background()) // back → client recreated
	if *calls != 2 {
		t.Fatalf("newAPI calls = %d, want 2 (recreated)", *calls)
	}
	if res.Status != DockerPresent {
		t.Fatalf("Status = %v, want DockerPresent", res.Status)
	}
}

func TestExcludedImages(t *testing.T) {
	d := NewDockerCollector("unix:///x.sock", []string{"busybox*", "redis"}, discardLogger())
	if !d.excluded("busybox:latest") {
		t.Error("busybox:latest should be excluded")
	}
	if !d.excluded("redis") {
		t.Error("redis should be excluded (exact)")
	}
	if d.excluded("nginx:1.27") {
		t.Error("nginx should not be excluded")
	}
	if d.excluded("myredis") {
		t.Error("myredis should not match exact pattern 'redis'")
	}

	// Wildcard must cross "/" so operators can exclude registry-qualified refs.
	d2 := NewDockerCollector("unix:///x.sock", []string{"*nginx*"}, discardLogger())
	if !d2.excluded("ghcr.io/org/nginx:1.27") {
		t.Error("*nginx* should match ghcr.io/org/nginx:1.27 (wildcard crosses /)")
	}

	// "?" matches exactly one character.
	d3 := NewDockerCollector("unix:///x.sock", []string{"nginx:1.2?"}, discardLogger())
	if !d3.excluded("nginx:1.27") {
		t.Error("? should match a single char")
	}
	if d3.excluded("nginx:1.27x") {
		t.Error("? should not match two chars")
	}
}

// manyContainers builds n running containers with strictly increasing Created
// timestamps (newest = highest index), so "newest first" truncation is verifiable.
func manyContainers(n int) []ctypes.Summary {
	items := make([]ctypes.Summary, n)
	for i := range n {
		items[i] = ctypes.Summary{
			ID:      fmt.Sprintf("c%d", i),
			Names:   []string{fmt.Sprintf("/app%d", i)},
			Image:   "img",
			State:   ctypes.StateRunning,
			Created: int64(i), // c(n-1) is newest
		}
	}
	return items
}

// TestCollectCapsAt200NewestFirst pins a25: an unbounded container list produces an
// unbounded, ever-repeating OTLP payload; Collect must cap it, keeping the newest
// containers (the ones an operator most likely cares about right now).
func TestCollectCapsAt200NewestFirst(t *testing.T) {
	api := &fakeAPI{items: manyContainers(250)}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if len(res.Containers) != maxContainers {
		t.Fatalf("len(Containers) = %d, want %d", len(res.Containers), maxContainers)
	}
	// Newest 200 have Created 50..249; none of the oldest 50 (Created 0..49) survive.
	seen := make(map[string]bool, len(res.Containers))
	for _, c := range res.Containers {
		seen[c.ID] = true
	}
	if seen["c0"] || seen["c49"] {
		t.Error("oldest containers were kept; want newest-first truncation")
	}
	if !seen["c249"] {
		t.Error("newest container (c249) was dropped; want it kept")
	}
}

// TestCollectWarnsOnceOnTruncation pins the "WARN once" part of a25: a host that
// stays over the cap must not re-warn every single cycle.
func TestCollectWarnsOnceOnTruncation(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d := NewDockerCollector("unix:///nonexistent.sock", nil, logger)
	api := &fakeAPI{items: manyContainers(250)}
	d.newAPI = func(string) (dockerAPI, error) { return api, nil }

	d.Collect(context.Background())
	d.Collect(context.Background())
	d.Collect(context.Background())

	got := strings.Count(buf.String(), "truncated")
	if got != 1 {
		t.Errorf("truncation WARN logged %d times, want 1 (once, not per cycle)", got)
	}
}

// TestCollectTruncatesLongFields pins the 256-rune truncation of Name/Image/
// ServiceName/ProjectName.
func TestCollectTruncatesLongFields(t *testing.T) {
	long := strings.Repeat("x", 300)
	api := &fakeAPI{items: []ctypes.Summary{{
		ID:     "c1",
		Names:  []string{"/" + long},
		Image:  long,
		State:  ctypes.StateRunning,
		Labels: map[string]string{"com.docker.compose.service": long, "com.docker.compose.project": long},
	}}}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if len(res.Containers) != 1 {
		t.Fatalf("Containers = %+v, want 1", res.Containers)
	}
	c := res.Containers[0]
	for name, v := range map[string]string{"Name": c.Name, "Image": c.Image, "ServiceName": c.ServiceName, "ProjectName": c.ProjectName} {
		if got := len([]rune(v)); got != maxFieldRunes {
			t.Errorf("%s length = %d, want %d (truncated)", name, got, maxFieldRunes)
		}
	}
}

// manyContainersWithImage is like manyContainers but every container carries the
// given image, so exclusion filtering can be pinned to a subset by image.
func manyContainersWithImage(n int, image string, createdStart int) []ctypes.Summary {
	items := make([]ctypes.Summary, n)
	for i := range n {
		items[i] = ctypes.Summary{
			ID:      fmt.Sprintf("%s-%d", image, i),
			Names:   []string{fmt.Sprintf("/%s-%d", image, i)},
			Image:   image,
			State:   ctypes.StateRunning,
			Created: int64(createdStart + i),
		}
	}
	return items
}

// TestCollectExcludeFilterBuysBackRoom pins b15: the image-exclude filter must run
// BEFORE the newest-first sort and the 200 cap, so excluded containers never occupy
// cap slots that real candidates need. 100 excluded containers are the NEWEST (highest
// Created), so a cap-before-filter bug would keep them and wrongly drop real candidates.
func TestCollectExcludeFilterBuysBackRoom(t *testing.T) {
	kept := manyContainersWithImage(150, "img", 0)         // Created 0..149
	excluded := manyContainersWithImage(100, "noise", 150) // Created 150..249 (newest)
	items := append(append([]ctypes.Summary{}, kept...), excluded...)

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d := NewDockerCollector("unix:///nonexistent.sock", []string{"noise"}, logger)
	api := &fakeAPI{items: items}
	d.newAPI = func(string) (dockerAPI, error) { return api, nil }

	res := d.Collect(context.Background())

	if len(res.Containers) != 150 {
		t.Fatalf("len(Containers) = %d, want 150 (all real candidates survive once excludes are filtered first)", len(res.Containers))
	}
	seen := make(map[string]bool, len(res.Containers))
	for _, c := range res.Containers {
		seen[c.ID] = true
	}
	if !seen["img-0"] {
		t.Error("img-0 (oldest real candidate) was dropped; excludes should have bought back its cap slot")
	}
	if strings.Contains(buf.String(), "truncated") {
		t.Errorf("unexpected truncation WARN: post-filter candidate count (150) is under the cap: %s", buf.String())
	}
}

// TestCollectTruncationWarnCountsRealCandidates pins the other half of b15: when the
// cap does bind, the WARN's "total" must count post-filter candidates, not the raw
// (pre-exclude) list length.
func TestCollectTruncationWarnCountsRealCandidates(t *testing.T) {
	kept := manyContainersWithImage(250, "img", 0)        // 250 real candidates, over the cap
	excluded := manyContainersWithImage(10, "noise", 250) // 10 more, all excluded
	items := append(append([]ctypes.Summary{}, kept...), excluded...)

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d := NewDockerCollector("unix:///nonexistent.sock", []string{"noise"}, logger)
	api := &fakeAPI{items: items}
	d.newAPI = func(string) (dockerAPI, error) { return api, nil }

	res := d.Collect(context.Background())

	if len(res.Containers) != maxContainers {
		t.Fatalf("len(Containers) = %d, want %d", len(res.Containers), maxContainers)
	}
	out := buf.String()
	if !strings.Contains(out, "total=250") {
		t.Errorf("WARN total should count the 250 real candidates, not the raw 260-item list: %s", out)
	}
	if strings.Contains(out, "total=260") {
		t.Errorf("WARN total counted excluded containers: %s", out)
	}
}

// TestCollectStripsControlCharacters pins s12: C0 control characters (incl. an ANSI
// escape sequence) and DEL are stripped from Name/Image/ServiceName/ProjectName before
// the 256-rune truncation, so the agent is not a silent pass-through for attacker-chosen
// control sequences into the backend/dashboard.
func TestCollectStripsControlCharacters(t *testing.T) {
	dirty := "app\x1b[31m-evil\x00name"
	api := &fakeAPI{items: []ctypes.Summary{{
		ID:     "c1",
		Names:  []string{"/" + dirty},
		Image:  dirty,
		State:  ctypes.StateRunning,
		Labels: map[string]string{"com.docker.compose.service": dirty, "com.docker.compose.project": dirty},
	}}}
	d, _ := newCollectorWithAPI("unix:///nonexistent.sock", api)
	res := d.Collect(context.Background())
	if len(res.Containers) != 1 {
		t.Fatalf("Containers = %+v, want 1", res.Containers)
	}
	want := "app[31m-evilname" // \x1b and \x00 removed, everything else kept
	c := res.Containers[0]
	for name, v := range map[string]string{"Name": c.Name, "Image": c.Image, "ServiceName": c.ServiceName, "ProjectName": c.ProjectName} {
		if v != want {
			t.Errorf("%s = %q, want %q (control chars stripped)", name, v, want)
		}
		if strings.ContainsAny(v, "\x1b\x00") {
			t.Errorf("%s still contains a control character: %q", name, v)
		}
	}
}

// TestCollectLogsInspectAndStatsErrors pins a26: both swallowed error paths must be
// visible at Debug with the container id attached.
func TestCollectLogsInspectAndStatsErrors(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	api := &fakeAPI{
		items:      []ctypes.Summary{runningSummary("c1", "app", "img")},
		inspectErr: errors.New("inspect boom"),
		statsErr:   errors.New("stats boom"),
	}
	d := NewDockerCollector("unix:///nonexistent.sock", nil, logger)
	d.newAPI = func(string) (dockerAPI, error) { return api, nil }
	d.Collect(context.Background())

	out := buf.String()
	if !strings.Contains(out, "c1") {
		t.Errorf("log output missing container id: %s", out)
	}
	if !strings.Contains(out, "inspect boom") {
		t.Errorf("log output missing inspect error: %s", out)
	}
}
