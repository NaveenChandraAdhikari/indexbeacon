package candidates

import (
	"strings"

	"github.com/indexbeacon/indexbeacon/internal/parser"
)

// AccessMethod is the index type. Choosing it correctly matters as much as
// choosing the columns: a btree on a jsonb column filtered with @> is not
// merely suboptimal, the planner cannot use it at all, so the index would sit
// there costing disk and write throughput and never be read.
type AccessMethod string

const (
	// MethodBTree serves equality, ranges and ordering. The default, and the
	// right answer for the great majority of queries.
	MethodBTree AccessMethod = "btree"

	// MethodGIN serves containment and overlap on jsonb and arrays, full text
	// search on tsvector, and, with the pg_trgm extension, unanchored LIKE
	// patterns. It is slower to update than a btree, which is why it is only
	// proposed when a btree genuinely cannot do the job.
	MethodGIN AccessMethod = "gin"

	// MethodBRIN stores one summary per range of pages rather than one entry
	// per row, so it is a tiny fraction of the size of a btree. It only works
	// when the column's values track physical order, which in practice means
	// an append-only table with a timestamp or sequential key. Where that
	// holds it is dramatically cheaper; where it does not, it is useless.
	MethodBRIN AccessMethod = "brin"
)

// CanValidate reports whether hypopg can create a hypothetical index of this
// type.
//
// hypopg supports btree, brin and hash, and refuses gin, gist and spgist with
// "access method is not supported". That distinction matters: without it, a
// GIN recommendation gets reported as one the planner rejected, when in fact
// it was never offered to the planner at all. Telling a user their index is
// useless when it has not been tested is worse than admitting it is unproven.
func (m AccessMethod) CanValidate() bool {
	switch m {
	case MethodBTree, MethodBRIN, "":
		return true
	default:
		return false
	}
}

// Opclass is a non-default operator class needed for a particular
// combination of type and access method.
type Opclass string

const (
	// OpclassJSONBPathOps indexes only the paths and values that containment
	// queries need. It is markedly smaller and faster than the default jsonb
	// operator class, at the cost of not supporting key-existence operators.
	OpclassJSONBPathOps Opclass = "jsonb_path_ops"

	// OpclassTrigram supports unanchored LIKE and ILIKE, and requires the
	// pg_trgm extension.
	OpclassTrigram Opclass = "gin_trgm_ops"

	// OpclassTextPattern lets a btree serve anchored LIKE under a collation
	// other than C, which is otherwise impossible.
	OpclassTextPattern Opclass = "text_pattern_ops"
)

// ColumnType is what the advisor knows about a column's type, which is what
// decides the access method.
type ColumnType struct {
	// Name is the type as PostgreSQL renders it, such as "jsonb" or
	// "timestamp with time zone".
	Name string
	// Correlation is the physical ordering correlation from the statistics,
	// between -1 and 1. A value near either extreme is what makes BRIN
	// viable.
	Correlation float64
	// Known is false when the column has never been analyzed.
	Known bool
}

// IsJSON reports whether the type is jsonb, the only JSON type that can be
// indexed for containment.
func (c ColumnType) IsJSON() bool {
	return strings.EqualFold(c.Name, "jsonb")
}

// IsArray reports whether the type is an array.
func (c ColumnType) IsArray() bool {
	return strings.HasSuffix(c.Name, "[]")
}

// IsTextSearch reports whether the type is tsvector.
func (c ColumnType) IsTextSearch() bool {
	return strings.EqualFold(c.Name, "tsvector")
}

// IsText reports whether the type holds text, which is what a trigram index
// applies to.
func (c ColumnType) IsText() bool {
	switch strings.ToLower(c.Name) {
	case "text", "character varying", "varchar", "character", "char", "citext":
		return true
	}
	return strings.HasPrefix(strings.ToLower(c.Name), "character varying")
}

// IsRangeFriendly reports whether the type is one where physical ordering
// commonly tracks value order: timestamps and sequential integers.
func (c ColumnType) IsRangeFriendly() bool {
	name := strings.ToLower(c.Name)
	switch {
	case strings.HasPrefix(name, "timestamp"), strings.HasPrefix(name, "date"):
		return true
	case name == "bigint", name == "integer", name == "smallint", name == "serial", name == "bigserial":
		return true
	}
	return false
}

// brinMinimumRows is how large a table must be before a BRIN index is worth
// proposing. Below this a btree is small anyway and far more flexible.
const brinMinimumRows = 5_000_000

// brinMinimumCorrelation is how tightly values must track physical order.
// BRIN skips a range of pages only when every value in it is outside the
// query's bounds, so weak correlation makes it read almost the whole table.
const brinMinimumCorrelation = 0.9

// chooseMethod decides the index type for a set of predicates on one column.
//
// The order of the checks reflects necessity rather than preference: the
// inverted cases come first because a btree simply cannot answer them, and
// BRIN comes last because it is an optimisation that only applies in narrow
// circumstances.
func chooseMethod(op parser.PredicateOp, ct ColumnType) (AccessMethod, Opclass, string) {
	switch op {
	case parser.OpContains:
		switch {
		case ct.IsJSON():
			// jsonb_path_ops is smaller and faster for containment, which is
			// the operator that brought us here.
			return MethodGIN, OpclassJSONBPathOps, "jsonb containment cannot be served by a btree"
		case ct.IsArray():
			return MethodGIN, "", "array containment and overlap cannot be served by a btree"
		default:
			return MethodGIN, "", "this operator cannot be served by a btree"
		}

	case parser.OpTextSearch:
		return MethodGIN, "", "full-text search cannot be served by a btree"

	case parser.OpLikeContains:
		if ct.IsText() {
			return MethodGIN, OpclassTrigram,
				"an unanchored pattern cannot use a btree; a trigram index can, and needs the pg_trgm extension"
		}
		return MethodGIN, "", "an unanchored pattern cannot be served by a btree"
	}

	return MethodBTree, "", ""
}

// brinAlternative reports whether a BRIN index would be a worthwhile
// alternative to a proposed btree, and why.
//
// It applies to one narrow but common case: a very large, append-only table
// queried by a time range. There the btree is enormous and the BRIN is a few
// hundred kilobytes, and both answer the query.
func brinAlternative(c *Candidate, rows int64, types map[string]ColumnType) (bool, string) {
	if len(c.Columns) != 1 || c.Predicate != "" {
		// BRIN has no useful notion of a leading column, so it only stands in
		// for a single-column index.
		return false, ""
	}
	if rows < brinMinimumRows {
		return false, ""
	}

	ct, ok := types[c.Columns[0].Name]
	if !ok || !ct.Known || !ct.IsRangeFriendly() {
		return false, ""
	}
	if abs(ct.Correlation) < brinMinimumCorrelation {
		return false, ""
	}

	return true, "this column's values track physical order closely on a large table, " +
		"so a BRIN index would answer range queries at a small fraction of the size, " +
		"though it cannot serve equality lookups or ordering as well as a btree"
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
