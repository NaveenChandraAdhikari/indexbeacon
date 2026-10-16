// Package cmd implements the idxa command-line interface.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// Global options shared by every subcommand.
type globalOptions struct {
	DSNs           []string
	DSNFile        string
	Format         string
	Output         string
	Timeout        time.Duration
	Schemas        []string
	ExcludeSchemas []string
	MinRows        int64
	MaxTables      int
	MinStatsAge    time.Duration
	Verbose        bool
}

var opts globalOptions

// Exit codes.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitUnusable = 3 // `check` found the target unusable
)

var rootCmd = &cobra.Command{
	Use:   "idxa",
	Short: "Read-only Postgres index advisor",
	Long: `idxa inspects one or more Postgres nodes and recommends missing indexes,
reports unused and duplicate indexes, and never modifies the database.

Connection strings are taken from --dsn (repeatable), --dsn-file, or the
	IDXA_DSNS environment variable (comma or newline separated). When several
	nodes are given, idxa detects the primary with pg_is_in_recovery() and sums
usage counters across replicas.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringArrayVar(&opts.DSNs, "dsn", nil, "connection string (repeatable; primary auto-detected)")
	pf.StringVar(&opts.DSNFile, "dsn-file", "", "file with one connection string per line")
	pf.StringVar(&opts.Format, "format", "table", "output format: table|json|markdown|sql")
	pf.StringVarP(&opts.Output, "output", "o", "", "write output to file instead of stdout")
	pf.DurationVar(&opts.Timeout, "timeout", 30*time.Second, "per-statement timeout")
	pf.StringSliceVar(&opts.Schemas, "schemas", nil, "only analyze these schemas (default: all non-system)")
	pf.StringSliceVar(&opts.ExcludeSchemas, "exclude-schemas", nil, "skip these schemas")
	pf.Int64Var(&opts.MinRows, "min-rows", 1000, "ignore tables with fewer estimated rows")
	pf.IntVar(&opts.MaxTables, "max-tables", 2000, "on wide schemas, analyse only this many tables, chosen by scan pressure")
	pf.DurationVar(&opts.MinStatsAge, "min-stats-age", time.Hour, "warn and lower confidence when stats are younger than this")
	pf.BoolVarP(&opts.Verbose, "verbose", "v", false, "log progress to stderr")
}

// Execute runs the CLI and returns a process exit code.
func Execute() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := rootCmd.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	var ec *exitCodeError
	if errors.As(err, &ec) {
		if ec.msg != "" {
			fmt.Fprintln(os.Stderr, "idxa:", ec.msg)
		}
		return ec.code
	}
	fmt.Fprintln(os.Stderr, "idxa:", err)
	return exitError
}

// exitCodeError carries a specific exit code out of a command.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

func exitWith(code int, format string, args ...any) error {
	return &exitCodeError{code: code, msg: fmt.Sprintf(format, args...)}
}

// resolveDSNs merges --dsn, --dsn-file and IDXA_DSNS (in that priority).
func resolveDSNs() ([]string, error) {
	var out []string
	out = append(out, opts.DSNs...)
	if opts.DSNFile != "" {
		data, err := os.ReadFile(opts.DSNFile)
		if err != nil {
			return nil, fmt.Errorf("read --dsn-file: %w", err)
		}
		out = append(out, splitDSNList(string(data))...)
	}
	if len(out) == 0 {
		out = append(out, splitDSNList(os.Getenv("IDXA_DSNS"))...)
	}
	if len(out) == 0 {
		return nil, exitWith(exitUsage, "no connection string: use --dsn, --dsn-file or IDXA_DSNS")
	}
	return out, nil
}

func splitDSNList(s string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// openNodes connects to every configured DSN. The returned slice has the
// primary first (if one was found). Callers must close every node.
func openNodes(ctx context.Context) ([]*db.Node, error) {
	dsns, err := resolveDSNs()
	if err != nil {
		return nil, err
	}
	nodes := make([]*db.Node, 0, len(dsns))
	for i, dsn := range dsns {
		name := fmt.Sprintf("node%d", i+1)
		n, err := db.Open(ctx, name, dsn, db.Options{StatementTimeout: opts.Timeout})
		if err != nil {
			closeNodes(nodes)
			return nil, fmt.Errorf("connect %s (%s): %w", name, db.RedactDSN(dsn), err)
		}
		logf("connected %s: %s (%s, pg %d)", name, n.Info.DSN, n.Info.Role, n.Info.ServerVersionNum)
		nodes = append(nodes, n)
	}
	// Primary first.
	for i, n := range nodes {
		if n.Info.Role == db.RolePrimary && i != 0 {
			nodes[0], nodes[i] = nodes[i], nodes[0]
			break
		}
	}
	return nodes, nil
}

func closeNodes(nodes []*db.Node) {
	for _, n := range nodes {
		n.Close()
	}
}

// outputWriter returns where reports are written, honouring --output.
func outputWriter() (io.Writer, func() error, error) {
	if opts.Output == "" || opts.Output == "-" {
		return os.Stdout, func() error { return nil }, nil
	}
	f, err := os.Create(opts.Output)
	if err != nil {
		return nil, nil, fmt.Errorf("open --output: %w", err)
	}
	return f, f.Close, nil
}

func logf(format string, args ...any) {
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "idxa: "+format+"\n", args...)
	}
}

func validateFormat(allowed ...string) error {
	for _, a := range allowed {
		if opts.Format == a {
			return nil
		}
	}
	return exitWith(exitUsage, "unsupported --format %q (allowed: %s)", opts.Format, strings.Join(allowed, "|"))
}
