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

	units "github.com/docker/go-units"
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

	inspectCalls map[string]int // per ID, counted also when inspectErr is returned
	onList       func()         // runs inside ContainerList (a test clock moving during the call)
}

func (f *fakeAPI) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	if f.onList != nil {
		f.onList()
	}
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	return client.ContainerListResult{Items: f.items}, nil
}

func (f *fakeAPI) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if f.inspectCalls == nil {
		f.inspectCalls = map[string]int{}
	}
	f.inspectCalls[id]++
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

// ---- Part 3: inspect only when the list row does not prove the remembered values ----

// fakeClock drives DockerCollector.clock in tests.
type fakeClock struct{ at instant }

func (c *fakeClock) now() instant { return c.at }

// advance moves both clocks.
func (c *fakeClock) advance(d time.Duration) { c.at = c.at.plus(d, d) }

// stepWall moves the wall clock only (a suspend, or a clock step).
func (c *fakeClock) stepWall(d time.Duration) { c.at.Wall = c.at.Wall.Add(d) }

func newCollectorWithClock(api dockerAPI, clock *fakeClock) *DockerCollector {
	d := NewDockerCollector("unix:///nonexistent.sock", nil, discardLogger())
	d.newAPI = func(string) (dockerAPI, error) { return api, nil }
	d.clock = clock.now
	return d
}

func upStatus(now, started time.Time) string {
	return "Up " + units.HumanDuration(now.Sub(started))
}

func exitedStatus(now, finished time.Time, code int) string {
	return fmt.Sprintf("Exited (%d) %s ago", code, units.HumanDuration(now.Sub(finished)))
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func runningInspect(started time.Time, restarts int) ctypes.InspectResponse {
	return ctypes.InspectResponse{RestartCount: restarts,
		State: &ctypes.State{Status: ctypes.StateRunning, StartedAt: rfc(started)}}
}

func exitedInspect(finished time.Time, code, restarts int) ctypes.InspectResponse {
	return ctypes.InspectResponse{RestartCount: restarts,
		State: &ctypes.State{Status: ctypes.StateExited, StartedAt: rfc(finished.Add(-time.Hour)), FinishedAt: rfc(finished), ExitCode: code}}
}

func exitedSummary(id, name string) ctypes.Summary {
	return ctypes.Summary{ID: id, Names: []string{"/" + name}, Image: "img", State: ctypes.StateExited}
}

func findContainer(cs []ContainerStats, id string) (ContainerStats, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return ContainerStats{}, false
}

func mustFind(t *testing.T, cs []ContainerStats, id string) ContainerStats {
	t.Helper()
	c, ok := findContainer(cs, id)
	if !ok {
		t.Fatalf("container %s missing from snapshot %+v", id, cs)
	}
	return c
}

func TestCollectReusesInspectOnQuietHost(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	t0 := clock.at.Wall
	c1Start, c2Finish := t0.Add(-72*time.Hour), t0.Add(-150*24*time.Hour)
	s1 := runningSummary("c1", "app", "img")
	s1.Health = &ctypes.HealthSummary{Status: ctypes.Healthy}
	api := &fakeAPI{
		items:     []ctypes.Summary{s1, exitedSummary("c2", "old")},
		statsBody: sampleStats,
		inspect: map[string]ctypes.InspectResponse{
			"c1": runningInspect(c1Start, 2),
			"c2": exitedInspect(c2Finish, 255, 0),
		},
	}
	d := newCollectorWithClock(api, clock)
	var startedMs int64
	for cycle := 1; cycle <= 3; cycle++ {
		now := clock.at.Wall
		api.items[0].Status = upStatus(now, c1Start) + " (healthy)"
		api.items[1].Status = exitedStatus(now, c2Finish, 255)
		res := d.Collect(context.Background())
		c1 := mustFind(t, res.Containers, "c1")
		if c1.RestartCount != 2 || c1.StartedAtUnixMs != c1Start.UnixMilli() {
			t.Errorf("cycle %d: c1 = %+v", cycle, c1)
		}
		if cycle > 1 && c1.StartedAtUnixMs != startedMs {
			t.Errorf("cycle %d: StartedAtUnixMs changed", cycle)
		}
		startedMs = c1.StartedAtUnixMs
		if _, ok := findContainer(res.Containers, "c2"); ok {
			t.Errorf("cycle %d: c2 (exited 5 months ago) in the snapshot", cycle)
		}
		if cycle == 1 && (api.inspectCalls["c1"] != 1 || api.inspectCalls["c2"] != 1) {
			t.Errorf("after cycle 1: inspect calls = %v, want 1 each", api.inspectCalls)
		}
		clock.advance(60 * time.Second)
	}
	if api.inspectCalls["c1"] != 1 || api.inspectCalls["c2"] != 1 {
		t.Errorf("after cycle 3: inspect calls = %v, want still 1 each", api.inspectCalls)
	}
}

// TestCollectChecksFromBeforeTheListCall pins spec 4.1: an entry's "checked" time is
// the reading taken before the list call, not after it. Anything the list does not show
// yet may have happened while the call ran (a stop becomes visible up to 32 s late), so a
// slow list call must not move the horizon forward. Here the list call takes 50 s.
func TestCollectChecksFromBeforeTheListCall(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	t0 := clock.at.Wall
	started := t0.Add(-60 * time.Second)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(started, 0)},
	}
	slowList := func() {
		api.items[0].Status = upStatus(clock.at.Wall, started) // Docker formats as the call starts
		clock.advance(50 * time.Second)
	}
	d := newCollectorWithClock(api, clock)

	// Cycle 1 at t0: first sight, inspected. The list call ends at t0+50s.
	api.onList = slowList
	d.Collect(context.Background())
	if got := d.memo["c1"].Checked.Wall; !got.Equal(t0) {
		t.Fatalf("after an inspect: Checked = %v, want the reading before the list call (%v)", got, t0)
	}
	if got := d.memo["c1"].Inspected.Wall; !got.Equal(t0) {
		t.Fatalf("Inspected = %v, want the reading before the list call (%v)", got, t0)
	}

	// Cycle 2 at t0+600s with a quick list: age 660 s ("11 minutes") equals the horizon
	// from t0 (600 s + 60 s slack), so the container is inspected. From the reading after
	// cycle 1's list (t0+50s) the horizon would be 610 s ("10 minutes"), a wrong reuse.
	api.onList = func() { api.items[0].Status = upStatus(clock.at.Wall, started) }
	clock.advance(550 * time.Second)
	d.Collect(context.Background())
	if api.inspectCalls["c1"] != 2 {
		t.Fatalf("cycle 2: inspect calls = %d, want 2 (the horizon runs from before cycle 1's list call)", api.inspectCalls["c1"])
	}

	// Cycle 3 at t0+660s, slow list again: reused, and Checked moves to this cycle's
	// reading before the list call (t0+660s), not after it (t0+710s).
	api.onList = slowList
	clock.advance(60 * time.Second)
	cycle3 := clock.at.Wall
	d.Collect(context.Background())
	if api.inspectCalls["c1"] != 2 {
		t.Fatalf("cycle 3: inspect calls = %d, want still 2 (reused)", api.inspectCalls["c1"])
	}
	if got := d.memo["c1"].Checked.Wall; !got.Equal(cycle3) {
		t.Errorf("after a reuse: Checked = %v, want the reading before the list call (%v)", got, cycle3)
	}
}

func TestCollectInspectsAfterRestart(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	c1Start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(c1Start, 2)},
	}
	d := newCollectorWithClock(api, clock)
	for range 2 {
		api.items[0].Status = upStatus(clock.at.Wall, c1Start)
		d.Collect(context.Background())
		clock.advance(60 * time.Second)
	}
	newStart := clock.at.Wall.Add(-20 * time.Second)
	api.inspect["c1"] = runningInspect(newStart, 3)
	api.items[0].Status = "Up 20 seconds"
	res := d.Collect(context.Background())
	if api.inspectCalls["c1"] != 2 {
		t.Errorf("inspect calls = %d, want 2 (cycle 3 inspects)", api.inspectCalls["c1"])
	}
	c1 := mustFind(t, res.Containers, "c1")
	if c1.RestartCount != 3 || c1.StartedAtUnixMs != newStart.UnixMilli() {
		t.Errorf("snapshot after restart = %+v, want restart count 3 and the new start", c1)
	}
}

func TestCollectYoungContainerInspectedUntilThreeMinutes(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	c1Start := clock.at.Wall.Add(-30 * time.Second)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(c1Start, 0)},
	}
	d := newCollectorWithClock(api, clock)
	for cycle := 1; cycle <= 4; cycle++ {
		api.items[0].Status = upStatus(clock.at.Wall, c1Start)
		d.Collect(context.Background())
		clock.advance(60 * time.Second)
		if cycle == 3 && api.inspectCalls["c1"] != 3 {
			t.Errorf("after cycle 3 (ages 30, 90, 150 s): calls = %d, want 3", api.inspectCalls["c1"])
		}
	}
	if api.inspectCalls["c1"] != 3 {
		t.Errorf("after cycle 4 (age 210 s): calls = %d, want still 3", api.inspectCalls["c1"])
	}
}

func TestCollectStateChangeInspects(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	c1Start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(c1Start, 0)},
	}
	d := newCollectorWithClock(api, clock)
	for range 2 {
		api.items[0].Status = upStatus(clock.at.Wall, c1Start)
		d.Collect(context.Background())
		clock.advance(60 * time.Second)
	}
	finish := clock.at.Wall.Add(-10 * time.Second)
	api.items[0].State = ctypes.StateExited
	api.items[0].Status = "Exited (0) 10 seconds ago"
	api.inspect["c1"] = exitedInspect(finish, 0, 0)
	res := d.Collect(context.Background())
	if api.inspectCalls["c1"] != 2 {
		t.Errorf("inspect calls = %d, want 2 (the state change inspects)", api.inspectCalls["c1"])
	}
	c1 := mustFind(t, res.Containers, "c1")
	if c1.State != "exited" || c1.ExitCode == nil || *c1.ExitCode != 0 || c1.FinishedAtUnixMs != finish.UnixMilli() {
		t.Errorf("snapshot = %+v", c1)
	}
}

func TestCollectHourlyRefresh(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	c1Start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(c1Start, 0)},
	}
	d := newCollectorWithClock(api, clock)
	for cycle := 1; cycle <= 61; cycle++ {
		api.items[0].Status = upStatus(clock.at.Wall, c1Start)
		d.Collect(context.Background())
		if cycle == 60 && api.inspectCalls["c1"] != 1 {
			t.Errorf("after cycle 60: calls = %d, want 1", api.inspectCalls["c1"])
		}
		clock.advance(60 * time.Second)
	}
	if api.inspectCalls["c1"] != 2 {
		t.Errorf("after 61 cycles: calls = %d, want 2 (cycle 1 and cycle 61)", api.inspectCalls["c1"])
	}
}

func TestCollectExitedWithRestartCountInspectsEveryCycle(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	finish := clock.at.Wall.Add(-2 * time.Hour)
	api := &fakeAPI{
		items:   []ctypes.Summary{exitedSummary("c1", "job")},
		inspect: map[string]ctypes.InspectResponse{"c1": exitedInspect(finish, 1, 3)},
	}
	d := newCollectorWithClock(api, clock)
	for cycle := 1; cycle <= 3; cycle++ {
		api.items[0].Status = exitedStatus(clock.at.Wall, finish, 1)
		res := d.Collect(context.Background())
		if c1 := mustFind(t, res.Containers, "c1"); c1.RestartCount != 3 {
			t.Errorf("cycle %d: restart_count = %d, want 3", cycle, c1.RestartCount)
		}
		clock.advance(60 * time.Second)
	}
	if api.inspectCalls["c1"] != 3 {
		t.Errorf("calls = %d, want 3 (every cycle)", api.inspectCalls["c1"])
	}
}

func TestCollectInspectErrorIsNotRemembered(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	c1Start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:      []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody:  sampleStats,
		inspect:    map[string]ctypes.InspectResponse{"c1": runningInspect(c1Start, 0)},
		inspectErr: errors.New("boom"),
	}
	d := newCollectorWithClock(api, clock)
	for cycle := 1; cycle <= 3; cycle++ {
		api.items[0].Status = upStatus(clock.at.Wall, c1Start)
		res := d.Collect(context.Background())
		c1 := mustFind(t, res.Containers, "c1")
		switch cycle {
		case 1:
			if c1.StartedAtUnixMs != 0 {
				t.Errorf("cycle 1 (inspect failed): StartedAtUnixMs = %d, want 0", c1.StartedAtUnixMs)
			}
			api.inspectErr = nil
		case 2:
			if api.inspectCalls["c1"] != 2 || c1.StartedAtUnixMs != c1Start.UnixMilli() {
				t.Errorf("cycle 2: calls = %d, snapshot %+v; want inspected with fields", api.inspectCalls["c1"], c1)
			}
		case 3:
			if api.inspectCalls["c1"] != 2 {
				t.Errorf("cycle 3: calls = %d, want 2 (reused)", api.inspectCalls["c1"])
			}
		}
		clock.advance(60 * time.Second)
	}
}

func TestCollectNilStateIsNotRemembered(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": {RestartCount: 4}},
	}
	d := newCollectorWithClock(api, clock)
	for cycle := 1; cycle <= 2; cycle++ {
		api.items[0].Status = "Up 3 days"
		res := d.Collect(context.Background())
		if c1 := mustFind(t, res.Containers, "c1"); c1.RestartCount != 4 || c1.StartedAtUnixMs != 0 {
			t.Errorf("cycle %d: snapshot = %+v, want restart count 4 and no start", cycle, c1)
		}
		clock.advance(60 * time.Second)
	}
	if api.inspectCalls["c1"] != 2 {
		t.Errorf("calls = %d, want 2 (nothing remembered)", api.inspectCalls["c1"])
	}
}

func TestCollectCrashLoopTracksRestartCount(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{},
	}
	d := newCollectorWithClock(api, clock)
	for cycle := 1; cycle <= 5; cycle++ {
		now := clock.at.Wall
		if cycle%2 == 1 {
			api.items[0].State, api.items[0].Status = ctypes.StateRunning, "Up 12 seconds"
			api.inspect["c1"] = runningInspect(now.Add(-12*time.Second), cycle)
		} else {
			api.items[0].State, api.items[0].Status = ctypes.StateRestarting, "Restarting (1) 3 seconds ago"
			api.inspect["c1"] = ctypes.InspectResponse{RestartCount: cycle, State: &ctypes.State{
				Status: ctypes.StateRestarting, StartedAt: rfc(now.Add(-50 * time.Second)),
				FinishedAt: rfc(now.Add(-3 * time.Second)), ExitCode: 1}}
		}
		res := d.Collect(context.Background())
		if c1 := mustFind(t, res.Containers, "c1"); c1.RestartCount != cycle {
			t.Errorf("cycle %d: restart_count = %d, want %d", cycle, c1.RestartCount, cycle)
		}
		clock.advance(60 * time.Second)
	}
	if api.inspectCalls["c1"] != 5 {
		t.Errorf("calls = %d, want 5", api.inspectCalls["c1"])
	}
}

// gapFixture: c1 started 90 s and c2 10 minutes before cycle 1, both running.
func gapFixture(t *testing.T) (*fakeAPI, *fakeClock, *DockerCollector, time.Time, time.Time) {
	t.Helper()
	clock := &fakeClock{at: memoBase}
	t0 := clock.at.Wall
	s1, s2 := t0.Add(-90*time.Second), t0.Add(-10*time.Minute)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "a", "img"), runningSummary("c2", "b", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(s1, 0), "c2": runningInspect(s2, 0)},
	}
	d := newCollectorWithClock(api, clock)
	api.items[0].Status, api.items[1].Status = upStatus(t0, s1), upStatus(t0, s2)
	d.Collect(context.Background())
	return api, clock, d, s1, s2
}

func TestCollectMissedCycleWidensTheGap(t *testing.T) {
	api, clock, d, s1, s2 := gapFixture(t)
	clock.advance(30 * time.Minute)
	api.items[0].Status, api.items[1].Status = upStatus(clock.at.Wall, s1), upStatus(clock.at.Wall, s2)
	if api.items[0].Status != "Up 31 minutes" || api.items[1].Status != "Up 40 minutes" {
		t.Fatalf("fixture statuses = %q, %q", api.items[0].Status, api.items[1].Status)
	}
	d.Collect(context.Background())
	if api.inspectCalls["c1"] != 2 {
		t.Errorf("c1 calls = %d, want 2 (its age reads like the gap)", api.inspectCalls["c1"])
	}
	if api.inspectCalls["c2"] != 1 {
		t.Errorf("c2 calls = %d, want 1 (reused)", api.inspectCalls["c2"])
	}
}

func TestCollectSuspendWidensTheGap(t *testing.T) {
	api, clock, d, s1, s2 := gapFixture(t)
	clock.stepWall(30 * time.Minute)
	clock.advance(60 * time.Second)
	api.items[0].Status, api.items[1].Status = upStatus(clock.at.Wall, s1), upStatus(clock.at.Wall, s2)
	if api.items[0].Status != "Up 32 minutes" || api.items[1].Status != "Up 41 minutes" {
		t.Fatalf("fixture statuses = %q, %q", api.items[0].Status, api.items[1].Status)
	}
	d.Collect(context.Background())
	if api.inspectCalls["c1"] != 2 {
		t.Errorf("c1 calls = %d, want 2 (wall clock says 31 minutes passed)", api.inspectCalls["c1"])
	}
	if api.inspectCalls["c2"] != 1 {
		t.Errorf("c2 calls = %d, want 1 (reused)", api.inspectCalls["c2"])
	}
}

func TestCollectHealthFlipDoesNotInspect(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	c1Start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(c1Start, 0)},
	}
	d := newCollectorWithClock(api, clock)
	for _, h := range []ctypes.HealthStatus{ctypes.Healthy, ctypes.Unhealthy} {
		api.items[0].Health = &ctypes.HealthSummary{Status: h}
		api.items[0].Status = upStatus(clock.at.Wall, c1Start) + " (" + string(h) + ")"
		res := d.Collect(context.Background())
		if c1 := mustFind(t, res.Containers, "c1"); c1.Health != string(h) {
			t.Errorf("health = %q, want %q", c1.Health, h)
		}
		clock.advance(60 * time.Second)
	}
	if api.inspectCalls["c1"] != 1 {
		t.Errorf("calls = %d, want 1 (a health flip is not a restart)", api.inspectCalls["c1"])
	}
}

func TestCollectPausedContainer(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	c1Start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{},
	}
	d := newCollectorWithClock(api, clock)
	states := []ctypes.ContainerState{ctypes.StateRunning, ctypes.StatePaused, ctypes.StatePaused, ctypes.StateRunning}
	wantCalls := []int{1, 2, 2, 3}
	for i, st := range states {
		api.items[0].State = st
		api.items[0].Status = upStatus(clock.at.Wall, c1Start)
		if st == ctypes.StatePaused {
			api.items[0].Status += " (Paused)"
		}
		api.inspect["c1"] = ctypes.InspectResponse{State: &ctypes.State{Status: st, StartedAt: rfc(c1Start)}}
		res := d.Collect(context.Background())
		if c1 := mustFind(t, res.Containers, "c1"); c1.StartedAtUnixMs != c1Start.UnixMilli() {
			t.Errorf("cycle %d: StartedAtUnixMs = %d, want %d", i+1, c1.StartedAtUnixMs, c1Start.UnixMilli())
		}
		if api.inspectCalls["c1"] != wantCalls[i] {
			t.Errorf("after cycle %d (%s): calls = %d, want %d", i+1, st, api.inspectCalls["c1"], wantCalls[i])
		}
		clock.advance(60 * time.Second)
	}
}

func TestCollectRecreatedContainerGetsItsOwnEntry(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect: map[string]ctypes.InspectResponse{
			"c1": runningInspect(start, 0),
			"c2": runningInspect(clock.at.Wall.Add(30*time.Second), 0),
		},
	}
	d := newCollectorWithClock(api, clock)
	api.items[0].Status = upStatus(clock.at.Wall, start)
	d.Collect(context.Background())
	clock.advance(60 * time.Second)
	api.items = []ctypes.Summary{runningSummary("c2", "app", "img")}
	api.items[0].Status = "Up 30 seconds"
	d.Collect(context.Background())
	if api.inspectCalls["c2"] != 1 {
		t.Errorf("c2 calls = %d, want 1", api.inspectCalls["c2"])
	}
	if _, ok := d.memo["c2"]; len(d.memo) != 1 || !ok {
		t.Errorf("memo = %+v, want only c2's entry", d.memo)
	}
}

// inspectedFields is the part of a snapshot entry that comes from inspect, plus identity.
type inspectedFields struct {
	ID, State                 string
	RestartCount              int
	StartedAtMs, FinishedAtMs int64
	ExitCode                  int
	HasExitCode               bool
}

func fieldsOf(cs []ContainerStats) []inspectedFields {
	out := make([]inspectedFields, len(cs))
	for i, c := range cs {
		out[i] = inspectedFields{ID: c.ID, State: c.State, RestartCount: c.RestartCount,
			StartedAtMs: c.StartedAtUnixMs, FinishedAtMs: c.FinishedAtUnixMs, HasExitCode: c.ExitCode != nil}
		if c.ExitCode != nil {
			out[i].ExitCode = *c.ExitCode
		}
	}
	return out
}

func TestCollectUnknownStatusWordingInspectsEveryCycle(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	c1Start, c2Finish := clock.at.Wall.Add(-72*time.Hour), clock.at.Wall.Add(-time.Hour)
	mk := func() *fakeAPI {
		return &fakeAPI{
			items:     []ctypes.Summary{runningSummary("c1", "app", "img"), exitedSummary("c2", "job")},
			statsBody: sampleStats,
			inspect: map[string]ctypes.InspectResponse{
				"c1": runningInspect(c1Start, 1),
				"c2": exitedInspect(c2Finish, 0, 0),
			},
		}
	}
	api, refAPI := mk(), mk()
	d := newCollectorWithClock(api, clock)
	for cycle := 1; cycle <= 3; cycle++ {
		for _, a := range []*fakeAPI{api, refAPI} {
			a.items[0].Status, a.items[1].Status = "running since Tuesday", ""
		}
		got := fieldsOf(d.Collect(context.Background()).Containers)
		want := fieldsOf(newCollectorWithClock(refAPI, clock).Collect(context.Background()).Containers)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("cycle %d: snapshot %+v, want %+v", cycle, got, want)
		}
		clock.advance(60 * time.Second)
	}
	if api.inspectCalls["c1"] != 3 || api.inspectCalls["c2"] != 3 {
		t.Errorf("calls = %v, want 3 each", api.inspectCalls)
	}
}

func TestCollectMemoBoundedByCap(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	start := clock.at.Wall.Add(-72 * time.Hour)
	items := manyContainers(250)
	inspect := make(map[string]ctypes.InspectResponse, len(items))
	for i := range items {
		items[i].Status = upStatus(clock.at.Wall, start)
		inspect[items[i].ID] = runningInspect(start, 0)
	}
	api := &fakeAPI{items: items, statsBody: sampleStats, inspect: inspect}
	d := newCollectorWithClock(api, clock)
	for cycle := 1; cycle <= 2; cycle++ {
		d.Collect(context.Background())
		if len(d.memo) != maxContainers {
			t.Errorf("cycle %d: len(memo) = %d, want %d", cycle, len(d.memo), maxContainers)
		}
		clock.advance(60 * time.Second)
	}
}

func TestCollectExcludedImageIsNotRemembered(t *testing.T) {
	clock := &fakeClock{at: memoBase}
	start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img"), runningSummary("c2", "noise", "noise:1")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(start, 0), "c2": runningInspect(start, 0)},
	}
	d := NewDockerCollector("unix:///nonexistent.sock", []string{"noise*"}, discardLogger())
	d.newAPI = func(string) (dockerAPI, error) { return api, nil }
	d.clock = clock.now
	api.items[0].Status, api.items[1].Status = upStatus(clock.at.Wall, start), upStatus(clock.at.Wall, start)
	d.Collect(context.Background())
	if _, ok := d.memo["c2"]; ok || api.inspectCalls["c2"] != 0 {
		t.Errorf("excluded c2: memo entry %v, inspect calls %d; want none", ok, api.inspectCalls["c2"])
	}
	if _, ok := d.memo["c1"]; !ok {
		t.Error("c1 not remembered")
	}
}

func TestCollectLogsInspectCount(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clock := &fakeClock{at: memoBase}
	start := clock.at.Wall.Add(-72 * time.Hour)
	api := &fakeAPI{
		items:     []ctypes.Summary{runningSummary("c1", "app", "img")},
		statsBody: sampleStats,
		inspect:   map[string]ctypes.InspectResponse{"c1": runningInspect(start, 0)},
	}
	d := NewDockerCollector("unix:///nonexistent.sock", nil, logger)
	d.newAPI = func(string) (dockerAPI, error) { return api, nil }
	d.clock = clock.now
	for range 2 {
		api.items[0].Status = upStatus(clock.at.Wall, start)
		d.Collect(context.Background())
		clock.advance(60 * time.Second)
	}
	var lines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `msg="docker: cycle"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("docker: cycle lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "containers=1") || !strings.Contains(lines[0], "inspected=1") {
		t.Errorf("first cycle line = %q, want containers=1 inspected=1", lines[0])
	}
	if !strings.Contains(lines[1], "inspected=0") {
		t.Errorf("second cycle line = %q, want inspected=0", lines[1])
	}
}
