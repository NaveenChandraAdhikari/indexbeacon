package load

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock lets the tests advance time without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestGovernor(opts Options) (*Governor, *fakeClock) {
	clk := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	opts.now = clk.Now
	return NewGovernor(opts), clk
}

func TestCooldownEnforcesDutyCycle(t *testing.T) {
	// At a 1% duty cycle an 80ms statement must be followed by roughly
	// 8 seconds of silence, which is the whole point of the governor.
	g, clk := newTestGovernor(Options{DutyCycle: 0.01, MinInterval: time.Millisecond})
	ctx := context.Background()

	release, err := g.TryAcquire(ctx, "n1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	clk.Advance(80 * time.Millisecond)
	release()

	st := g.Stats("n1")
	want := 80*time.Millisecond/time.Duration(1)*100 - 80*time.Millisecond // 8s - 80ms
	if st.Cooldown != want {
		t.Errorf("cooldown = %v, want %v", st.Cooldown, want)
	}
	if st.LastTime != 80*time.Millisecond {
		t.Errorf("last time = %v, want 80ms", st.LastTime)
	}

	// Still cooling: the next tick must be skipped, not queued.
	if _, err := g.TryAcquire(ctx, "n1"); !errors.Is(err, ErrBusy) {
		t.Errorf("acquire during cooldown = %v, want ErrBusy", err)
	}
	if got := g.Stats("n1").Skipped; got != 1 {
		t.Errorf("skipped = %d, want 1", got)
	}

	// Once the cooldown elapses it may run again.
	clk.Advance(want)
	if _, err := g.TryAcquire(ctx, "n1"); err != nil {
		t.Errorf("acquire after cooldown: %v", err)
	}
}

func TestCooldownClampedToBounds(t *testing.T) {
	g, clk := newTestGovernor(Options{
		DutyCycle:   0.01,
		MinInterval: 2 * time.Second,
		MaxInterval: 30 * time.Second,
	})
	ctx := context.Background()

	// A very fast statement would imply a sub-second cooldown; the floor wins.
	release, _ := g.TryAcquire(ctx, "fast")
	clk.Advance(1 * time.Millisecond)
	release()
	if got := g.Stats("fast").Cooldown; got != 2*time.Second {
		t.Errorf("fast cooldown = %v, want the 2s floor", got)
	}

	// A very slow statement would imply a 100s cooldown; the ceiling wins.
	release, _ = g.TryAcquire(ctx, "slow")
	clk.Advance(1 * time.Second)
	release()
	if got := g.Stats("slow").Cooldown; got != 30*time.Second {
		t.Errorf("slow cooldown = %v, want the 30s ceiling", got)
	}
}

func TestSlowNodeIsPolledLessOften(t *testing.T) {
	// The same duty cycle applied to a fast and a slow server should leave
	// the slow one with a much longer gap, with no per-server tuning.
	g, clk := newTestGovernor(Options{DutyCycle: 0.01, MinInterval: time.Millisecond})
	ctx := context.Background()

	rf, _ := g.TryAcquire(ctx, "fast")
	clk.Advance(5 * time.Millisecond)
	rf()

	rs, _ := g.TryAcquire(ctx, "slow")
	clk.Advance(500 * time.Millisecond)
	rs()

	fast := g.Stats("fast").Cooldown
	slow := g.Stats("slow").Cooldown
	if slow <= fast*10 {
		t.Errorf("slow cooldown %v should be far longer than fast %v", slow, fast)
	}
}

func TestNoOverlapOnOneNode(t *testing.T) {
	g, _ := newTestGovernor(Options{DutyCycle: 1, MinInterval: time.Nanosecond})
	ctx := context.Background()

	release, err := g.TryAcquire(ctx, "n1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// A second statement against the same node must be refused while the
	// first is in flight, so a slow tick cannot pile up behind itself.
	if _, err := g.TryAcquire(ctx, "n1"); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent acquire = %v, want ErrBusy", err)
	}
	release()
}

func TestGlobalConcurrencyLimit(t *testing.T) {
	// Adding databases must not multiply load without bound.
	g, _ := newTestGovernor(Options{DutyCycle: 1, MaxConcurrent: 2, MinInterval: time.Nanosecond})
	ctx := context.Background()

	r1, err := g.TryAcquire(ctx, "n1")
	if err != nil {
		t.Fatalf("n1: %v", err)
	}
	r2, err := g.TryAcquire(ctx, "n2")
	if err != nil {
		t.Fatalf("n2: %v", err)
	}

	// The third node must block until a slot frees up.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := g.TryAcquire(cctx, "n3"); !errors.Is(err, context.Canceled) {
		t.Errorf("third acquire = %v, want context.Canceled", err)
	}

	// n3 was rolled back cleanly, so it can run once a slot is free.
	r1()
	r2()
	if _, err := g.TryAcquire(ctx, "n3"); err != nil {
		t.Errorf("n3 after release: %v", err)
	}
}

func TestAcquireWaitsInsteadOfSkipping(t *testing.T) {
	// User-initiated work should wait for the cooldown rather than be
	// dropped the way a background tick is.
	g, _ := newTestGovernor(Options{DutyCycle: 0.5, MinInterval: 20 * time.Millisecond})
	g.opts.now = time.Now // real clock: we want the wait to actually elapse
	ctx := context.Background()

	release, err := g.Acquire(ctx, "n1")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	release()

	start := time.Now()
	release, err = g.Acquire(ctx, "n1")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	release()
	if waited := time.Since(start); waited < 10*time.Millisecond {
		t.Errorf("Acquire returned after %v, expected it to wait out the cooldown", waited)
	}
	if got := g.Stats("n1").Skipped; got != 0 {
		t.Errorf("skipped = %d, want 0: Acquire waits, it does not skip", got)
	}
}

func TestAcquireRespectsCancellation(t *testing.T) {
	g, _ := newTestGovernor(Options{DutyCycle: 0.001, MinInterval: time.Second})
	g.opts.now = time.Now
	ctx := context.Background()

	release, _ := g.Acquire(ctx, "n1")
	release()

	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(cctx, "n1"); err == nil {
		t.Error("Acquire during a long cooldown should honour ctx deadline")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	g, clk := newTestGovernor(Options{DutyCycle: 1, MinInterval: time.Nanosecond, MaxConcurrent: 1})
	ctx := context.Background()

	release, _ := g.TryAcquire(ctx, "n1")
	clk.Advance(time.Millisecond)
	release()
	release() // a double release must not free a second global slot

	if got := g.Stats("n1").Statements; got != 1 {
		t.Errorf("statements = %d, want 1", got)
	}
	// The single global slot must still be available.
	if _, err := g.TryAcquire(ctx, "n2"); err != nil {
		t.Errorf("slot leaked after double release: %v", err)
	}
}

func TestStatsAccounting(t *testing.T) {
	g, clk := newTestGovernor(Options{DutyCycle: 1, MinInterval: time.Nanosecond})
	ctx := context.Background()

	for _, d := range []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 20 * time.Millisecond} {
		release, err := g.TryAcquire(ctx, "n1")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		clk.Advance(d)
		release()
		// Step past the minimum interval so the next iteration is allowed.
		clk.Advance(time.Millisecond)
	}

	st := g.Stats("n1")
	if st.Statements != 3 {
		t.Errorf("statements = %d, want 3", st.Statements)
	}
	if st.TotalTime != 80*time.Millisecond {
		t.Errorf("total = %v, want 80ms", st.TotalTime)
	}
	if st.SlowestTime != 50*time.Millisecond {
		t.Errorf("slowest = %v, want 50ms", st.SlowestTime)
	}
	if st.LastTime != 20*time.Millisecond {
		t.Errorf("last = %v, want 20ms", st.LastTime)
	}
}

func TestAllStatsAndReset(t *testing.T) {
	g, clk := newTestGovernor(Options{DutyCycle: 1, MinInterval: time.Nanosecond})
	ctx := context.Background()

	for _, node := range []string{"a", "b"} {
		release, _ := g.TryAcquire(ctx, node)
		clk.Advance(time.Millisecond)
		release()
	}

	all := g.AllStats()
	if len(all) != 2 {
		t.Fatalf("AllStats returned %d nodes, want 2", len(all))
	}

	g.Reset("a")
	if got := g.Stats("a").Statements; got != 0 {
		t.Errorf("after reset statements = %d, want 0", got)
	}
	if got := g.Stats("b").Statements; got != 1 {
		t.Errorf("reset of a must not touch b, got %d", got)
	}
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	g, _ := newTestGovernor(Options{DutyCycle: 1, MaxConcurrent: 4, MinInterval: time.Nanosecond})
	g.opts.now = time.Now
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				release, err := g.TryAcquire(ctx, "shared")
				if err != nil {
					continue
				}
				release()
			}
		}(i)
	}
	wg.Wait()

	if got := g.Stats("shared").Statements; got == 0 {
		t.Error("expected at least one statement to complete")
	}
}

func TestDutyCycleReporting(t *testing.T) {
	g, _ := newTestGovernor(Options{DutyCycle: 1, MinInterval: time.Nanosecond})
	g.opts.now = time.Now
	ctx := context.Background()

	release, _ := g.TryAcquire(ctx, "n1")
	time.Sleep(5 * time.Millisecond)
	release()

	if got := g.Stats("n1").DutyCycle(); got <= 0 || got > 1 {
		t.Errorf("DutyCycle() = %v, want a fraction in (0,1]", got)
	}
	if got := (Stats{}).DutyCycle(); got != 0 {
		t.Errorf("zero Stats DutyCycle() = %v, want 0", got)
	}
}

func TestOptionDefaults(t *testing.T) {
	g := NewGovernor(Options{})
	if g.opts.DutyCycle != DefaultDutyCycle {
		t.Errorf("duty cycle = %v, want default", g.opts.DutyCycle)
	}
	if g.opts.MaxConcurrent != DefaultMaxConcurrent {
		t.Errorf("max concurrent = %d, want default", g.opts.MaxConcurrent)
	}

	// An out-of-range duty cycle is clamped rather than rejected.
	g = NewGovernor(Options{DutyCycle: 5})
	if g.opts.DutyCycle != 1 {
		t.Errorf("duty cycle = %v, want clamp to 1", g.opts.DutyCycle)
	}

	// MaxInterval below MinInterval is corrected instead of inverting.
	g = NewGovernor(Options{MinInterval: time.Minute, MaxInterval: time.Second})
	if g.opts.MaxInterval < g.opts.MinInterval {
		t.Error("MaxInterval must not be below MinInterval")
	}
}

func TestTierIntervalsAndTimeouts(t *testing.T) {
	// The live tier must stay fast enough to feel realtime, and the cold
	// tier must never land inside a polling loop.
	if TierLive.DefaultInterval() >= TierWarm.DefaultInterval() {
		t.Error("live tier must refresh more often than warm")
	}
	if TierWarm.DefaultInterval() >= TierCold.DefaultInterval() {
		t.Error("warm tier must refresh more often than cold")
	}
	if TierLive.StatementTimeout() >= TierCold.StatementTimeout() {
		t.Error("live statements must time out sooner than cold ones")
	}
	if got := TierLive.String(); got != "live" {
		t.Errorf("TierLive.String() = %q", got)
	}
}
