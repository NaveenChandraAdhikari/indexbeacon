package candidates

import (
	"sort"
	"strings"

	"github.com/indexbeacon/indexbeacon/internal/parser"
)

// MaxKeyColumns caps composite width. Beyond about four columns an index is
// large, expensive to maintain, and rarely more selective than its prefix.
const MaxKeyColumns = 4

// Options tunes candidate generation.
type Options struct {
	// MaxColumns caps key columns per candidate. Zero selects MaxKeyColumns.
	MaxColumns int
	// Partial enables partial index candidates when a literal is known.
	Partial bool
	// Covering enables INCLUDE columns. Off by default: a covering index is
	// larger and only pays off for index-only scans.
	Covering bool
	// Selectivity, when set, estimates how selective a column is so that
	// equality columns can be ordered most-selective-first. Lower is more
	// selective. Missing entries fall back to a stable name ordering.
	Selectivity func(table, column string) (float64, bool)
	// ColumnType, when set, reports a column's type and physical
	// correlation, which is what decides the index type. Without it every
	// candidate is a btree, which is wrong for jsonb, arrays and full-text
	// columns.
	ColumnType func(table, column string) (ColumnType, bool)
	// Rows, when set, reports a table's estimated row count, used to decide
	// whether a BRIN index is worth offering as an alternative.
	Rows func(table string) int64
}

func (o Options) maxColumns() int {
	if o.MaxColumns <= 0 {
		return MaxKeyColumns
	}
	return o.MaxColumns
}

// Generate produces candidate indexes for one query shape.
//
// Rules, in the order they matter:
//  1. Composite of equality columns, most selective first, then at most one
//     range column, then the ORDER BY columns. This is the shape that serves
//     the widest set of queries.
//  2. Equality columns plus the ORDER BY columns, which is what makes a
//     "latest N for this key" query fast.
//  3. Single leading equality column, as a cheaper alternative.
//  4. Join key columns, since either side of an equi-join may need one.
//  5. A partial index when the query pins a column to a known literal.
func Generate(shape *parser.Shape, ref QueryRef, opts Options) []*Candidate {
	var out []*Candidate

	for _, table := range shape.TablesWithPredicates() {
		preds := shape.PredicatesFor(table)
		if len(preds) == 0 {
			continue
		}
		schema, name := splitTable(table)

		eq, rng, other := classify(preds)

		// Predicates a btree cannot answer at all become their own
		// single-column candidates with the right access method, rather than
		// being folded into a composite that would never be used.
		for _, p := range preds {
			if !p.Op.NeedsInvertedIndex() {
				continue
			}
			ct, _ := lookupType(opts, table, columnKey(p))
			method, opclass, why := chooseMethod(p.Op, ct)
			col := columnOf(p)
			col.Opclass = opclass
			r := ref
			r.Rule = "inverted"
			out = append(out, &Candidate{
				Schema:  schema,
				Table:   name,
				Columns: []Column{col},
				Method:  method,
				Reason:  why,
				Queries: []QueryRef{r},
			})
		}
		orderCols := orderColumnsFor(shape, table, eq, rng)
		sortEqualityColumns(eq, table, opts)

		add := func(cols []Column, predicate, rule string) {
			cols = dedupeColumns(cols)
			if len(cols) == 0 {
				return
			}
			if len(cols) > opts.maxColumns() {
				cols = cols[:opts.maxColumns()]
			}
			r := ref
			r.Rule = rule
			out = append(out, &Candidate{
				Schema:    schema,
				Table:     name,
				Columns:   cols,
				Predicate: predicate,
				Queries:   []QueryRef{r},
			})
		}

		eqCols := columnsOf(eq)

		// Rule 1: equality columns, then one range column, then ordering.
		if len(eqCols) > 0 || len(rng) > 0 {
			cols := append([]Column{}, eqCols...)
			if len(rng) > 0 {
				cols = append(cols, Column{Name: rng[0].Column, Expr: rng[0].Column == ""})
				if rng[0].Column == "" {
					cols[len(cols)-1].Name = rng[0].Expr
				}
			}
			cols = append(cols, orderCols...)
			add(cols, "", "equality+range+order")
		}

		// Rule 2: equality columns plus ordering, without the range column.
		// A range column between the equality columns and the sort columns
		// destroys the index's ability to supply the ordering, so this
		// variant is often the better one when the query has a LIMIT.
		if len(eqCols) > 0 && len(orderCols) > 0 && len(rng) > 0 {
			cols := append([]Column{}, eqCols...)
			cols = append(cols, orderCols...)
			add(cols, "", "equality+order")
		}

		// Rule 3: the single leading equality column, a cheaper alternative
		// that the merge step will discard if a longer index subsumes it.
		if len(eqCols) > 1 {
			add([]Column{eqCols[0]}, "", "leading-equality")
		}

		// Rule 4: join keys. Either side of an equi-join may be the one
		// scanned second and therefore the one needing the index.
		for _, p := range preds {
			if p.FromJoin && p.Op == parser.OpEq {
				add([]Column{columnOf(p)}, "", "join-key")
			}
		}

		// Rule 5: a partial index, when the query pins a column to a literal
		// and other columns remain to index. Partial indexes are much
		// smaller, which is the whole reason to prefer one.
		if opts.Partial {
			for _, p := range eq {
				if p.Const == "" || p.Param || p.Column == "" {
					continue
				}
				rest := withoutColumn(eqCols, p.Column)
				rest = append(rest, orderCols...)
				rest = withoutColumn(rest, p.Column)
				if len(rest) == 0 {
					continue
				}
				add(rest, quoteIdent(p.Column)+" = "+quoteLiteral(p.Const), "partial")
			}
		}

		_ = other
	}

	// Offer a BRIN alternative where one would genuinely be better: a very
	// large table whose values follow physical order.
	if opts.Rows != nil && opts.ColumnType != nil {
		var extra []*Candidate
		for _, c := range out {
			types := map[string]ColumnType{}
			for _, col := range c.Columns {
				if ct, ok := lookupType(opts, c.QualifiedTable(), col.Name); ok {
					types[col.Name] = ct
				}
			}
			if ok, why := brinAlternative(c, opts.Rows(c.QualifiedTable()), types); ok {
				clone := *c
				clone.Method = MethodBRIN
				clone.Reason = why
				clone.Queries = append([]QueryRef{}, c.Queries...)
				extra = append(extra, &clone)
			}
		}
		out = append(out, extra...)
	}

	sortCandidates(out)
	return out
}

// lookupType resolves a column's type through the options, if provided.
func lookupType(opts Options, table, column string) (ColumnType, bool) {
	if opts.ColumnType == nil {
		return ColumnType{}, false
	}
	return opts.ColumnType(table, column)
}

// classify splits predicates into equality, range and everything else,
// keeping only the strongest predicate per column.
func classify(preds []parser.Predicate) (eq, rng, other []parser.Predicate) {
	strongest := map[string]parser.Predicate{}
	order := []string{}

	for _, p := range preds {
		key := p.Column
		if key == "" {
			key = p.Expr
		}
		if key == "" {
			continue
		}
		cur, seen := strongest[key]
		if !seen {
			strongest[key] = p
			order = append(order, key)
			continue
		}
		if rank(p.Op) > rank(cur.Op) {
			strongest[key] = p
		}
	}

	for _, key := range order {
		p := strongest[key]
		switch {
		case p.Op.NeedsInvertedIndex():
			// Handled separately: these need their own index type.
			other = append(other, p)
		case p.Op.IsEquality():
			eq = append(eq, p)
		case p.Op == parser.OpRange || p.Op == parser.OpLikePrefix:
			rng = append(rng, p)
		case p.Op == parser.OpIsNotNull:
			other = append(other, p)
		default:
			other = append(other, p)
		}
	}
	return eq, rng, other
}

// rank orders predicate kinds by how useful they are as a leading column.
func rank(op parser.PredicateOp) int {
	switch op {
	case parser.OpEq:
		return 5
	case parser.OpIn:
		return 4
	case parser.OpIsNull:
		return 3
	case parser.OpRange, parser.OpLikePrefix:
		return 2
	case parser.OpIsNotNull:
		return 1
	default:
		return 0
	}
}

// sortEqualityColumns orders equality predicates most selective first. A more
// selective leading column eliminates more rows earlier, and the resulting
// index also serves more queries as a prefix.
func sortEqualityColumns(eq []parser.Predicate, table string, opts Options) {
	sort.SliceStable(eq, func(i, j int) bool {
		if opts.Selectivity != nil {
			si, oki := opts.Selectivity(table, eq[i].Column)
			sj, okj := opts.Selectivity(table, eq[j].Column)
			if oki && okj && si != sj {
				return si < sj
			}
			if oki != okj {
				return oki
			}
		}
		// Without statistics, order by name so output stays deterministic.
		return columnKey(eq[i]) < columnKey(eq[j])
	})
}

func columnKey(p parser.Predicate) string {
	if p.Column != "" {
		return p.Column
	}
	return p.Expr
}

// orderColumnsFor returns the ORDER BY columns usable as trailing index
// columns, provided they do not merely repeat a column already present.
func orderColumnsFor(shape *parser.Shape, table string, eq, rng []parser.Predicate) []Column {
	entries := shape.OrderByFor(table)
	if len(entries) == 0 {
		return nil
	}
	used := map[string]bool{}
	for _, p := range eq {
		used[columnKey(p)] = true
	}
	// A range column may still be followed by an ordering column, but the
	// index cannot supply the sort in that case, so only skip exact repeats.
	var out []Column
	for _, o := range entries {
		if used[o.Column] {
			continue
		}
		out = append(out, Column{Name: o.Column, Desc: o.Desc})
		used[o.Column] = true
	}
	return out
}

// columnsOf renders predicates as index columns.
func columnsOf(preds []parser.Predicate) []Column {
	out := make([]Column, 0, len(preds))
	for _, p := range preds {
		out = append(out, columnOf(p))
	}
	return out
}

func columnOf(p parser.Predicate) Column {
	if p.Column != "" {
		return Column{Name: p.Column}
	}
	return Column{Name: p.Expr, Expr: true}
}

// dedupeColumns removes repeated columns, keeping first occurrence, since an
// index cannot list the same column twice.
func dedupeColumns(cols []Column) []Column {
	seen := map[string]bool{}
	out := cols[:0]
	for _, c := range cols {
		if c.Name == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	return out
}

func withoutColumn(cols []Column, name string) []Column {
	out := make([]Column, 0, len(cols))
	for _, c := range cols {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}

// splitTable separates a qualified name into schema and table.
func splitTable(qualified string) (schema, table string) {
	if i := strings.LastIndex(qualified, "."); i >= 0 {
		return qualified[:i], qualified[i+1:]
	}
	return "", qualified
}

// quoteLiteral renders a string constant safely for a partial index
// predicate.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
