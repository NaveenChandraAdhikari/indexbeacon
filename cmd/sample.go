package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/indexbeacon/indexbeacon/internal/load"
	"github.com/indexbeacon/indexbeacon/internal/workload"
)

var sampleOpts struct {
	Duration     time.Duration
	Interval     time.Duration
	Out          string
	KeepLiterals bool
	Quiet        bool
}

var sampleCmd = &cobra.Command{
	Use:   "sample",
	Short: "Record which queries a database runs, without pg_stat_statements",
	Long: `sample watches pg_stat_activity and records what the database is running.

It exists for servers where pg_stat_statements is not available. That
extension needs a line in postgresql.conf and a restart, which on a managed
service can mean a maintenance window, so plenty of databases do not have it.
Without a workload idxa can still report unused and duplicate indexes, but it
cannot recommend new ones.

Sampling fills the gap: a query that runs constantly appears in most samples,
one that runs rarely appears in few. The resulting counts are estimates, not
exact call counts, and every recommendation derived from them is marked with
lower confidence.

Feed the recording to analyze:

    idxa sample --dsn ... --duration 10m --out workload.json.gz
    idxa analyze --dsn ... --workload workload.json.gz

Press Ctrl-C at any time; the recording so far is written out.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runSample(cmd)
	},
}

func init() {
	f := sampleCmd.Flags()
	f.DurationVar(&sampleOpts.Duration, "duration", 5*time.Minute, "how long to record (0 records until interrupted)")
	f.DurationVar(&sampleOpts.Interval, "interval", time.Second, "how often to look at what is running")
	f.StringVar(&sampleOpts.Out, "out", "workload.json.gz", "file to write the recording to")
	f.BoolVar(&sampleOpts.KeepLiterals, "keep-literals", false,
		"store one example of each query with its values, which partial index candidates need; statement text may contain personal data")
	f.BoolVar(&sampleOpts.Quiet, "quiet", false, "do not print progress")
	rootCmd.AddCommand(sampleCmd)
}

func runSample(cmd *cobra.Command) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nodes, err := openNodes(ctx)
	if err != nil {
		return err
	}
	defer closeNodes(nodes)

	// Warn before spending several minutes producing something unusable.
	for _, n := range nodes {
		if n.Info.TrackActivityQuerySize > 0 && n.Info.TrackActivityQuerySize < 4096 {
			fmt.Fprintf(os.Stderr,
				"warning: %s truncates statements at %d bytes, so longer queries cannot be recorded.\n"+
					"         Raise track_activity_query_size to 4096 or more for a complete picture.\n",
				n.Info.Name, n.Info.TrackActivityQuerySize)
		}
	}

	if !sampleOpts.Quiet {
		what := sampleOpts.Duration.String()
		if sampleOpts.Duration == 0 {
			what = "until interrupted"
		}
		fmt.Fprintf(os.Stderr, "recording every %s for %s; press Ctrl-C to stop early\n",
			sampleOpts.Interval, what)
	}

	governor := load.NewGovernor(load.Options{
		MinInterval: sampleOpts.Interval / 2,
	})

	var lastPrint time.Time
	progress := func(elapsed time.Duration, samples int64, shapes int) {
		if sampleOpts.Quiet || time.Since(lastPrint) < 2*time.Second {
			return
		}
		lastPrint = time.Now()
		fmt.Fprintf(os.Stderr, "\r%s elapsed, %d samples, %d distinct queries   ",
			elapsed.Round(time.Second), samples, shapes)
	}

	w, err := workload.Sample(ctx, nodes, workload.SampleOptions{
		Interval:     sampleOpts.Interval,
		Duration:     sampleOpts.Duration,
		KeepLiterals: sampleOpts.KeepLiterals,
		Governor:     governor,
		Progress:     progress,
	})
	if err != nil {
		return err
	}
	if !sampleOpts.Quiet {
		fmt.Fprintln(os.Stderr)
	}

	if len(w.Queries) == 0 {
		return exitWith(exitUnusable,
			"no queries were seen in %s. Either the database is idle, or its statements are\n"+
				"being truncated. Record while the application is under normal load.",
			w.Duration().Round(time.Second))
	}

	if err := workload.Save(sampleOpts.Out, w); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "wrote %s: %s\n", sampleOpts.Out, w.Summary())
	fmt.Fprintf(os.Stderr, "now run: idxa analyze --workload %s\n", sampleOpts.Out)
	return nil
}
