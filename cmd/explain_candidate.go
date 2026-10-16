package cmd

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/indexbeacon/indexbeacon/internal/candidates"
	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/db"
	"github.com/indexbeacon/indexbeacon/internal/validator"
)

var explainOpts struct {
	Index     string
	Query     string
	QueryFile string
	QueryID   int64
}

var explainCandidateCmd = &cobra.Command{
	Use:   "explain-candidate",
	Short: "Show what the planner would do with one index",
	Long: `explain-candidate creates one index hypothetically and prints the plan for a
query with and without it.

Use it when a recommendation looks wrong, or when you have an index in mind
that idxa did not suggest. It answers the only question that matters: would
the planner actually use this?

    idxa explain-candidate --dsn ... \
      --index "shop.orders(customer_id, created_at DESC)" \
      --query "SELECT * FROM shop.orders WHERE customer_id = 42 ORDER BY created_at DESC LIMIT 20"

Nothing is created on disk. The hypothetical index exists only inside this
session and disappears when it ends. It needs the hypopg extension.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runExplainCandidate(cmd.Context())
	},
}

func init() {
	f := explainCandidateCmd.Flags()
	f.StringVar(&explainOpts.Index, "index", "",
		`the index to test, as "schema.table(col, col DESC)" with an optional " WHERE ..."`)
	f.StringVar(&explainOpts.Query, "query", "", "the query to plan")
	f.StringVar(&explainOpts.QueryFile, "query-file", "", "read the query from a file")
	f.Int64Var(&explainOpts.QueryID, "queryid", 0, "take the query from pg_stat_statements by its identifier")
	rootCmd.AddCommand(explainCandidateCmd)
}

func runExplainCandidate(ctx context.Context) error {
	if explainOpts.Index == "" {
		return exitWith(exitUsage, "--index is required, for example: --index \"public.orders(customer_id)\"")
	}

	candidate, err := parseIndexSpec(explainOpts.Index)
	if err != nil {
		return exitWith(exitUsage, "%v", err)
	}

	nodes, err := openNodes(ctx)
	if err != nil {
		return err
	}
	defer closeNodes(nodes)

	node := nodes[0]
	for _, n := range nodes {
		if n.Info.HasExtension("hypopg") {
			node = n
			break
		}
	}
	if !node.Info.HasExtension("hypopg") {
		return exitWith(exitUnusable,
			"this needs the hypopg extension, which is not installed.\nAdd it with: CREATE EXTENSION hypopg;")
	}

	query, err := resolveQuery(ctx, node)
	if err != nil {
		return err
	}

	q, err := validator.Acquire(ctx, node.Pool)
	if err != nil {
		return err
	}
	defer q.Release()

	v := validator.New(q, node.Info.ServerVersionNum >= 160000, validator.Budget{MaxCandidates: 1})
	defer v.Reset(ctx)

	generic := node.Info.ServerVersionNum >= 160000

	before, err := validator.Explain(ctx, q, query, generic)
	if err != nil {
		return exitWith(exitUnusable, "could not plan the query: %v", err)
	}

	candidate.Queries = []candidates.QueryRef{{SQL: query, Calls: 1}}
	results, err := v.ValidateAll(ctx, []*candidates.Candidate{candidate})
	if err != nil {
		return err
	}

	fmt.Printf("index:  %s\n", candidate.DDL())
	fmt.Printf("query:  %s\n\n", oneLine(query))

	fmt.Println("without the index")
	fmt.Println("  " + strings.ReplaceAll(validator.RenderPlanText(before), "\n", "\n  "))

	if len(results) == 0 || !results[0].Validated {
		reason := "the planner could not be consulted"
		if len(results) > 0 && results[0].Note != "" {
			reason = results[0].Note
		}
		fmt.Printf("\nwith the index\n  %s\n", reason)
		return nil
	}

	res := results[0]
	fmt.Println("\nwith the index")
	for _, qr := range res.Queries {
		if qr.Skipped != "" {
			fmt.Printf("  could not plan: %s\n", qr.Skipped)
			continue
		}
		fmt.Printf("  total cost %.2f\n", qr.CostAfter)
		fmt.Printf("  planner uses it: %v\n", qr.Used)
		if qr.Used && qr.CostBefore > 0 {
			fmt.Printf("  cost falls from %.2f to %.2f, a %.0f%% reduction\n",
				qr.CostBefore, qr.CostAfter, 100*(1-qr.CostAfter/qr.CostBefore))
		}
	}
	if res.SizeBytes > 0 {
		fmt.Printf("\nestimated size: %s\n", humanBytes(res.SizeBytes))
	}
	if !res.Used {
		fmt.Println("\nThe planner would not use this index for this query. That usually means the " +
			"column order does not match how the query filters, the index is not selective enough " +
			"to beat a sequential scan, or a type or collation mismatch prevents it being used.")
	}
	return nil
}

// indexSpec matches "schema.table(col, col DESC) WHERE ..." with the schema
// and the predicate both optional.
var indexSpec = regexp.MustCompile(`^\s*(?:([\w$]+)\.)?([\w$]+)\s*\(([^)]*)\)\s*(?:WHERE\s+(.+?))?\s*$`)

// parseIndexSpec turns the command line form into a candidate.
func parseIndexSpec(spec string) (*candidates.Candidate, error) {
	m := indexSpec.FindStringSubmatch(strings.TrimSpace(spec))
	if m == nil {
		return nil, fmt.Errorf(
			"could not read %q as an index. Use the form \"schema.table(col1, col2 DESC)\","+
				" optionally followed by WHERE a condition", spec)
	}

	c := &candidates.Candidate{Schema: m[1], Table: m[2], Predicate: strings.TrimSpace(m[4])}
	for _, part := range strings.Split(m[3], ",") {
		name, rest := splitColumnName(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		col := candidates.Column{Name: name}
		for _, extra := range strings.Fields(rest) {
			switch strings.ToUpper(extra) {
			case "DESC":
				col.Desc = true
			case "ASC":
			default:
				col.Opclass = candidates.Opclass(extra)
			}
		}
		c.Columns = append(c.Columns, col)
	}
	if len(c.Columns) == 0 {
		return nil, fmt.Errorf("no columns given in %q", spec)
	}
	return c, nil
}

// splitColumnName separates a column name from what follows it, respecting
// double quotes.
//
// A quoted identifier may contain spaces, so splitting the whole thing on
// whitespace would turn "order date" into a column called "order" followed by
// a mystery keyword.
func splitColumnName(s string) (name, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if s[0] == '"' {
		if end := strings.Index(s[1:], `"`); end >= 0 {
			return s[1 : end+1], strings.TrimSpace(s[end+2:])
		}
		return strings.Trim(s, `"`), ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i:])
	}
	return s, ""
}

// resolveQuery finds the statement to plan, from a flag, a file, or
// pg_stat_statements.
func resolveQuery(ctx context.Context, node *db.Node) (string, error) {
	switch {
	case explainOpts.Query != "":
		return explainOpts.Query, nil

	case explainOpts.QueryFile != "":
		data, err := os.ReadFile(explainOpts.QueryFile)
		if err != nil {
			return "", fmt.Errorf("read --query-file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil

	case explainOpts.QueryID != 0:
		snap, err := collector.Collect(ctx, node, collector.Options{})
		if err != nil {
			return "", err
		}
		for _, st := range snap.Statements {
			if st.QueryID == explainOpts.QueryID {
				return st.Query, nil
			}
		}
		return "", exitWith(exitUnusable,
			"no statement with identifier %d is in pg_stat_statements", explainOpts.QueryID)
	}

	return "", exitWith(exitUsage, "give a query with --query, --query-file or --queryid")
}

// oneLine collapses a statement for a single-line heading.
func oneLine(s string) string {
	out := strings.Join(strings.Fields(s), " ")
	if len(out) > 140 {
		return out[:137] + "..."
	}
	return out
}

// humanBytes renders a size the way PostgreSQL does.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d bytes", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}
