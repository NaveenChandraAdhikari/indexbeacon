// Package parser turns SQL text into the small amount of structure an index
// advisor actually needs: which tables a statement touches, which columns it
// filters on and how, which columns it joins on, and how it orders results.
//
// Parsing uses the real PostgreSQL grammar via libpg_query rather than
// pattern matching, because the difference shows up immediately on ordinary
// production SQL: casts, CTEs, subqueries, parameter placeholders and
// operator classes all defeat a regular expression, and a wrong predicate
// produces a wrong index recommendation.
package parser

import (
	"fmt"
	"sort"
	"strings"
)

// PredicateOp classifies how a column is constrained, which decides where the
// column may appear in a composite index. Equality columns can be placed in
// any order ahead of everything else; at most one range column is useful, and
// only after all the equality columns; ordering columns come last.
type PredicateOp string

const (
	// OpEq is `col = value`, the most useful shape for an index.
	OpEq PredicateOp = "eq"
	// OpIn is `col IN (...)` or `col = ANY($1)`, treated as a weaker equality.
	OpIn PredicateOp = "in"
	// OpRange is <, <=, >, >= or BETWEEN.
	OpRange PredicateOp = "range"
	// OpIsNull is `col IS NULL`.
	OpIsNull PredicateOp = "isnull"
	// OpIsNotNull is `col IS NOT NULL`.
	OpIsNotNull PredicateOp = "isnotnull"
	// OpLikePrefix is `col LIKE 'literal%'`, which a btree index can serve
	// only under the C collation or with a text_pattern_ops operator class.
	OpLikePrefix PredicateOp = "like_prefix"
	// OpContains is a containment or overlap test: the jsonb operators @>, ?,
	// ?& and ?|, and the array operators @> and &&. A btree cannot serve
	// these at all; they need an inverted index.
	OpContains PredicateOp = "contains"
	// OpTextSearch is the full-text match operator @@, which also needs an
	// inverted index.
	OpTextSearch PredicateOp = "textsearch"
	// OpLikeContains is an unanchored pattern such as LIKE '%term%'. A btree
	// is useless for it, but a trigram index can serve it.
	OpLikeContains PredicateOp = "like_contains"
	// OpOther is a constraint we recognise but cannot exploit.
	OpOther PredicateOp = "other"
)

// Usable reports whether a predicate of this kind can drive an index scan.
func (op PredicateOp) Usable() bool {
	switch op {
	case OpEq, OpIn, OpRange, OpIsNull, OpIsNotNull, OpLikePrefix,
		OpContains, OpTextSearch, OpLikeContains:
		return true
	default:
		return false
	}
}

// NeedsInvertedIndex reports whether the predicate can only be served by an
// inverted index such as GIN, rather than by a btree.
func (op PredicateOp) NeedsInvertedIndex() bool {
	switch op {
	case OpContains, OpTextSearch, OpLikeContains:
		return true
	default:
		return false
	}
}

// IsEquality reports whether the predicate pins a column to a single value,
// which is what allows further columns to remain selective behind it.
func (op PredicateOp) IsEquality() bool {
	return op == OpEq || op == OpIn || op == OpIsNull
}

// TableRef is one table appearing in a statement, with the alias it was given
// so predicates can be attributed back to it.
type TableRef struct {
	// Schema is the explicit schema, empty when the query relied on
	// search_path.
	Schema string `json:"schema,omitempty"`
	// Name is the table name as written.
	Name string `json:"name"`
	// Alias is the alias if one was given, otherwise the table name.
	Alias string `json:"alias"`
}

// Qualified renders the table as schema.name, or just name when unqualified.
func (t TableRef) Qualified() string {
	if t.Schema == "" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// Predicate is one column constraint found in a WHERE clause or a join
// condition, already attributed to a specific table.
type Predicate struct {
	// Table is the qualified table the column belongs to, resolved from the
	// alias used in the statement.
	Table string `json:"table"`
	// Column is the column name. Empty when Expr is set.
	Column string `json:"column"`
	// Expr holds the rendered expression when the predicate is on something
	// other than a bare column, such as lower(email). An expression index
	// would be needed to serve it.
	Expr string `json:"expr,omitempty"`
	// Op is how the column is constrained.
	Op PredicateOp `json:"op"`
	// Const is the literal the column was compared against, when the
	// statement contained one rather than a parameter placeholder. Partial
	// index candidates need this.
	Const string `json:"const,omitempty"`
	// Param is true when the comparison was against $1 or similar, which is
	// the normal case for text read from pg_stat_statements.
	Param bool `json:"param"`
	// FromJoin marks predicates that came from a join condition rather than
	// a WHERE clause.
	FromJoin bool `json:"from_join"`
	// FromOr marks predicates that appeared inside an OR. They are recorded
	// for completeness but must not be combined into composite candidates,
	// because a disjunction does not restrict both sides at once.
	FromOr bool `json:"from_or"`
}

// OrderCol is one ORDER BY entry attributed to a table.
type OrderCol struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	// Desc is true for descending order. A btree index can serve either
	// direction by scanning backwards, but a composite ordering that mixes
	// directions needs the index to match.
	Desc bool `json:"desc"`
	// NullsFirst records explicit NULLS FIRST/LAST when given.
	NullsFirst *bool `json:"nulls_first,omitempty"`
}

// Shape is everything the advisor needs to know about one statement.
type Shape struct {
	// SQL is the statement text that produced this shape.
	SQL string `json:"sql"`
	// Command is select, update, delete or insert.
	Command string `json:"command"`
	// Tables lists every table referenced, in order of appearance.
	Tables []TableRef `json:"tables"`
	// Predicates are the column constraints, attributed to tables.
	Predicates []Predicate `json:"predicates"`
	// OrderBy lists ORDER BY entries in order.
	OrderBy []OrderCol `json:"order_by,omitempty"`
	// GroupBy lists GROUP BY columns.
	GroupBy []OrderCol `json:"group_by,omitempty"`
	// HasLimit is true when the statement has a LIMIT, which makes an index
	// that satisfies the ordering far more valuable.
	HasLimit bool `json:"has_limit"`
	// Unparsed records why a statement could not be fully understood, for
	// honest reporting rather than silent omission.
	Unparsed string `json:"unparsed,omitempty"`
}

// TablesWithPredicates returns the qualified names of tables that have at
// least one usable predicate, sorted for deterministic output. Tables without
// predicates cannot benefit from a new index for this statement.
func (s *Shape) TablesWithPredicates() []string {
	seen := map[string]bool{}
	for _, p := range s.Predicates {
		if p.Op.Usable() && p.Table != "" {
			seen[p.Table] = true
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// PredicatesFor returns the predicates belonging to one table. Predicates
// that came from an OR branch are excluded, because combining them into a
// composite index would be unsound.
func (s *Shape) PredicatesFor(table string) []Predicate {
	var out []Predicate
	for _, p := range s.Predicates {
		if p.Table == table && p.Op.Usable() && !p.FromOr {
			out = append(out, p)
		}
	}
	return out
}

// OrderByFor returns the ORDER BY entries that belong entirely to one table.
// An ordering that spans tables cannot be satisfied by a single index, so in
// that case nothing is returned.
func (s *Shape) OrderByFor(table string) []OrderCol {
	if len(s.OrderBy) == 0 {
		return nil
	}
	for _, o := range s.OrderBy {
		if o.Table != table {
			return nil
		}
	}
	return s.OrderBy
}

// String renders a shape compactly for test failure messages.
func (s *Shape) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s tables=[", s.Command)
	for i, t := range s.Tables {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(t.Qualified())
		if t.Alias != t.Name {
			fmt.Fprintf(&b, " AS %s", t.Alias)
		}
	}
	b.WriteString("] preds=[")
	for i, p := range s.Predicates {
		if i > 0 {
			b.WriteString(" ")
		}
		col := p.Column
		if col == "" {
			col = p.Expr
		}
		fmt.Fprintf(&b, "%s.%s %s", p.Table, col, p.Op)
	}
	b.WriteString("]")
	if len(s.OrderBy) > 0 {
		b.WriteString(" order=[")
		for i, o := range s.OrderBy {
			if i > 0 {
				b.WriteString(" ")
			}
			fmt.Fprintf(&b, "%s.%s", o.Table, o.Column)
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
		b.WriteString("]")
	}
	if s.HasLimit {
		b.WriteString(" limit")
	}
	return b.String()
}

// Qualify resolves every table the statement left unqualified, rewriting the
// table references, predicates and ordering columns to schema.name form.
//
// Statements read from pg_stat_statements almost never name a schema; they
// rely on search_path, which the advisor cannot see. Statistics, on the other
// hand, are always keyed by schema and table. Without this step the two never
// meet, and a candidate on a bare table name is silently dropped as
// belonging to no table at all. resolve is given the bare name and returns
// the schema it lives in, or an empty string when it is unknown.
func (s *Shape) Qualify(resolve func(name string) string) {
	if s == nil || resolve == nil {
		return
	}
	rename := map[string]string{}
	for i := range s.Tables {
		t := &s.Tables[i]
		if t.Schema != "" {
			continue
		}
		schema := resolve(t.Name)
		if schema == "" {
			continue
		}
		rename[t.Name] = schema + "." + t.Name
		t.Schema = schema
	}
	if len(rename) == 0 {
		return
	}
	for i := range s.Predicates {
		if q, ok := rename[s.Predicates[i].Table]; ok {
			s.Predicates[i].Table = q
		}
	}
	for i := range s.OrderBy {
		if q, ok := rename[s.OrderBy[i].Table]; ok {
			s.OrderBy[i].Table = q
		}
	}
	for i := range s.GroupBy {
		if q, ok := rename[s.GroupBy[i].Table]; ok {
			s.GroupBy[i].Table = q
		}
	}
}
