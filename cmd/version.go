package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Set via -ldflags at release time.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print idxa version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		v, c, d := version, commit, date
		if bi, ok := debug.ReadBuildInfo(); ok && v == "dev" {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if c == "" {
						c = s.Value
					}
				case "vcs.time":
					if d == "" {
						d = s.Value
					}
				}
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "idxa %s", v)
		if c != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " (%s", c)
			if d != "" {
				fmt.Fprintf(cmd.OutOrStdout(), " %s", d)
			}
			fmt.Fprint(cmd.OutOrStdout(), ")")
		}
		fmt.Fprintf(cmd.OutOrStdout(), " %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
