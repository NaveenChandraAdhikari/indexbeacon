package cmd

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/indexbeacon/indexbeacon/internal/advisor"
	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/report"
	"github.com/indexbeacon/indexbeacon/internal/validator"
	"github.com/indexbeacon/indexbeacon/internal/workload"
)

var analyzeOpts struct {
	Source          string
	MaxIndexes      int
	MinGain         float64
	Validate        string
	HotLimit        int
	Partial         bool
	Workload        string
	ValidateQueries int
	ValidateSeconds int
}

var analyzeCmd = &cobra.Command{
	Use:   "analyze",
	Short: "Recommend missing indexes",
	Long: `analyze reads the workload from pg_stat_statements (or a sampled workload
file), derives index candidates from query predicates, joins and ORDER BY
clauses, scores them with planner statistics and prints a ranked list.

Without pg_stat_statements and without --workload, analyze runs in
table-only mode and reports the tables with the most sequential-scan
pressure. No flag ever creates an index.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateFormat("table", "json", "markdown", "sql"); err != nil {
			return err
		}
		return runAnalyze(cmd)
	},
}

func init() {
	f := analyzeCmd.Flags()
	f.StringVar(&analyzeOpts.Source, "source", "auto", "workload source: auto|pss|table")
	f.IntVar(&analyzeOpts.MaxIndexes, "max-indexes", 10, "maximum number of recommendations")
	f.Float64Var(&analyzeOpts.MinGain, "min-gain", 0, "drop recommendations whose score is below this")
	f.StringVar(&analyzeOpts.Validate, "validate", "auto", "validate with hypopg: auto|hypopg|off")
	f.IntVar(&analyzeOpts.HotLimit, "hot-tables", 20, "number of hot tables to show")
	f.IntVar(&analyzeOpts.ValidateQueries, "validate-queries", 5, "queries to plan per candidate during validation")
	f.IntVar(&analyzeOpts.ValidateSeconds, "validate-timeout", 60, "seconds allowed for the whole validation run")
	f.StringVar(&analyzeOpts.Workload, "workload", "",
		"use a recording made by `idxa sample` instead of pg_stat_statements")
	f.BoolVar(&analyzeOpts.Partial, "partial", false, "propose partial indexes when a query pins a column to a literal")
	rootCmd.AddCommand(analyzeCmd)
}

func runAnalyze(cmd *cobra.Command) error {
	if analyzeOpts.Source != "auto" && analyzeOpts.Source != "pss" && analyzeOpts.Source != "table" {
		return exitWith(exitUsage, "unsupported --source %q", analyzeOpts.Source)
	}
	var recording *workload.Workload
	if analyzeOpts.Workload != "" {
		var err error
		recording, err = workload.Load(analyzeOpts.Workload)
		if err != nil {
			return err
		}
		logf("loaded %s: %s", analyzeOpts.Workload, recording.Summary())
	}

	// A recording supplies the workload, so there is no need to read
	// pg_stat_statements from the server as well.
	snaps, err := collectAll(cmd.Context(), analyzeOpts.Source == "table" || recording != nil)
	if err != nil {
		return err
	}
	r := newReport(report.KindAnalyze, snaps)
	statements := 0
	for _, s := range snaps {
		statements += len(s.Statements)
	}
	if recording != nil {
		r.Warnings = append(r.Warnings, recording.Describe())
		if err := workloadReportWith(cmd.Context(), r, snaps, recording); err != nil {
			return err
		}
		return writeReport(r)
	}
	if analyzeOpts.Source == "pss" && statements == 0 {
		return exitWith(exitUnusable, "--source pss requested but no pg_stat_statements rows were readable")
	}
	if statements == 0 {
		tableOnlyReport(r, snaps)
		return writeReport(r)
	}
	if err := workloadReport(cmd.Context(), r, snaps); err != nil {
		return err
	}
	return writeReport(r)
}

// tableOnlyReport fills the report for the mode used when no workload is available.
func tableOnlyReport(r *report.Report, snaps []*collector.Snapshot) {
	r.Mode = "table-only"
	r.HotTables = advisor.HotTables(snaps, opts.MinRows, analyzeOpts.HotLimit)
	r.Warnings = append(r.Warnings, "no query workload available: showing tables with sequential-scan pressure only. Install pg_stat_statements or run `idxa sample` to get index recommendations.")
}

// workloadReport is the query-driven path: parse the workload, derive
// candidates, score them and rank. When the workload yields nothing usable it
// falls back to the table-only view rather than printing an empty report.
func workloadReport(ctx context.Context, r *report.Report, snaps []*collector.Snapshot) error {
	return workloadReportWith(ctx, r, snaps, nil)
}

// workloadReportWith runs the analysis, optionally against a recording rather
// than the server's own statement statistics.
func workloadReportWith(ctx context.Context, r *report.Report, snaps []*collector.Snapshot,
	recording *workload.Workload) error {
	res := advisor.Missing(snaps, advisor.MissingOptions{
		Workload:           recording,
		MinRows:            opts.MinRows,
		MaxRecommendations: analyzeOpts.MaxIndexes,
		MinNet:             analyzeOpts.MinGain,
		Partial:            analyzeOpts.Partial,
		MaxTables:          opts.MaxTables,
	})
	r.Warnings = append(r.Warnings, res.Warnings...)

	if len(res.Recommendations) == 0 {
		r.Mode = "table-only"
		r.HotTables = advisor.HotTables(snaps, opts.MinRows, analyzeOpts.HotLimit)
		r.Warnings = append(r.Warnings,
			"no index candidates scored above the threshold; showing sequential-scan pressure instead")
		return nil
	}

	r.Mode = "heuristic"
	r.Recommendations = res.Recommendations

	// Ask the planner to confirm the recommendations when hypopg is present.
	if validated, warns, err := validateRecommendations(ctx, r); err != nil {
		return err
	} else if validated != nil {
		r.Recommendations = validated
		r.Mode = "validated"
		r.Warnings = append(r.Warnings, warns...)
	}
	// The hot-table list stays useful alongside recommendations: it shows
	// pressure that no candidate addressed.
	r.HotTables = advisor.HotTables(snaps, opts.MinRows, analyzeOpts.HotLimit)
	return nil
}

// validateRecommendations proves recommendations against the planner using
// hypopg, when the extension is installed and validation is not disabled.
//
// It returns nil when validation is skipped, so the caller keeps the
// heuristic report rather than treating the skip as an error.
func validateRecommendations(ctx context.Context, r *report.Report) ([]report.Recommendation, []string, error) {
	if analyzeOpts.Validate == "off" {
		return nil, nil, nil
	}

	// hypopg lives on one node; use the first that has it. A hypothetical
	// index is session-local and costs the server nothing but planning time.
	nodes, err := openNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer closeNodes(nodes)

	for _, n := range nodes {
		if !n.Info.HasExtension("hypopg") {
			continue
		}
		q, err := validator.Acquire(ctx, n.Pool)
		if err != nil {
			return nil, nil, err
		}
		defer q.Release()

		// GENERIC_PLAN, needed to plan statements that still contain
		// parameter placeholders, arrived in PostgreSQL 16.
		generic := n.Info.ServerVersionNum >= 160000
		budget := validator.Budget{
			MaxCandidates: analyzeOpts.MaxIndexes,
			MaxQueriesPer: analyzeOpts.ValidateQueries,
			Deadline:      time.Duration(analyzeOpts.ValidateSeconds) * time.Second,
		}
		logf("validating %d recommendation(s) with hypopg on %s", len(r.Recommendations), n.Info.Name)
		return advisor.Validate(ctx, q, generic, r.Recommendations, budget)
	}

	if analyzeOpts.Validate == "hypopg" {
		return nil, nil, exitWith(exitUnusable,
			"--validate hypopg requested but the hypopg extension is not installed on any node")
	}
	return nil, []string{
		"hypopg is not installed, so recommendations are heuristic estimates rather than planner-confirmed;" +
			" install it with CREATE EXTENSION hypopg to have idxa validate each suggestion",
	}, nil
}
