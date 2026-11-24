package collectors

import (
	"testing"
	"time"

	units "github.com/docker/go-units"
)

// TestHumanDurationWording pins the daemon's formatter at every boundary (spec 4.5):
// a go-units update that changes a wording fails here, before the agent stops matching
// the list's status text and silently falls back to inspecting every cycle.
func TestHumanDurationWording(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "Less than a second"},
		{0, "Less than a second"},
		{999 * time.Millisecond, "Less than a second"},
		{time.Second, "1 second"},
		{59 * time.Second, "59 seconds"},
		{60 * time.Second, "About a minute"},
		{119 * time.Second, "About a minute"},
		{120 * time.Second, "2 minutes"},
		{59*time.Minute + 59*time.Second, "59 minutes"},
		{60 * time.Minute, "About an hour"},
		{89 * time.Minute, "About an hour"},
		{90 * time.Minute, "2 hours"},
		{47*time.Hour + 29*time.Minute, "47 hours"},
		{47*time.Hour + 30*time.Minute, "2 days"},
		{335 * time.Hour, "13 days"},
		{336 * time.Hour, "2 weeks"},
		{1439 * time.Hour, "8 weeks"},
		{1440 * time.Hour, "2 months"},
		{17519 * time.Hour, "24 months"},
		{17520 * time.Hour, "2 years"},
	}
	for _, c := range cases {
		if got := units.HumanDuration(c.d); got != c.want {
			t.Errorf("HumanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestHumanDurationNeverGoesBack: rule 7 rests on each wording covering one range of
// durations, in increasing order. Once a wording is left it must never come back.
func TestHumanDurationNeverGoesBack(t *testing.T) {
	left := map[string]bool{}
	current := ""
	visit := func(d time.Duration) {
		w := units.HumanDuration(d)
		if w == current {
			return
		}
		if left[w] {
			t.Fatalf("wording %q reappears at %v after it was left", w, d)
		}
		if current != "" {
			left[current] = true
		}
		current = w
	}
	for d := time.Duration(0); d <= 3*time.Hour; d += time.Second {
		visit(d)
	}
	for d := 3 * time.Hour; d <= 3*365*24*time.Hour; d += time.Hour {
		visit(d)
	}
}

var memoBase = instant{Wall: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), Mono: 1000 * time.Second}

func (i instant) plus(wall, mono time.Duration) instant {
	return instant{Wall: i.Wall.Add(wall), Mono: i.Mono + mono}
}

func TestInstantSince(t *testing.T) {
	cases := []struct {
		name  string
		later instant
		want  time.Duration
	}{
		{"both clocks +60s", memoBase.plus(60*time.Second, 60*time.Second), 60 * time.Second},
		{"suspended: wall +3h, mono +60s", memoBase.plus(3*time.Hour, 60*time.Second), 3 * time.Hour},
		{"clock stepped back: wall +30s, mono +60s", memoBase.plus(30*time.Second, 60*time.Second), 60 * time.Second},
		{"wall -30min, mono +60s", memoBase.plus(-30*time.Minute, 60*time.Second), 60 * time.Second},
	}
	for _, c := range cases {
		if got := c.later.since(memoBase); got != c.want {
			t.Errorf("%s: since = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanReuse(t *testing.T) {
	now := memoBase.plus(60*time.Second, 60*time.Second)
	ago := func(d time.Duration) time.Time { return now.Wall.Add(-d) }
	before := func(d time.Duration) instant { return now.plus(-d, -d) }

	// entry builds the default remembered entry of the table: State = the row's state,
	// Checked = memoBase, Inspected 10 minutes before now, RestartCount 0.
	entry := func(state string, edit func(m *inspectMemo)) *inspectMemo {
		m := &inspectMemo{State: state, Checked: memoBase, Inspected: before(10 * time.Minute)}
		if edit != nil {
			edit(m)
		}
		return m
	}
	started := func(d time.Duration) func(m *inspectMemo) {
		return func(m *inspectMemo) { m.StartedAt = ago(d) }
	}
	finished := func(d time.Duration, code, restarts int) func(m *inspectMemo) {
		return func(m *inspectMemo) { m.FinishedAt, m.ExitCode, m.RestartCount = ago(d), code, restarts }
	}

	const reuse, inspect = true, false
	cases := []struct {
		n      int
		state  string
		status string
		m      *inspectMemo
		want   bool
	}{
		{1, "running", "Up 3 days", entry("running", started(72*time.Hour)), reuse},
		{2, "running", "Up 3 days (healthy)", entry("running", started(72*time.Hour)), reuse},
		{3, "paused", "Up 3 days (Paused)", entry("paused", started(72*time.Hour)), reuse},
		{4, "running", "Up 30 seconds", entry("running", started(72*time.Hour)), inspect},
		{5, "running", "Up 3 days", nil, inspect},
		{6, "restarting", "Restarting (1) 5 seconds ago", entry("restarting", finished(5*time.Second, 1, 0)), inspect},
		{7, "dead", "Dead", entry("dead", nil), inspect},
		{8, "exited", "Exited (0) 3 days ago", entry("running", finished(72*time.Hour, 0, 0)), inspect},
		{9, "running", "Up 3 days", entry("running", func(m *inspectMemo) {
			started(72 * time.Hour)(m)
			m.Inspected = before(time.Hour)
		}), inspect},
		{10, "running", "Up 3 days", entry("running", func(m *inspectMemo) {
			started(72 * time.Hour)(m)
			m.Inspected = before(59*time.Minute + 59*time.Second)
		}), reuse},
		{11, "exited", "Exited (255) 5 months ago", entry("exited", finished(150*24*time.Hour, 255, 0)), reuse},
		{12, "exited", "Exited (0) 5 months ago", entry("exited", finished(150*24*time.Hour, 255, 0)), inspect},
		{13, "exited", "Exited (255) 5 months ago (x)", entry("exited", finished(150*24*time.Hour, 255, 0)), inspect},
		{14, "exited", "Exited (1) 2 hours ago", entry("exited", finished(2*time.Hour, 1, 3)), inspect},
		{15, "exited", "Exited (1) 25 hours ago", entry("exited", finished(25*time.Hour, 1, 3)), reuse},
		{16, "exited", "Exited (1) 2 hours ago", entry("exited", finished(2*time.Hour, 1, 0)), reuse},
		{17, "running", "Up About a minute", entry("running", started(80*time.Second)), inspect},
		{18, "running", "Up 2 minutes", entry("running", started(130*time.Second)), inspect},
		{19, "running", "Up 3 minutes", entry("running", started(180*time.Second)), reuse},
		{20, "running", "Up 2 minutes", entry("running", func(m *inspectMemo) {
			started(130 * time.Second)(m)
			m.Checked = before(59900 * time.Millisecond)
		}), reuse},
		{21, "running", "Up 3 hours", entry("running", func(m *inspectMemo) {
			started(3*time.Hour + 10*time.Minute)(m)
			m.Checked = before(3 * time.Hour)
		}), inspect},
		{22, "running", "Up 3 hours", entry("running", func(m *inspectMemo) {
			started(3*time.Hour + 10*time.Minute)(m)
			m.Checked = now.plus(-3*time.Hour, -60*time.Second)
		}), inspect},
		{23, "running", "Up 3 hours", entry("running", started(3*time.Hour+10*time.Minute)), reuse},
		{24, "running", "Up 30 minutes", entry("running", started(3*time.Minute)), inspect},
		{25, "running", "running since Tuesday", entry("running", started(72*time.Hour)), inspect},
		{26, "running", "", entry("running", started(72*time.Hour)), inspect},
		{27, "running", "Up Less than a second", entry("running", func(m *inspectMemo) {
			m.StartedAt = now.Wall.Add(30 * time.Minute) // remote daemon's clock ahead
		}), inspect},
		{28, "exited", "", entry("exited", nil), inspect},
	}
	for _, c := range cases {
		if got := canReuse(c.state, c.status, c.m, now); got != c.want {
			t.Errorf("row %d (%s, %q): canReuse = %v, want %v", c.n, c.state, c.status, got, c.want)
		}
	}
}
