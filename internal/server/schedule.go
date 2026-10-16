package server

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/config"
	"github.com/robfig/cron/v3"
)

// minimumGap is how recently a successful run must have finished for the
// scheduler to skip the next one. Analysing twice within the hour tells you
// nothing new, because the statistics have barely moved.
const minimumGap = time.Hour

// maxJitter spreads scheduled runs so that ten databases all set to daily do
// not all fire at three in the morning and become the load spike the whole
// design exists to avoid.
const maxJitter = 5 * time.Minute

// ScheduleView describes a schedule for the interface.
type ScheduleView struct {
	Mode     config.ScheduleMode `json:"mode"`
	Interval string              `json:"interval,omitempty"`
	Cron     string              `json:"cron,omitempty"`
	Timezone string              `json:"timezone,omitempty"`
	// NextRuns are the next few firing times, so the interface can show what
	// was actually configured. Not knowing what a cron expression means is
	// the single most common way these settings go wrong.
	NextRuns []time.Time `json:"next_runs,omitempty"`
	// Describe is the schedule in plain words, such as
	// "every day at 03:00 Asia/Kolkata".
	Describe string `json:"describe"`
	// Error explains an unusable expression.
	Error string `json:"error,omitempty"`
	// LastSkip records why the most recent due run did not happen.
	LastSkip string `json:"last_skip,omitempty"`
}

// ClusterSchedule pairs a database with its schedule for the overview.
type ClusterSchedule struct {
	ClusterID string        `json:"cluster_id"`
	Name      string        `json:"name"`
	Schedule  ScheduleView  `json:"schedule"`
	NextRun   *time.Time    `json:"next_run,omitempty"`
	LastRun   *time.Time    `json:"last_run,omitempty"`
	LastState RunStatus     `json:"last_state,omitempty"`
	Duration  time.Duration `json:"-"`
}

// scheduler fires analyses on their own.
type scheduler struct {
	s *Server

	mu       sync.Mutex
	skips    map[string]string
	reloadCh chan struct{}
	// running ensures only one scheduled analysis happens at a time across
	// every database, which is the other half of not becoming a load spike.
	running sync.Mutex
}

func newScheduler(s *Server) *scheduler {
	return &scheduler{s: s, skips: map[string]string{}, reloadCh: make(chan struct{}, 1)}
}

// reload asks the loop to recompute after a schedule change, so an edit takes
// effect without a restart.
func (sc *scheduler) reload() {
	select {
	case sc.reloadCh <- struct{}{}:
	default:
	}
}

// run is the scheduler loop.
func (sc *scheduler) run(ctx context.Context) {
	// Deliberately no catch-up: if idxa was not running when an occurrence
	// was due, firing every missed run at startup would hammer the server
	// exactly when it is least expected.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sc.reloadCh:
		case <-ticker.C:
		}
		sc.check(ctx)
	}
}

// check fires any database whose next occurrence has passed.
func (sc *scheduler) check(ctx context.Context) {
	now := time.Now()
	for _, c := range sc.s.opts.Store.Clusters() {
		if c.Schedule.Mode == "" || c.Schedule.Mode == config.ScheduleManual {
			continue
		}

		next, err := sc.nextAfter(c, sc.lastRunTime(c.ID))
		if err != nil || next.IsZero() || next.After(now) {
			continue
		}
		if reason, skip := sc.shouldSkip(c); skip {
			sc.noteSkip(c.ID, reason)
			continue
		}
		sc.fire(ctx, c)
	}
}

// shouldSkip decides whether a due run should be dropped, and says why.
//
// Skipping rather than queueing is deliberate: a queued backlog of analyses
// would arrive together and undo the load budget.
func (sc *scheduler) shouldSkip(c *config.Cluster) (string, bool) {
	sc.s.runs.mu.RLock()
	for _, lr := range sc.s.runs.live {
		if lr.run.ClusterID == c.ID && !lr.done {
			sc.s.runs.mu.RUnlock()
			return "an analysis was already running", true
		}
	}
	sc.s.runs.mu.RUnlock()

	if last, ok := sc.s.runs.latest(c.ID); ok {
		if time.Since(last.EndedAt) < minimumGap {
			return fmt.Sprintf("the previous analysis finished %s ago, which is less than the one hour minimum",
				humanDuration(time.Since(last.EndedAt))), true
		}
	}

	if status, ok := sc.s.probes.get(c.ID); ok {
		for _, n := range status.Nodes {
			if n.Reachable && n.StatsKnown && n.StatsAgeSec < 600 {
				return "statistics were reset less than ten minutes ago, so results would not be meaningful", true
			}
		}
	}
	return "", false
}

func (sc *scheduler) noteSkip(clusterID, reason string) {
	sc.mu.Lock()
	sc.skips[clusterID] = fmt.Sprintf("%s (%s)", reason, time.Now().Format(time.RFC3339))
	sc.mu.Unlock()
}

// fire starts a scheduled analysis, one at a time across all databases.
func (sc *scheduler) fire(ctx context.Context, c *config.Cluster) {
	go func() {
		sc.running.Lock()
		defer sc.running.Unlock()

		if ctx.Err() != nil {
			return
		}
		sc.s.logf("scheduled analysis starting for %s", c.Name)
		if _, started := sc.s.startAnalysis(c, TriggerScheduled); !started {
			sc.noteSkip(c.ID, "an analysis was already running")
			return
		}

		// Hold the global lock until this run finishes so two scheduled
		// analyses never overlap.
		deadline := time.After(30 * time.Minute)
		poll := time.NewTicker(2 * time.Second)
		defer poll.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline:
				return
			case <-poll.C:
				busy := false
				sc.s.runs.mu.RLock()
				for _, lr := range sc.s.runs.live {
					if lr.run.ClusterID == c.ID && !lr.done {
						busy = true
					}
				}
				sc.s.runs.mu.RUnlock()
				if !busy {
					return
				}
			}
		}
	}()
}

// lastRunTime is when this database last completed an analysis.
func (sc *scheduler) lastRunTime(clusterID string) time.Time {
	if last, ok := sc.s.runs.latest(clusterID); ok {
		return last.EndedAt
	}
	return time.Time{}
}

// jitterFor derives a stable per-database offset from its identifier, so the
// spread is consistent across restarts rather than moving every time.
func jitterFor(clusterID string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(clusterID))
	return time.Duration(h.Sum32()%uint32(maxJitter/time.Second)) * time.Second
}

// nextAfter computes the next firing time after a reference point.
func (sc *scheduler) nextAfter(c *config.Cluster, after time.Time) (time.Time, error) {
	if after.IsZero() {
		after = time.Now().Add(-time.Minute)
	}
	jitter := jitterFor(c.ID)

	switch c.Schedule.Mode {
	case config.ScheduleInterval:
		d := c.Schedule.Interval.Duration()
		if d <= 0 {
			return time.Time{}, fmt.Errorf("the interval is not a valid duration")
		}
		return after.Add(d).Add(jitter), nil

	case config.ScheduleCron:
		loc := time.Local
		if c.Schedule.Timezone != "" {
			l, err := time.LoadLocation(c.Schedule.Timezone)
			if err != nil {
				return time.Time{}, fmt.Errorf("unknown timezone %q", c.Schedule.Timezone)
			}
			loc = l
		}
		sched, err := cron.ParseStandard(c.Schedule.Cron)
		if err != nil {
			return time.Time{}, fmt.Errorf("that is not a valid cron expression: %w", err)
		}
		return sched.Next(after.In(loc)).Add(jitter), nil
	}
	return time.Time{}, nil
}

// nextRuns returns the next n firing times.
func (sc *scheduler) nextRuns(c *config.Cluster, n int) ([]time.Time, error) {
	if c.Schedule.Mode == "" || c.Schedule.Mode == config.ScheduleManual {
		return nil, nil
	}
	var out []time.Time
	cursor := time.Now()
	for i := 0; i < n; i++ {
		next, err := sc.nextAfter(c, cursor)
		if err != nil {
			return nil, err
		}
		if next.IsZero() {
			break
		}
		out = append(out, next)
		cursor = next
	}
	return out, nil
}

// describe renders a schedule for the interface.
func (sc *scheduler) describe(c *config.Cluster) ScheduleView {
	v := ScheduleView{
		Mode:     c.Schedule.Mode,
		Interval: string(c.Schedule.Interval),
		Cron:     c.Schedule.Cron,
		Timezone: c.Schedule.Timezone,
	}
	if v.Mode == "" {
		v.Mode = config.ScheduleManual
	}

	sc.mu.Lock()
	v.LastSkip = sc.skips[c.ID]
	sc.mu.Unlock()

	switch v.Mode {
	case config.ScheduleManual:
		v.Describe = "only when you ask"
	case config.ScheduleInterval:
		v.Describe = "every " + humanDuration(c.Schedule.Interval.Duration())
	case config.ScheduleCron:
		zone := c.Schedule.Timezone
		if zone == "" {
			zone = time.Local.String()
		}
		v.Describe = fmt.Sprintf("on the schedule %s, %s time", c.Schedule.Cron, zone)
	}

	next, err := sc.nextRuns(c, 3)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.NextRuns = next
	return v
}

// summaries lists every database's schedule for the overview.
func (sc *scheduler) summaries() []ClusterSchedule {
	var out []ClusterSchedule
	for _, c := range sc.s.opts.Store.Clusters() {
		item := ClusterSchedule{ClusterID: c.ID, Name: c.Name, Schedule: sc.describe(c)}
		if len(item.Schedule.NextRuns) > 0 {
			next := item.Schedule.NextRuns[0]
			item.NextRun = &next
		}
		if last, ok := sc.s.runs.latest(c.ID); ok {
			ended := last.EndedAt
			item.LastRun = &ended
			item.LastState = last.Status
		}
		out = append(out, item)
	}
	return out
}
