package collectors

import (
	"fmt"
	"strings"
	"time"

	units "github.com/docker/go-units"
)

// Spec: docs/superpowers/specs/2026-11-24-lean-agent-part3-design.md, section 4.
const (
	inspectRefresh = time.Hour        // spec 4.4: every container is inspected at least once an hour
	inspectSlack   = 60 * time.Second // spec 4.2, "why the slack": Docker can take 32 s to make a stop visible
)

// processStart is the origin of instant.Mono readings.
var processStart = time.Now()

// instant is one reading of both clocks.
type instant struct {
	Wall time.Time     // wall clock, monotonic reading stripped (t.Round(0))
	Mono time.Duration // monotonic clock, from a process-wide origin
}

// systemInstant reads both clocks now.
func systemInstant() instant {
	t := time.Now()
	return instant{Wall: t.Round(0), Mono: t.Sub(processStart)}
}

// since returns the time from then to i: the larger of the two clocks' differences.
// The monotonic difference survives a wall-clock step; the wall difference counts
// time spent suspended.
func (i instant) since(then instant) time.Duration {
	return max(i.Mono-then.Mono, i.Wall.Sub(then.Wall))
}

// inspectMemo is what the last successful inspect said about one container.
type inspectMemo struct {
	RestartCount int
	StartedAt    time.Time // zero when Docker gave none
	FinishedAt   time.Time // zero when Docker gave none
	ExitCode     int
	State        string  // the list's state in the cycle that last confirmed the entry
	Checked      instant // start of that cycle, read before its list call
	Inspected    instant // start of the cycle that last inspected
}

// canReuse reports whether m still describes the container whose list row has this
// state and status text (spec 4.2, rules 1 to 7). m is nil when nothing is remembered.
// Any doubt answers false: inspecting is always correct, only more expensive.
func canReuse(state, status string, m *inspectMemo, now instant) bool {
	if m == nil || state != m.State { // rules 2 and 3
		return false
	}
	var ref time.Time
	switch state { // rule 1
	case "running", "paused":
		ref = m.StartedAt
		want := "Up " + units.HumanDuration(now.Wall.Sub(ref))
		if status != want && !strings.HasPrefix(status, want+" (") { // rule 6
			return false
		}
	case "exited":
		ref = m.FinishedAt
		if m.RestartCount > 0 && now.Wall.Sub(ref) <= exitedRetention { // rule 5
			return false
		}
		if status != fmt.Sprintf("Exited (%d) %s ago", m.ExitCode, units.HumanDuration(now.Wall.Sub(ref))) { // rule 6
			return false
		}
	default:
		return false
	}
	if now.since(m.Inspected) >= inspectRefresh { // rule 4
		return false
	}
	// Rule 7: anything unseen happened after Checked minus the slack, so Docker's text
	// for it reads as HumanDuration of something shorter than horizon. A remembered
	// time that reads as a later wording than horizon cannot be confused with it.
	age, horizon := now.Wall.Sub(ref), now.since(m.Checked)+inspectSlack
	return age > horizon && units.HumanDuration(age) != units.HumanDuration(horizon)
}
