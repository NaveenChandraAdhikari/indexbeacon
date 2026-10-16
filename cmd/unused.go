package cmd

import (
	"github.com/spf13/cobra"

	"github.com/indexbeacon/indexbeacon/internal/advisor"
	"github.com/indexbeacon/indexbeacon/internal/report"
)

var unusedMinSize int64

var unusedCmd = &cobra.Command{
	Use:   "unused",
	Short: "Report indexes with zero scans across all nodes",
	Long: `unused lists indexes whose idx_scan counter is zero on every configured
node since the last statistics reset. Primary keys, unique indexes,
constraint-backing indexes and invalid indexes are never listed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateFormat("table", "json", "markdown", "sql"); err != nil {
			return err
		}
		snaps, err := collectAll(cmd.Context(), true)
		if err != nil {
			return err
		}
		r := newReport(report.KindUnused, snaps)
		unused, warnings := advisor.Unused(snaps, advisor.UnusedOptions{MinStatsAge: opts.MinStatsAge, MinSizeBytes: unusedMinSize})
		r.Unused = unused
		r.Warnings = append(r.Warnings, warnings...)
		return writeReport(r)
	},
}

func init() {
	unusedCmd.Flags().Int64Var(&unusedMinSize, "min-size", 0, "hide indexes smaller than this many bytes")
	rootCmd.AddCommand(unusedCmd)
}
