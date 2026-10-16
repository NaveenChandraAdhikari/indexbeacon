// Package candidates turns parsed query shapes into proposed indexes.
//
// The rules encode how a btree index is actually used by the planner: leading
// equality columns narrow the scan, at most one range column can follow and
// still be useful, and ordering columns come last so the index can supply the
// sort order. Getting that order wrong produces an index the planner ignores,
// which is worse than no recommendation because it costs disk and write time.
package candidates

import (
	"fmt"
	"sort"
	"strings"
)

// Column is one key column of a proposed index.
type Column struct {
	// Opclass is a non-default operator class, when the type and access
	// method need one.
	Opclass Opclass `json:"opclass,omitempty"`
	// Name is the column name, or an expression such as lower(email).
	Name string `json:"name"`
	// Desc requests descending order, used when a query's ORDER BY is
	// descending and the index is meant to supply that ordering.
	Desc bool `json:"desc,omitempty"`
	// Expr marks Name as an expression rather than a plain column, which
	// changes how the DDL is rendered.
	Expr bool `json:"expr,omitempty"`
}

// String renders the column as it appears in index DDL.
func (c Column) String() string {
	name := c.Name
	if !c.Expr {
		name = quoteIdent(name)
	}
	if c.Opclass != "" {
		name += " " + string(c.Opclass)
	}
	if c.Desc {
		return name + " DESC"
	}
	return name
}

// QueryRef links a candidate back to a statement that motivated it, so a
// recommendation can explain itself rather than appearing by magic.
type QueryRef struct {
	// Fingerprint identifies the statement shape.
	Fingerprint string `json:"fingerprint"`
	// SQL is the statement text.
	SQL string `json:"sql"`
	// Calls is how often it ran during the statistics window.
	Calls int64 `json:"calls"`
	// TotalTimeMS is the total execution time attributed to it.
	TotalTimeMS float64 `json:"total_time_ms"`
	// Rule records which generation rule produced the candidate from this
	// query, which is useful when a recommendation looks surprising.
	Rule string `json:"rule"`
}

// Candidate is one proposed index.
type Candidate struct {
	// Schema and Table identify the relation.
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// Columns are the key columns in order. Order is significant.
	Columns []Column `json:"columns"`
	// Method is the index type. A btree unless the predicates cannot be
	// served by one.
	Method AccessMethod `json:"method,omitempty"`
	// Reason explains a non-default access method, so a GIN recommendation
	// does not look arbitrary.
	Reason string `json:"reason,omitempty"`
	// Predicate is the WHERE clause of a partial index, empty for a full one.
	Predicate string `json:"predicate,omitempty"`
	// Include lists non-key columns for a covering index.
	Include []string `json:"include,omitempty"`
	// Queries are the statements this candidate would help.
	Queries []QueryRef `json:"queries"`
	// Alternatives records candidates that were merged into this one, so the
	// report can mention that a shorter index would also have worked.
	Alternatives []string `json:"alternatives,omitempty"`
	// Note carries an explanation when something about the candidate needs
	// qualifying, such as an existing index that already covers it.
	Note string `json:"note,omitempty"`
}

// QualifiedTable renders schema.table.
func (c *Candidate) QualifiedTable() string {
	if c.Schema == "" {
		return c.Table
	}
	return c.Schema + "." + c.Table
}

// Key returns a stable identity for deduplication: same table, same columns
// in the same order, same partial predicate.
func (c *Candidate) Key() string {
	var b strings.Builder
	b.WriteString(c.QualifiedTable())
	if m := c.method(); m != MethodBTree {
		b.WriteString(" ")
		b.WriteString(string(m))
	}
	b.WriteString("(")
	for i, col := range c.Columns {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(col.Name)
		if col.Desc {
			b.WriteString(" desc")
		}
	}
	b.WriteString(")")
	if c.Predicate != "" {
		b.WriteString(" where ")
		b.WriteString(c.Predicate)
	}
	return b.String()
}

// ColumnNames returns just the key column names, in order.
func (c *Candidate) ColumnNames() []string {
	out := make([]string, len(c.Columns))
	for i, col := range c.Columns {
		out[i] = col.Name
	}
	return out
}

// IndexName builds a deterministic index name within PostgreSQL's 63 byte
// identifier limit, falling back to a hash suffix when the natural name is
// too long.
func (c *Candidate) IndexName() string {
	parts := []string{c.Table}
	for _, col := range c.Columns {
		parts = append(parts, sanitizeIdent(col.Name))
	}
	if c.Predicate != "" {
		parts = append(parts, "partial")
	}
	name := strings.Join(parts, "_") + "_idx"
	if len(name) <= 63 {
		return name
	}

	// Too long: keep the table name and a hash of the full key so the name
	// stays stable and collision-resistant.
	sum := fnv1a(c.Key())
	suffix := fmt.Sprintf("_%08x_idx", sum)
	keep := 63 - len(suffix)
	base := c.Table
	if len(base) > keep {
		base = base[:keep]
	}
	return base + suffix
}

// DDL renders the CREATE INDEX statement. It always uses CONCURRENTLY, since
// a plain CREATE INDEX takes a lock that blocks writes for the duration, and
// IF NOT EXISTS so re-running a generated script is safe.
func (c *Candidate) DDL() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE INDEX CONCURRENTLY IF NOT EXISTS %s\n  ON %s USING %s (",
		c.IndexName(), c.qualifiedQuoted(), c.method())
	for i, col := range c.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(col.String())
	}
	b.WriteString(")")
	if len(c.Include) > 0 {
		b.WriteString("\n  INCLUDE (")
		for i, col := range c.Include {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteIdent(col))
		}
		b.WriteString(")")
	}
	if c.Predicate != "" {
		fmt.Fprintf(&b, "\n  WHERE %s", c.Predicate)
	}
	b.WriteString(";")
	return b.String()
}

// hypoDDL renders the statement in the form hypopg accepts, which is a plain
// CREATE INDEX with no CONCURRENTLY, no IF NOT EXISTS and no index name.
func (c *Candidate) HypoDDL() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE INDEX ON %s USING %s (", c.qualifiedQuoted(), c.method())
	for i, col := range c.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(col.String())
	}
	b.WriteString(")")
	if c.Predicate != "" {
		fmt.Fprintf(&b, " WHERE %s", c.Predicate)
	}
	return b.String()
}

// MethodName returns the access method, defaulting to btree.
func (c *Candidate) MethodName() AccessMethod { return c.method() }

// method returns the access method, defaulting to btree.
func (c *Candidate) method() AccessMethod {
	if c.Method == "" {
		return MethodBTree
	}
	return c.Method
}

// RequiresExtension names an extension the index needs, so the report can say
// so rather than leaving the user with a statement that fails.
func (c *Candidate) RequiresExtension() string {
	for _, col := range c.Columns {
		if col.Opclass == OpclassTrigram {
			return "pg_trgm"
		}
	}
	return ""
}

func (c *Candidate) qualifiedQuoted() string {
	if c.Schema == "" {
		return quoteIdent(c.Table)
	}
	return quoteIdent(c.Schema) + "." + quoteIdent(c.Table)
}

// IsPrefixOf reports whether c's key columns are a leading prefix of other's,
// with matching sort directions and the same partial predicate. A prefix
// index is redundant: any query the shorter one serves, the longer one
// serves too.
func (c *Candidate) IsPrefixOf(other *Candidate) bool {
	if c.QualifiedTable() != other.QualifiedTable() || c.Predicate != other.Predicate {
		return false
	}
	if len(c.Columns) > len(other.Columns) {
		return false
	}
	for i, col := range c.Columns {
		if col.Name != other.Columns[i].Name || col.Desc != other.Columns[i].Desc {
			return false
		}
	}
	return true
}

// addQuery records a motivating statement, avoiding duplicates.
func (c *Candidate) addQuery(ref QueryRef) {
	for _, q := range c.Queries {
		if q.Fingerprint == ref.Fingerprint {
			return
		}
	}
	c.Queries = append(c.Queries, ref)
}

// Calls totals how often the statements this candidate serves were executed,
// which is the frequency term in the scoring formula.
func (c *Candidate) Calls() int64 {
	var total int64
	for _, q := range c.Queries {
		total += q.Calls
	}
	return total
}

// TotalTimeMS totals the execution time of the statements this candidate
// serves.
func (c *Candidate) TotalTimeMS() float64 {
	var total float64
	for _, q := range c.Queries {
		total += q.TotalTimeMS
	}
	return total
}

// sortCandidates orders candidates deterministically so output does not
// shuffle between runs on equal scores.
func sortCandidates(cands []*Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].QualifiedTable() != cands[j].QualifiedTable() {
			return cands[i].QualifiedTable() < cands[j].QualifiedTable()
		}
		return cands[i].Key() < cands[j].Key()
	})
}

// quoteIdent quotes an identifier only when PostgreSQL would need it to be,
// so ordinary names stay readable in generated DDL.
func quoteIdent(s string) string {
	if s == "" {
		return s
	}
	needsQuote := false
	for i, r := range s {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if !lower && r != '_' && !(digit && i > 0) {
			needsQuote = true
			break
		}
	}
	if !needsQuote && !isReservedWord(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// sanitizeIdent reduces an expression to something usable inside an index
// name.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_':
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "expr"
	}
	return b.String()
}

// fnv1a is a small stable hash used for index name disambiguation.
func fnv1a(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// reserved is the subset of PostgreSQL reserved words likely to appear as a
// column name. Quoting one that does not need it is harmless; failing to
// quote one that does produces invalid DDL.
var reserved = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "authorization": true, "between": true,
	"binary": true, "both": true, "case": true, "cast": true, "check": true,
	"collate": true, "column": true, "constraint": true, "create": true,
	"cross": true, "current_date": true, "current_role": true, "current_time": true,
	"current_timestamp": true, "current_user": true, "default": true,
	"deferrable": true, "desc": true, "distinct": true, "do": true, "else": true,
	"end": true, "except": true, "false": true, "for": true, "foreign": true,
	"freeze": true, "from": true, "full": true, "grant": true, "group": true,
	"having": true, "ilike": true, "in": true, "initially": true, "inner": true,
	"intersect": true, "into": true, "is": true, "isnull": true, "join": true,
	"leading": true, "left": true, "like": true, "limit": true, "localtime": true,
	"localtimestamp": true, "natural": true, "new": true, "not": true,
	"notnull": true, "null": true, "off": true, "offset": true, "old": true,
	"on": true, "only": true, "or": true, "order": true, "outer": true,
	"overlaps": true, "placing": true, "primary": true, "references": true,
	"right": true, "select": true, "session_user": true, "similar": true,
	"some": true, "symmetric": true, "table": true, "then": true, "to": true,
	"trailing": true, "true": true, "union": true, "unique": true, "user": true,
	"using": true, "verbose": true, "when": true, "where": true, "window": true,
	"with": true,
}

func isReservedWord(s string) bool { return reserved[strings.ToLower(s)] }
