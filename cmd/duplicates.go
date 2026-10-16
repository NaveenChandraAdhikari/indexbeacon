package cmd

import (
	"github.com/spf13/cobra"

	"github.com/indexbeacon/indexbeacon/internal/advisor"
	"github.com/indexbeacon/indexbeacon/internal/report"
)

var duplicatesCmd = &cobra.Command{
	Use:   "duplicates",
	Short: "Report exact-duplicate and prefix-redundant indexes",
	Long: `duplicates finds indexes that are exact copies of another index, or whose
key columns are a leading prefix of another index with the same access
method, operator classes, collations, sort options and predicate. Primary
keys and unique indexes are never proposed for removal.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateFormat("table", "json", "markdown", "sql"); err != nil {
			return err
		}
		snaps, err := collectAll(cmd.Context(), true)
		if err != nil {
			return err
		}
		r := newReport(report.KindDuplicates, snaps)
		r.Duplicates = advisor.Duplicates(snaps)
		return writeReport(r)
	},
}

func init() {
	rootCmd.AddCommand(duplicatesCmd)
}
