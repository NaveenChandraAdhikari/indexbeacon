// Package load keeps idxa cheap for the databases it inspects.
//
// idxa is meant to be pointed at production. A monitoring tool that adds
// measurable load to the server it is monitoring will be turned off, so every
// statement idxa issues goes through a Governor that enforces four things:
//
//  1. Duty cycle. Each node gets a budget expressed as a fraction of wall
//     clock time (default 1%). A collection that took 80ms forces an 8s pause
//     before the next one on that node. Slow servers are therefore polled less
//     often, automatically, without any tuning.
//  2. Serialization. At most one statement is in flight per node, so a slow
//     tick can never pile up behind itself.
//  3. Global concurrency. Across all configured nodes, only a few statements
//     run at once, so adding databases to the UI does not multiply load.
//  4. Accounting. Every statement's duration is recorded so the UI can show
//     exactly how much database time idxa has consumed.
//
// The zero value is not usable; call NewGovernor.
package load

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Defaults chosen to be unnoticeable on a busy server.
const (
	// DefaultDutyCycle is the fraction of wall-clock time a single node may
	// spend running idxa statements. 0.01 means a 100ms collection is
	// followed by ~10s of silence.
	DefaultDutyCycle = 0.01

	// DefaultMaxConcurrent caps statements in flight across all nodes.
	DefaultMaxConcurrent = 4

	// DefaultMinInterval is the floor between statements on one node, applied
	// even when the duty cycle would allow a faster loop.
	DefaultMinInterval = 1 * time.Second

	// DefaultMaxInterval caps the backoff a slow node can reach.
	DefaultMaxInterval = 5 * time.Minute
)

// ErrBusy is returned by TryAcquire when the node is still within its
// cooldown, or another statement is already running against it. Callers
// polling on a ticker should treat it as "skip this tick", not as a failure.
var ErrBusy = errors.New("load: node is cooling down or busy")

// Stats is a snapshot of what idxa has cost one node.
type Stats struct {
	// Statements is how many statements completed.
	Statements int64 `json:"statements"`
	// TotalTime is the summed duration of those statements: the database
	// time idxa is responsible for.
	TotalTime time.Duration `json:"total_time"`
	// LastTime is the duration of the most recent statement.
	LastTime time.Duration `json:"last_time"`
	// SlowestTime is the worst statement seen.
	SlowestTime time.Duration `json:"slowest_time"`
	// Cooldown is how long the node must stay idle before the next
	// statement, derived from LastTime and the duty cycle.
	Cooldown time.Duration `json:"cooldown"`
	// NextAllowed is when the next statement may start.
	NextAllowed time.Time `json:"next_allowed"`
	// Skipped counts ticks dropped because the node was busy or cooling.
	Skipped int64 `json:"skipped"`
	// Since is when accounting started.
	Since time.Time `json:"since"`
}

// DutyCycle reports the fraction of wall-clock time spent running idxa
// statements since accounting began. It is the number to show a DBA who asks
// what this tool is costing them.
func (s Stats) DutyCycle() float64 {
	if s.Since.IsZero() {
		return 0
	}
	elapsed := time.Since(s.Since)
	if elapsed <= 0 {
		return 0
	}
	return float64(s.TotalTime) / float64(elapsed)
}

// Options configures a Governor.
type Options struct {
	// DutyCycle is the per-node share of wall-clock time idxa may use.
	// Values <= 0 select DefaultDutyCycle. Values > 1 are clamped to 1.
	DutyCycle float64
	// MaxConcurrent caps in-flight statements across all nodes.
	MaxConcurrent int
	// MinInterval is the floor between statements on one node.
	MinInterval time.Duration
	// MaxInterval caps the computed cooldown.
	MaxInterval time.Duration
	// now is injectable for tests.
	now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.DutyCycle <= 0 {
		o.DutyCycle = DefaultDutyCycle
	}
	if o.DutyCycle > 1 {
		o.DutyCycle = 1
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = DefaultMaxConcurrent
	}
	if o.MinInterval <= 0 {
		o.MinInterval = DefaultMinInterval
	}
	if o.MaxInterval <= 0 {
		o.MaxInterval = DefaultMaxInterval
	}
	if o.MaxInterval < o.MinInterval {
		o.MaxInterval = o.MinInterval
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// Governor throttles statements per node and across nodes. It is safe for
// concurrent use.
type Governor struct {
	opts Options

	sem chan struct{}

	mu    sync.Mutex
	nodes map[string]*nodeState
}

type nodeState struct {
	running     bool
	nextAllowed time.Time
	stats       Stats
}

// NewGovernor returns a Governor applying opts, with defaults filled in.
func NewGovernor(opts Options) *Governor {
	o := opts.withDefaults()
	return &Governor{
		opts:  o,
		sem:   make(chan struct{}, o.MaxConcurrent),
		nodes: make(map[string]*nodeState),
	}
}

// state returns the per-node record, creating it on first use.
// Caller must hold g.mu.
func (g *Governor) state(node string) *nodeState {
	st, ok := g.nodes[node]
	if !ok {
		st = &nodeState{stats: Stats{Since: g.opts.now()}}
		g.nodes[node] = st
	}
	return st
}

// TryAcquire takes the slot for node without blocking on its cooldown. It
// returns ErrBusy if the node is still cooling down or already has a
// statement in flight. Use it for periodic collection, where a skipped tick
// is better than a queued one.
//
// On success the caller must call the returned release function exactly once,
// which records the statement's duration and starts the cooldown.
func (g *Governor) TryAcquire(ctx context.Context, node string) (release func(), err error) {
	g.mu.Lock()
	st := g.state(node)
	now := g.opts.now()
	if st.running || now.Before(st.nextAllowed) {
		st.stats.Skipped++
		g.mu.Unlock()
		return nil, ErrBusy
	}
	st.running = true
	g.mu.Unlock()

	// Global slot. If we cannot get one promptly, give the node back.
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		g.mu.Lock()
		g.state(node).running = false
		g.mu.Unlock()
		return nil, ctx.Err()
	}

	return g.releaser(node), nil
}

// Acquire waits until node is allowed to run another statement, then takes
// its slot. Use it for user-initiated work, where waiting is better than
// skipping. It respects ctx cancellation.
//
// The returned release function must be called exactly once.
func (g *Governor) Acquire(ctx context.Context, node string) (release func(), err error) {
	for {
		g.mu.Lock()
		st := g.state(node)
		now := g.opts.now()
		switch {
		case st.running:
			// Another statement is in flight; poll briefly.
			g.mu.Unlock()
			if err := sleepCtx(ctx, 10*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		case now.Before(st.nextAllowed):
			wait := st.nextAllowed.Sub(now)
			g.mu.Unlock()
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue
		default:
			st.running = true
			g.mu.Unlock()
		}

		select {
		case g.sem <- struct{}{}:
		case <-ctx.Done():
			g.mu.Lock()
			g.state(node).running = false
			g.mu.Unlock()
			return nil, ctx.Err()
		}
		return g.releaser(node), nil
	}
}

// releaser builds the one-shot release closure for an acquired node slot.
func (g *Governor) releaser(node string) func() {
	start := g.opts.now()
	var once sync.Once
	return func() {
		once.Do(func() {
			elapsed := g.opts.now().Sub(start)
			if elapsed < 0 {
				elapsed = 0
			}
			<-g.sem

			g.mu.Lock()
			defer g.mu.Unlock()
			st := g.state(node)
			st.running = false
			st.stats.Statements++
			st.stats.TotalTime += elapsed
			st.stats.LastTime = elapsed
			if elapsed > st.stats.SlowestTime {
				st.stats.SlowestTime = elapsed
			}
			cooldown := g.cooldownFor(elapsed)
			st.stats.Cooldown = cooldown
			st.nextAllowed = g.opts.now().Add(cooldown)
			st.stats.NextAllowed = st.nextAllowed
		})
	}
}

// cooldownFor converts a statement duration into the idle time required to
// stay within the duty cycle, clamped to [MinInterval, MaxInterval].
//
// A statement of duration d at duty cycle c must be followed by d/c - d of
// idleness, so that d / (d + idle) == c.
func (g *Governor) cooldownFor(d time.Duration) time.Duration {
	if d <= 0 {
		return g.opts.MinInterval
	}
	cooldown := time.Duration(float64(d)/g.opts.DutyCycle) - d
	if cooldown < g.opts.MinInterval {
		cooldown = g.opts.MinInterval
	}
	if cooldown > g.opts.MaxInterval {
		cooldown = g.opts.MaxInterval
	}
	return cooldown
}

// Stats returns a snapshot of what idxa has cost one node.
func (g *Governor) Stats(node string) Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state(node).stats
}

// AllStats returns a snapshot for every node seen so far.
func (g *Governor) AllStats() map[string]Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]Stats, len(g.nodes))
	for name, st := range g.nodes {
		out[name] = st.stats
	}
	return out
}

// Reset clears accounting for a node, keeping any active cooldown.
func (g *Governor) Reset(node string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(node)
	st.stats = Stats{Since: g.opts.now(), NextAllowed: st.nextAllowed}
}

// sleepCtx sleeps for d unless ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
