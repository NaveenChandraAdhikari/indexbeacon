//go:build integration

package load

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// liveSQL is the cheap tier: statistics counters plus the planner's own page
// count standing in for on-disk size. No pg_relation_size anywhere.
const liveSQL = `
SELECT n.nspname, c.relname, c.relpages::bigint * 8192,
       coalesce(s.seq_scan, 0), coalesce(s.seq_tup_read, 0), coalesce(s.idx_scan, 0)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r','p','m')
  AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')`

// costlySQL fetches the same sizes through the on-disk size functions. It
// exists to prove the governor throttles an expensive collector on its own.
const costlySQL = `
SELECT n.nspname, c.relname, pg_table_size(c.oid), pg_indexes_size(c.oid)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','m')
  AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')`

// dsn returns the test database DSN, skipping when none is configured.
func dsn(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("IDXA_TEST_DSN"); v != "" {
		return v
	}
	t.Skip("set IDXA_TEST_DSN to run live governor tests")
	return ""
}

// TestGovernorHoldsBudgetAgainstRealDatabase runs a realtime polling loop
// against a real server and asserts that the measured share of wall-clock
// time spent inside idxa statements stays within the configured budget, even
// though the loop asks to poll far more often than the budget allows.
func TestGovernorHoldsBudgetAgainstRealDatabase(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET default_transaction_read_only = on"); err != nil {
		t.Fatalf("read-only: %v", err)
	}

	cases := []struct {
		name string
		sql  string
	}{
		{"live tier using relpages", liveSQL},
		{"cold tier using size functions", costlySQL},
	}

	const (
		budget = 0.01
		window = 12 * time.Second
		tick   = 250 * time.Millisecond
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGovernor(Options{DutyCycle: budget, MinInterval: 100 * time.Millisecond})

			deadline := time.Now().Add(window)
			ticker := time.NewTicker(tick)
			defer ticker.Stop()

			var wanted, ran, skipped int
			for time.Now().Before(deadline) {
				<-ticker.C
				wanted++

				release, err := g.TryAcquire(ctx, "node1")
				if errors.Is(err, ErrBusy) {
					skipped++
					continue
				}
				if err != nil {
					t.Fatalf("acquire: %v", err)
				}

				qctx, cancel := context.WithTimeout(ctx, TierLive.StatementTimeout())
				rows, err := conn.Query(qctx, tc.sql)
				if err == nil {
					for rows.Next() {
					}
					rows.Close()
					err = rows.Err()
				}
				cancel()
				release()
				if err != nil {
					t.Fatalf("query: %v", err)
				}
				ran++
			}

			st := g.Stats("node1")
			statements := st.Statements
			if statements < 1 {
				statements = 1
			}
			avgMS := float64(st.TotalTime.Milliseconds()) / float64(statements)
			measured := st.DutyCycle()

			t.Logf("loop wanted %d polls at %v; governor allowed %d, skipped %d",
				wanted, tick, ran, skipped)
			t.Logf("per statement avg %.1f ms, slowest %.1f ms",
				avgMS, float64(st.SlowestTime.Microseconds())/1000)
			t.Logf("total DB time %d ms over %v; measured duty cycle %.3f%% (budget %.3f%%)",
				st.TotalTime.Milliseconds(), window, measured*100, budget*100)
			t.Logf("current cooldown %v", st.Cooldown.Round(time.Millisecond))

			// The whole promise of the governor: we stay inside the budget.
			// A small tolerance covers the first statement, which necessarily
			// runs before any cooldown has been computed.
			if measured > budget*2 {
				t.Errorf("measured duty cycle %.3f%% exceeds budget %.3f%% by more than 2x",
					measured*100, budget*100)
			}
			if skipped == 0 {
				t.Error("expected the governor to skip ticks; it never throttled")
			}
			if ran == 0 {
				t.Error("expected at least one statement to run")
			}
		})
	}
}

// TestExpensiveCollectorIsThrottledHarder confirms the self-tuning property:
// the same budget produces a much longer cooldown for the costly query than
// for the cheap one, with no per-query configuration.
func TestExpensiveCollectorIsThrottledHarder(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	measure := func(sql string) time.Duration {
		g := NewGovernor(Options{DutyCycle: 0.01, MinInterval: time.Millisecond})
		release, err := g.TryAcquire(ctx, "n")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		rows, err := conn.Query(ctx, sql)
		if err == nil {
			for rows.Next() {
			}
			rows.Close()
		}
		release()
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		return g.Stats("n").Cooldown
	}

	cheap := measure(liveSQL)
	costly := measure(costlySQL)
	t.Logf("cooldown after cheap query: %v; after costly query: %v", cheap, costly)

	if costly <= cheap {
		t.Errorf("costly query cooldown %v should exceed cheap query cooldown %v", costly, cheap)
	}
}
