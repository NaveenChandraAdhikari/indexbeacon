package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// checkFinding is one line of the doctor report.
type checkFinding struct {
	Level   string `json:"level"` // ok | warn | fail
	Message string `json:"message"`
}

type checkNodeReport struct {
	Node     db.Info        `json:"node"`
	Findings []checkFinding `json:"findings"`
}

type checkReport struct {
	Nodes    []checkNodeReport `json:"nodes"`
	Usable   bool              `json:"usable"`
	Warnings int               `json:"warnings"`
	Failures int               `json:"failures"`
}

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Verify connectivity, privileges, extensions and stats freshness",
	Long: `check connects to every configured node and reports whether idxa can do
useful work there. It exits with status 3 if the target is unusable
(cannot connect, or no statistics are readable).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateFormat("table", "json"); err != nil {
			return err
		}
		ctx := cmd.Context()
		nodes, err := openNodes(ctx)
		if err != nil {
			var ec *exitCodeError
			if errors.As(err, &ec) {
				return err
			}
			return exitWith(exitUnusable, "%v", err)
		}
		defer closeNodes(nodes)

		rep := checkReport{Usable: true}
		for _, n := range nodes {
			nr := checkNodeReport{Node: n.Info, Findings: doctor(&n.Info, opts.MinStatsAge)}
			for _, f := range nr.Findings {
				switch f.Level {
				case "warn":
					rep.Warnings++
				case "fail":
					rep.Failures++
					rep.Usable = false
				}
			}
			rep.Nodes = append(rep.Nodes, nr)
		}

		w, closeFn, err := outputWriter()
		if err != nil {
			return err
		}
		defer closeFn() //nolint:errcheck

		if opts.Format == "json" {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			if err := enc.Encode(rep); err != nil {
				return err
			}
		} else {
			renderCheckTable(w, rep)
		}
		if !rep.Usable {
			return exitWith(exitUnusable, "")
		}
		return nil
	},
}

// doctor evaluates one node. It is pure so it can be unit-tested.
func doctor(info *db.Info, minStatsAge time.Duration) []checkFinding {
	var out []checkFinding
	ok := func(f string, a ...any) { out = append(out, checkFinding{"ok", fmt.Sprintf(f, a...)}) }
	warn := func(f string, a ...any) { out = append(out, checkFinding{"warn", fmt.Sprintf(f, a...)}) }
	fail := func(f string, a ...any) { out = append(out, checkFinding{"fail", fmt.Sprintf(f, a...)}) }

	ok("connected as %s to %s (%s)", info.User, info.Database, info.Role)
	if info.ServerVersionNum < 120000 {
		fail("server version %d is below the supported minimum (12)", info.ServerVersionNum)
	} else {
		ok("server version %s", shortVersion(info.ServerVersion))
	}
	if info.ServerVersionNum < 160000 {
		warn("GENERIC_PLAN needs PG16+; parameterised statements will use heuristic costs")
	}

	switch {
	case info.Superuser:
		ok("role is superuser; all statistics readable")
	case info.ReadAllStats:
		ok("role has pg_read_all_stats")
	default:
		warn("role lacks pg_read_all_stats: pg_stat_statements text for other users will be hidden")
	}

	if info.HasExtension("pg_stat_statements") {
		if info.PSSPreloaded() {
			ok("pg_stat_statements installed and preloaded")
		} else {
			warn("pg_stat_statements is installed but not in shared_preload_libraries; workload will be sampled from pg_stat_activity")
		}
	} else {
		warn("pg_stat_statements not installed; use `idxa sample` or --workload for query-level advice")
	}
	if info.HasExtension("hypopg") {
		ok("hypopg installed; recommendations can be validated with hypothetical indexes")
	} else {
		warn("hypopg not installed; recommendations will use the heuristic cost model only")
	}
	if info.TrackActivityQuerySize < 4096 {
		warn("track_activity_query_size=%d may truncate long queries during sampling (recommend >= 4096)", info.TrackActivityQuerySize)
	} else {
		ok("track_activity_query_size=%d", info.TrackActivityQuerySize)
	}
	if age, known := info.StatsAge(); known {
		if age < minStatsAge {
			warn("pg_stat_database counters were reset %s ago (< %s); usage-based advice has low confidence", age.Round(time.Second), minStatsAge)
		} else {
			ok("statistics window: %s", age.Round(time.Minute))
		}
	} else {
		warn("pg_stat_database.stats_reset is NULL; statistics age unknown")
	}
	if info.PSSStatsReset != nil {
		age := info.ServerNow.Sub(*info.PSSStatsReset)
		if age < minStatsAge {
			warn("pg_stat_statements was reset %s ago (< %s)", age.Round(time.Second), minStatsAge)
		}
	}
	return out
}

func shortVersion(v string) string {
	// "PostgreSQL 16.4 (Debian ...)" -> "PostgreSQL 16.4"
	parts := strings.SplitN(v, " (", 2)
	return parts[0]
}

func renderCheckTable(w io.Writer, rep checkReport) {
	for _, nr := range rep.Nodes {
		fmt.Fprintf(w, "%s  %s\n", nr.Node.Name, nr.Node.DSN)
		for _, f := range nr.Findings {
			mark := map[string]string{"ok": "  ok  ", "warn": " warn ", "fail": " FAIL "}[f.Level]
			fmt.Fprintf(w, "  [%s] %s\n", mark, f.Message)
		}
		fmt.Fprintln(w)
	}
	switch {
	case !rep.Usable:
		fmt.Fprintf(w, "result: UNUSABLE (%d failures, %d warnings)\n", rep.Failures, rep.Warnings)
	case rep.Warnings > 0:
		fmt.Fprintf(w, "result: usable with %d warnings\n", rep.Warnings)
	default:
		fmt.Fprintln(w, "result: ok")
	}
}

func init() {
	rootCmd.AddCommand(checkCmd)
}
