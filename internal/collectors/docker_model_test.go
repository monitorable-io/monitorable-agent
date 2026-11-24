package collectors

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	units "github.com/docker/go-units"
	ctypes "github.com/moby/moby/api/types/container"
	client "github.com/moby/moby/client"
)

// version is one state of a model container, visible to the list and to inspect from
// visibleFrom on (Docker stamps a stop's finish time at exit and makes it visible later).
type version struct {
	state        string // running|paused|restarting|exited
	startedAt    time.Time
	finishedAt   time.Time
	exitCode     int
	restartCount int
	visibleFrom  time.Time
}

type modelContainer struct {
	id       string
	created  int64
	versions []version
}

func (c *modelContainer) last() version { return c.versions[len(c.versions)-1] }

// at returns the newest version visible at now. visibleFrom never decreases along
// versions, so that is the last one at or before now.
func (c *modelContainer) at(now time.Time) version {
	v := c.versions[0]
	for _, w := range c.versions[1:] {
		if w.visibleFrom.After(now) {
			break
		}
		v = w
	}
	return v
}

// add appends a version, never visible earlier than the one before it.
func (c *modelContainer) add(v version) {
	if prev := c.last(); v.visibleFrom.Before(prev.visibleFrom) {
		v.visibleFrom = prev.visibleFrom
	}
	c.versions = append(c.versions, v)
}

// modelDaemon is a dockerAPI whose list and inspect answer from the same versions, with
// the list's Status built as the daemon builds it.
type modelDaemon struct {
	clock      *fakeClock
	containers []*modelContainer
}

func modelStatus(v version, now time.Time) string {
	switch v.state {
	case "running":
		return "Up " + units.HumanDuration(now.Sub(v.startedAt))
	case "paused":
		return "Up " + units.HumanDuration(now.Sub(v.startedAt)) + " (Paused)"
	case "restarting":
		return fmt.Sprintf("Restarting (%d) %s ago", v.exitCode, units.HumanDuration(now.Sub(v.finishedAt)))
	default: // exited
		return fmt.Sprintf("Exited (%d) %s ago", v.exitCode, units.HumanDuration(now.Sub(v.finishedAt)))
	}
}

func (m *modelDaemon) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	now := m.clock.at.Wall
	items := make([]ctypes.Summary, 0, len(m.containers))
	for _, c := range m.containers {
		v := c.at(now)
		items = append(items, ctypes.Summary{ID: c.id, Names: []string{"/" + c.id}, Image: "img",
			State: ctypes.ContainerState(v.state), Status: modelStatus(v, now), Created: c.created})
	}
	return client.ContainerListResult{Items: items}, nil
}

func (m *modelDaemon) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	for _, c := range m.containers {
		if c.id != id {
			continue
		}
		v := c.at(m.clock.at.Wall)
		ts := func(t time.Time) string {
			if t.IsZero() {
				return "0001-01-01T00:00:00Z"
			}
			return rfc(t)
		}
		return client.ContainerInspectResult{Container: ctypes.InspectResponse{RestartCount: v.restartCount,
			State: &ctypes.State{Status: ctypes.ContainerState(v.state), ExitCode: v.exitCode,
				StartedAt: ts(v.startedAt), FinishedAt: ts(v.finishedAt)}}}, nil
	}
	return client.ContainerInspectResult{}, fmt.Errorf("no such container: %s", id)
}

func (m *modelDaemon) ContainerStats(_ context.Context, _ string, _ client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	return client.ContainerStatsResult{Body: io.NopCloser(strings.NewReader(sampleStats))}, nil
}

func (m *modelDaemon) Close() error { return nil }

// countingAPI counts one collector's inspect calls on a shared daemon.
type countingAPI struct {
	dockerAPI
	inspects int
}

func (c *countingAPI) ContainerInspect(ctx context.Context, id string, o client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	c.inspects++
	return c.dockerAPI.ContainerInspect(ctx, id, o)
}

// modelEvents applies one random event, at time t, to a container (plan Task 2, step 3).
func modelEvent(r *rand.Rand, c *modelContainer, t time.Time) {
	dur := func(lo, hi time.Duration) time.Duration { return lo + time.Duration(r.Int64N(int64(hi-lo)+1)) }
	startVisible := func(start time.Time) time.Time { return start.Add(dur(0, 50*time.Millisecond)) }
	stopVisible := func(finish time.Time) time.Time { return finish.Add(dur(0, 32*time.Second)) }

	cur := c.last()
	if t.Before(cur.visibleFrom) { // an event acts on what the daemon has finished processing
		t = cur.visibleFrom
	}
	switch cur.state {
	case "running":
		switch r.IntN(5) {
		case 0: // stop
			v := cur
			v.state, v.finishedAt, v.exitCode = "exited", t, []int{0, 1, 137}[r.IntN(3)]
			v.visibleFrom = stopVisible(t)
			c.add(v)
		case 1: // crash under a restart policy
			v := cur
			v.state, v.finishedAt, v.exitCode, v.restartCount = "restarting", t, 1, cur.restartCount+1
			v.visibleFrom = startVisible(t)
			c.add(v)
			w := v
			w.state, w.startedAt = "running", t.Add(dur(100*time.Millisecond, 2*time.Second))
			w.visibleFrom = startVisible(w.startedAt)
			c.add(w)
		case 2: // crash, then docker stop during the backoff, old daemon
			// SetRestarting stamps the daemon's own clock as the finish time when it
			// handles the exit (up to 32 s late); a stop during the backoff (at most
			// 1 min) replaces it with the earlier, true exit time.
			v := cur
			v.state, v.finishedAt, v.exitCode, v.restartCount = "restarting", t.Add(dur(300*time.Millisecond, 32*time.Second)), 1, cur.restartCount+1
			v.visibleFrom = v.finishedAt
			c.add(v)
			w := v
			w.state, w.finishedAt = "exited", t
			w.visibleFrom = v.finishedAt.Add(dur(100*time.Millisecond, time.Minute))
			c.add(w)
		case 3: // docker restart
			v := cur
			v.state, v.finishedAt, v.exitCode = "exited", t, 0
			v.visibleFrom = stopVisible(t)
			c.add(v)
			w := v
			w.state, w.startedAt, w.restartCount = "running", t.Add(dur(50*time.Millisecond, time.Second)), 0
			w.visibleFrom = startVisible(w.startedAt)
			c.add(w)
		case 4: // pause
			v := cur
			v.state, v.visibleFrom = "paused", startVisible(t)
			c.add(v)
		}
	case "paused": // unpause
		v := cur
		v.state, v.visibleFrom = "running", startVisible(t)
		c.add(v)
	case "exited":
		switch r.IntN(3) {
		case 0: // start
			v := cur
			v.state, v.startedAt, v.restartCount = "running", t, 0
			v.visibleFrom = startVisible(t)
			c.add(v)
		case 1: // start, exits again within 5 s
			v := cur
			v.state, v.startedAt, v.restartCount = "running", t, 0
			v.visibleFrom = startVisible(t)
			c.add(v)
			w := v
			w.state, w.finishedAt, w.exitCode = "exited", t.Add(dur(0, 5*time.Second)), r.IntN(2)
			w.visibleFrom = stopVisible(w.finishedAt)
			c.add(w)
		case 2: // a start that fails early: the count is reset, the times are not touched
			v := cur
			v.restartCount = 0
			if v.exitCode == 0 {
				v.exitCode = 128
			}
			v.visibleFrom = startVisible(t)
			c.add(v)
		}
	}
}

// TestReuseNeverDiffersFromInspect checks the anti-goal directly (spec section 2): a
// collector with memory against one without, on the same model daemon, cycle by cycle.
func TestReuseNeverDiffersFromInspect(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 2026))
	clock := &fakeClock{at: memoBase}
	t0 := clock.at.Wall
	logUniform := func(lo, hi time.Duration) time.Duration {
		return time.Duration(math.Exp(math.Log(float64(lo)) + r.Float64()*(math.Log(float64(hi))-math.Log(float64(lo)))))
	}

	daemon := &modelDaemon{clock: clock}
	for i := range 20 {
		c := &modelContainer{id: fmt.Sprintf("m%02d", i), created: int64(i)}
		started := t0.Add(-logUniform(time.Second, 150*24*time.Hour))
		v := version{state: "running", startedAt: started, visibleFrom: started}
		if i < 5 {
			finished := t0.Add(-logUniform(time.Second, 150*24*time.Hour))
			v = version{state: "exited", startedAt: finished.Add(-logUniform(time.Second, 30*24*time.Hour)),
				finishedAt: finished, exitCode: []int{0, 1, 2, 137}[r.IntN(4)], restartCount: r.IntN(5),
				visibleFrom: finished}
		}
		c.versions = []version{v}
		daemon.containers = append(daemon.containers, c)
	}

	underTest := &countingAPI{dockerAPI: daemon}
	reference := &countingAPI{dockerAPI: daemon}
	d := newCollectorWithClock(underTest, clock)

	const cycles = 5000
	for cycle := 1; cycle <= cycles; cycle++ {
		got := fieldsOf(d.Collect(context.Background()).Containers)
		ref := newCollectorWithClock(reference, clock)
		want := fieldsOf(ref.Collect(context.Background()).Containers)
		if len(got) != len(want) {
			t.Fatalf("cycle %d: %d containers, reference %d\n got %+v\nwant %+v", cycle, len(got), len(want), got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("cycle %d: container %s differs from a fresh inspect\n got %+v\nwant %+v", cycle, want[i].ID, got[i], want[i])
			}
		}

		// The interval to the next cycle, and what happens in it.
		prev := clock.at.Wall
		interval := time.Duration(59500+r.IntN(1001)) * time.Millisecond
		if r.IntN(20) == 0 {
			interval = 2*time.Minute + time.Duration(r.Int64N(int64(3*time.Hour-2*time.Minute)))
		}
		clock.advance(interval)
		if r.IntN(50) == 0 {
			clock.stepWall(time.Duration(r.Int64N(int64(3 * time.Hour))))
		}
		span := clock.at.Wall.Sub(prev)
		for _, c := range daemon.containers {
			if r.Float64() >= 0.15 {
				continue
			}
			at := prev.Add(time.Duration(r.Int64N(int64(span))))
			if r.IntN(4) == 0 {
				// In the 32 s before the next list call: a stop stamped there can still be
				// invisible to that call (spec 4.2, "why the slack").
				at = clock.at.Wall.Add(-time.Duration(r.Int64N(int64(32 * time.Second))))
			}
			modelEvent(r, c, at)
			for r.IntN(3) == 0 {
				// A burst: another event within a minute of the last one becoming visible,
				// so a container can start and restart between two cycles.
				modelEvent(r, c, c.last().visibleFrom.Add(time.Duration(r.Int64N(int64(time.Minute)))))
			}
		}
	}
	t.Logf("inspect calls: with memory %d, without %d (%.1f%%)", underTest.inspects, reference.inspects,
		100*float64(underTest.inspects)/float64(reference.inspects))
	// Not a trivial pass by always inspecting. With an event on 15% of the containers
	// per cycle and about three cycles of inspects per event, about 35% is expected;
	// the quiet-host unit tests pin the real saving (0 inspects).
	if underTest.inspects*2 >= reference.inspects {
		t.Errorf("with memory %d inspect calls, without %d: want fewer than half", underTest.inspects, reference.inspects)
	}
}
