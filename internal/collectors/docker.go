package collectors

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	ctypes "github.com/moby/moby/api/types/container"
	client "github.com/moby/moby/client"
)

// exitedRetention bounds how long exited/dead containers stay in the snapshot.
const exitedRetention = 24 * time.Hour

// maxContainers caps the container list emitted in one snapshot; a host with a very
// large (or actively hostile) fleet must not produce an unbounded, ever-repeating
// OTLP payload. When the list exceeds the cap, the newest containers are kept.
const maxContainers = 200

// maxFieldRunes bounds Name/Image/ServiceName/ProjectName — all sourced from the
// daemon (container names, image refs, compose labels) with no length guarantee.
const maxFieldRunes = 256

// DockerStatus is the outcome of a single Docker collection attempt.
type DockerStatus int

const (
	DockerAbsent       DockerStatus = iota // socket missing or daemon unreachable
	DockerNoPermission                     // socket exists but not accessible (EACCES)
	DockerPresent                          // daemon reachable; containers (possibly zero) collected
)

// DockerResult is the outcome of DockerCollector.Collect.
type DockerResult struct {
	Status     DockerStatus
	Containers []ContainerStats
}

// dockerAPI is the subset of the moby client we use; lets tests inject a fake.
type dockerAPI interface {
	ContainerList(ctx context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStats(ctx context.Context, id string, opts client.ContainerStatsOptions) (client.ContainerStatsResult, error)
	Close() error
}

// DockerCollector gathers container stats via the Docker Engine API. It owns a lazily
// created client and tears it down whenever the daemon becomes unreachable, so it is safe
// to call every cycle regardless of whether Docker is installed.
type DockerCollector struct {
	endpoint string
	excludes []*regexp.Regexp
	newAPI   func(endpoint string) (dockerAPI, error) // overridable in tests
	api      dockerAPI
	prevCPU  map[string]cpuSample
	prevNet  map[string]netSample
	logger   *slog.Logger // may be nil (tests); Debug/Warn logs are then no-ops

	memo  map[string]inspectMemo // what the last inspect said, per container ID; rebuilt every cycle
	clock func() instant         // systemInstant; overridable in tests

	warnedTruncated bool // WARN once per process lifetime, not once per cycle
}

// NewDockerCollector returns a collector for the given socket endpoint and image excludes.
func NewDockerCollector(endpoint string, excludedImages []string, logger *slog.Logger) *DockerCollector {
	return &DockerCollector{
		endpoint: endpoint,
		excludes: compileImageGlobs(excludedImages),
		newAPI:   defaultNewAPI,
		prevCPU:  map[string]cpuSample{},
		prevNet:  map[string]netSample{},
		logger:   logger,
		memo:     map[string]inspectMemo{},
		clock:    systemInstant,
	}
}

func (d *DockerCollector) debug(msg string, args ...any) {
	if d.logger != nil {
		d.logger.Debug(msg, args...)
	}
}

// compileImageGlobs turns wildcard patterns into anchored regexps. "*" matches any run of
// characters (including "/", unlike path.Match — operators expect "*nginx*" to match
// ghcr.io/org/nginx:tag), "?" matches one, everything else is literal. QuoteMeta guarantees
// every pattern compiles, so there is no invalid-pattern case to validate.
func compileImageGlobs(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, regexp.MustCompile(imageGlobToRegexp(p)))
	}
	return out
}

func imageGlobToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return b.String()
}

func defaultNewAPI(endpoint string) (dockerAPI, error) {
	c, err := client.New(client.WithHost(endpoint))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Close releases the underlying client, if any.
func (d *DockerCollector) Close() error { return d.closeAPI() }

func (d *DockerCollector) closeAPI() error {
	if d.api == nil {
		return nil
	}
	err := d.api.Close()
	d.api = nil
	return err
}

// Collect performs one detect-and-gather cycle. It never returns an error: an absent or
// unreachable daemon yields Status=DockerAbsent (or DockerNoPermission) and no snapshot.
func (d *DockerCollector) Collect(ctx context.Context) DockerResult {
	if d.api == nil {
		api, err := d.newAPI(d.endpoint)
		if err != nil {
			return DockerResult{Status: d.classify(err)}
		}
		d.api = api
	}

	// The cycle's start is read before the list call: anything the list does not show
	// yet happened after it (spec 4.1, "checked").
	start := d.clock()

	// ContainerList doubles as the presence probe. All=true so recently exited
	// containers stay visible in the snapshot.
	list, err := d.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		d.closeAPI() // recreate (and re-negotiate API version) next cycle
		return DockerResult{Status: d.classify(err)}
	}

	// Apply the image-exclude filter before the newest-first sort and the cap: an
	// excluded image must not consume room in the 200-container budget, and the
	// truncation WARN's "total" must count real candidates, not excluded ones.
	items := list.Items
	if len(d.excludes) > 0 {
		candidates := make([]ctypes.Summary, 0, len(items))
		for i := range items {
			if !d.excluded(items[i].Image) {
				candidates = append(candidates, items[i])
			}
		}
		items = candidates
	}

	if len(items) > maxContainers {
		// Newest first: sort by Created descending, then keep only the cap. An
		// operator watching a live host cares most about what's running now.
		slices.SortFunc(items, func(a, b ctypes.Summary) int { return cmp.Compare(b.Created, a.Created) })
		total := len(items)
		items = items[:maxContainers]
		if !d.warnedTruncated {
			d.warnedTruncated = true
			if d.logger != nil {
				d.logger.Warn("docker: container list truncated", "limit", maxContainers, "total", total)
			}
		}
	}

	cy := &dockerCycle{
		start:    start,
		now:      d.clock(),
		statsNow: time.Now(),
		prevCPU:  make(map[string]cpuSample, len(items)),
		prevNet:  make(map[string]netSample, len(items)),
		memo:     make(map[string]inspectMemo, len(items)),
	}
	out := make([]ContainerStats, 0, len(items)) // non-nil: zero containers must marshal as []
	for i := range items {
		c := &items[i]
		cs, ok := d.snapshotFor(ctx, c, cy)
		if !ok {
			continue
		}
		truncateFields(&cs)
		out = append(out, cs)
	}
	d.prevCPU = cy.prevCPU
	d.prevNet = cy.prevNet
	d.memo = cy.memo
	d.debug("docker: cycle", "containers", len(items), "inspected", cy.inspected)
	return DockerResult{Status: DockerPresent, Containers: out}
}

// dockerCycle is one Collect call's clocks and the state it builds for the next cycle.
type dockerCycle struct {
	start    instant   // read before the list call
	now      instant   // read after it; its Wall bounds exited-container retention
	statsNow time.Time // the stats path's own reading, for CPU and network deltas

	prevCPU   map[string]cpuSample
	prevNet   map[string]netSample
	memo      map[string]inspectMemo
	inspected int // inspect calls made
}

// truncateFields bounds the daemon-sourced string fields that carry no length or
// content guarantee (container name, image ref, compose service/project labels):
// control characters are stripped first (s12 — the agent must not be a silent
// pass-through for attacker-chosen control sequences into the backend/dashboard),
// then the result is truncated to maxFieldRunes.
func truncateFields(cs *ContainerStats) {
	cs.Name = truncateRunes(stripControlChars(cs.Name), maxFieldRunes)
	cs.Image = truncateRunes(stripControlChars(cs.Image), maxFieldRunes)
	cs.ServiceName = truncateRunes(stripControlChars(cs.ServiceName), maxFieldRunes)
	cs.ProjectName = truncateRunes(stripControlChars(cs.ProjectName), maxFieldRunes)
}

// stripControlChars removes C0 control characters (0x00-0x1F, including ESC 0x1B —
// the start of ANSI escape sequences) and DEL (0x7F) from a daemon-sourced string.
// json.Marshal already guarantees valid UTF-8/JSON escaping, so this is not about
// injection into the agent's own output; it is about not passing an attacker's raw
// control sequences through to whatever renders this field downstream (backend
// storage, dashboard).
func stripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r <= 0x1F || r == 0x7F {
			return -1
		}
		return r
	}, s)
}

// truncateRunes truncates s to at most limit runes (not bytes, so multi-byte UTF-8
// isn't split mid-character).
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}

// snapshotFor builds one container's snapshot; ok=false when the container is out of
// scope (created/removing, or exited past retention / with unreadable inspect). Running
// containers survive stats/inspect errors — they just lose the affected fields.
// The four inspect fields come from memory when the list row proves them current
// (canReuse), else from an inspect call.
func (d *DockerCollector) snapshotFor(ctx context.Context, c *ctypes.Summary, cy *dockerCycle) (ContainerStats, bool) {

	state := string(c.State)
	switch state {
	case "created", "removing":
		return ContainerStats{}, false
	}

	cs := ContainerStats{
		ID:          c.ID,
		Name:        containerName(c.Names),
		Image:       c.Image,
		State:       state,
		ServiceName: c.Labels["com.docker.compose.service"],
		ProjectName: c.Labels["com.docker.compose.project"],
	}
	if c.Health != nil && c.Health.Status != ctypes.NoHealthcheck {
		cs.Health = string(c.Health.Status)
	}

	gone := state == "exited" || state == "dead"
	var prev *inspectMemo
	if m, ok := d.memo[c.ID]; ok {
		prev = &m
	}
	if canReuse(state, c.Status, prev, cy.now) {
		m := *prev
		m.Checked = cy.start
		cy.memo[c.ID] = m // remembered also when retention leaves it out below
		if !applyInspectMemo(&cs, m, gone, cy.now.Wall) {
			return ContainerStats{}, false
		}
	} else {
		cy.inspected++
		insp, err := d.api.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		switch {
		case err != nil && gone:
			// Exited containers need FinishedAt to bound retention; without it, skip.
			d.debug("docker: inspect failed for exited/dead container", "id", c.ID, "err", err)
			return ContainerStats{}, false
		case err != nil:
			d.debug("docker: inspect failed", "id", c.ID, "err", err)
		case insp.Container.State == nil:
			cs.RestartCount = insp.Container.RestartCount
			if gone {
				return ContainerStats{}, false
			}
		default:
			st := insp.Container.State
			m := inspectMemo{
				RestartCount: insp.Container.RestartCount,
				StartedAt:    dockerTime(st.StartedAt),
				FinishedAt:   dockerTime(st.FinishedAt),
				ExitCode:     st.ExitCode,
				State:        state,
				Checked:      cy.start,
				Inspected:    cy.start,
			}
			cy.memo[c.ID] = m // remembered also when retention leaves it out below
			if !applyInspectMemo(&cs, m, gone, cy.now.Wall) {
				return ContainerStats{}, false
			}
		}
	}

	if state == "running" {
		if res, err := d.api.ContainerStats(ctx, c.ID, client.ContainerStatsOptions{}); err == nil {
			func() {
				defer res.Body.Close()
				var raw ctypes.StatsResponse
				if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
					return
				}
				curCPU, curNet := applyRunningStats(&cs, &raw, d.prevCPU[c.ID], d.prevNet[c.ID], cy.statsNow)
				cy.prevCPU[c.ID] = curCPU
				cy.prevNet[c.ID] = curNet
			}()
		} else {
			d.debug("docker: stats failed", "id", c.ID, "err", err)
		}
	}
	return cs, true
}

// applyInspectMemo fills the four inspect fields from m: the one path for both a reuse
// and a fresh inspect, so the two cannot drift apart. ok=false when an exited or dead
// container is out of scope (no finish time, or past exitedRetention at now).
func applyInspectMemo(cs *ContainerStats, m inspectMemo, gone bool, now time.Time) bool {
	cs.RestartCount = m.RestartCount
	cs.StartedAtUnixMs = unixMsOrZero(m.StartedAt)
	if gone {
		finished := unixMsOrZero(m.FinishedAt)
		if finished == 0 || now.Sub(time.UnixMilli(finished)) > exitedRetention {
			return false
		}
		cs.FinishedAtUnixMs = finished
		code := m.ExitCode
		cs.ExitCode = &code
	}
	return true
}

func (d *DockerCollector) excluded(image string) bool {
	for _, re := range d.excludes {
		if re.MatchString(image) {
			return true
		}
	}
	return false
}

// classify turns a client error into a status, distinguishing "socket exists but we lack
// permission" (actionable) from "daemon simply not there".
func (d *DockerCollector) classify(err error) DockerStatus {
	if d.socketExists() && isPermission(err) {
		return DockerNoPermission
	}
	return DockerAbsent
}

func (d *DockerCollector) socketExists() bool {
	p := strings.TrimPrefix(d.endpoint, "unix://")
	if p == d.endpoint {
		return false // non-unix endpoint (tcp://…): can't stat
	}
	_, err := os.Stat(p)
	return err == nil
}

func isPermission(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}
