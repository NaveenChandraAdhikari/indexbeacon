package workload

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/db"
	"github.com/indexbeacon/indexbeacon/internal/load"
	"github.com/indexbeacon/indexbeacon/internal/parser"
)

// SampleOptions tunes a recording.
type SampleOptions struct {
	// Interval is how often to look at what is running. One second catches
	// anything that runs for longer than about a second; shorter intervals
	// catch faster queries at proportionally more cost to the server.
	Interval time.Duration
	// Duration is how long to record for. Zero records until cancelled.
	Duration time.Duration
	// KeepLiterals stores one example of each statement with its values
	// intact. Partial index candidates need a constant, but statement text
	// can contain personal data, so this is off unless asked for.
	KeepLiterals bool
	// Governor bounds what the sampler costs each server. Sampling is cheap,
	// but it runs continuously for minutes, so it goes through the same
	// budget as everything else.
	Governor *load.Governor
	// Progress, when set, is called after each round so a command can show
	// what is happening during a long recording.
	Progress func(elapsed time.Duration, samples int64, shapes int)
}

func (o SampleOptions) withDefaults() SampleOptions {
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.Interval < 100*time.Millisecond {
		// Below this the sampler costs more than it learns.
		o.Interval = 100 * time.Millisecond
	}
	return o
}

// Sample watches the given nodes and records what they are running.
//
// It stops when the duration elapses or the context is cancelled, and returns
// whatever it gathered either way, so interrupting a long recording still
// produces a usable file.
func Sample(ctx context.Context, nodes []*db.Node, opts SampleOptions) (*Workload, error) {
	opts = opts.withDefaults()
	if len(nodes) == 0 {
		return nil, errors.New("no nodes to sample")
	}

	recorder := NewRecorder(opts.Interval, opts.KeepLiterals)

	if opts.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Duration)
		defer cancel()
	}

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	started := time.Now()

	for {
		select {
		case <-ctx.Done():
			return recorder.Workload(), nil
		case <-ticker.C:
		}

		for _, node := range nodes {
			observations, err := sampleNode(ctx, node, opts)
			if errors.Is(err, load.ErrBusy) {
				// The budget says wait. A skipped round costs a little
				// accuracy; queueing would cost the server.
				continue
			}
			if err != nil {
				if ctx.Err() != nil {
					return recorder.Workload(), nil
				}
				// One unreachable node must not end a recording that may be
				// covering several.
				continue
			}
			recorder.Tick(node.Info.Name, observations)
		}

		if opts.Progress != nil {
			opts.Progress(time.Since(started), recorder.samples, len(recorder.queries))
		}
	}
}

// sampleNode reads what one server is running right now.
func sampleNode(ctx context.Context, node *db.Node, opts SampleOptions) ([]Observation, error) {
	run := func(ctx context.Context) ([]Observation, error) {
		queryCtx, cancel := context.WithTimeout(ctx, load.TierLive.StatementTimeout())
		defer cancel()

		rows, err := node.Pool.Query(queryCtx, db.SQLActivity)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var out []Observation
		for rows.Next() {
			var (
				queryID   int64
				statement string
				runningMS int64
			)
			if err := rows.Scan(&queryID, &statement, &runningMS); err != nil {
				return nil, err
			}

			obs := Observation{
				Node:       node.Info.Name,
				Raw:        statement,
				RunningFor: time.Duration(runningMS) * time.Millisecond,
			}

			// A statement the server truncated cannot be parsed, and guessing
			// at the missing tail would produce wrong recommendations.
			if isTruncated(statement, node.Info.TrackActivityQuerySize) {
				out = append(out, Observation{Node: node.Info.Name})
				continue
			}

			normalized, err := parser.Normalize(statement)
			if err != nil {
				out = append(out, Observation{Node: node.Info.Name})
				continue
			}
			fingerprint, err := parser.Fingerprint(statement)
			if err != nil {
				out = append(out, Observation{Node: node.Info.Name})
				continue
			}
			obs.Normalized = normalized
			obs.Fingerprint = fingerprint
			out = append(out, obs)
		}
		return out, rows.Err()
	}

	if opts.Governor == nil {
		return run(ctx)
	}

	var out []Observation
	release, err := opts.Governor.TryAcquire(ctx, node.Info.Name)
	if err != nil {
		return nil, err
	}
	defer release()

	out, err = run(ctx)
	return out, err
}

// isTruncated reports whether the server cut the statement short.
//
// pg_stat_activity stores only the first track_activity_query_size bytes of
// each statement, so a long query arrives with its tail missing. Parsing the
// fragment would either fail or, worse, succeed against a partial WHERE
// clause and produce a recommendation for a query that does not exist.
func isTruncated(statement string, limit int) bool {
	if limit <= 0 {
		return false
	}
	return len(statement) >= limit-1
}

// Describe explains what a sampled workload can and cannot tell you, for the
// command line and the report.
func (w *Workload) Describe() string {
	if w.Source != SourceSampled {
		return ""
	}
	return fmt.Sprintf(
		"this workload was sampled every %dms rather than read from pg_stat_statements, "+
			"so call counts are estimates: a query running for less than the sample interval "+
			"may be missed entirely, and recommendations are marked with lower confidence",
		w.IntervalMS)
}
