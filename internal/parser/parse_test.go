package parser

import (
	"strings"
	"testing"
)

// find returns the predicate for a table and column, or reports failure.
func find(t *testing.T, s *Shape, table, column string) Predicate {
	t.Helper()
	for _, p := range s.Predicates {
		if p.Table == table && p.Column == column {
			return p
		}
	}
	t.Fatalf("no predicate for %s.%s in %s", table, column, s)
	return Predicate{}
}

func mustParse(t *testing.T, sql string) *Shape {
	t.Helper()
	s, err := Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	return s
}

func TestSimpleEquality(t *testing.T) {
	s := mustParse(t, `SELECT * FROM orders WHERE customer_id = $1`)

	if s.Command != "select" {
		t.Errorf("command = %q, want select", s.Command)
	}
	if len(s.Tables) != 1 || s.Tables[0].Name != "orders" {
		t.Fatalf("tables = %v", s.Tables)
	}
	p := find(t, s, "orders", "customer_id")
	if p.Op != OpEq {
		t.Errorf("op = %q, want eq", p.Op)
	}
	if !p.Param {
		t.Error("expected the comparison to be recognised as a parameter")
	}
}

func TestSchemaQualifiedAndAlias(t *testing.T) {
	// The alias must resolve back to the real, schema-qualified table, or
	// every predicate would be attributed to the wrong relation.
	s := mustParse(t, `SELECT o.id FROM shop.orders o WHERE o.status = 'pending'`)

	if got := s.Tables[0].Qualified(); got != "shop.orders" {
		t.Errorf("qualified = %q, want shop.orders", got)
	}
	p := find(t, s, "shop.orders", "status")
	if p.Op != OpEq {
		t.Errorf("op = %q, want eq", p.Op)
	}
	if p.Const != "pending" {
		t.Errorf("const = %q, want pending", p.Const)
	}
	if p.Param {
		t.Error("a literal must not be reported as a parameter")
	}
}

func TestLiteralIsCapturedForPartialIndexes(t *testing.T) {
	// Partial index candidates depend on knowing the constant, so a literal
	// must survive parsing rather than being normalised away.
	s := mustParse(t, `SELECT * FROM orders WHERE status = 'pending' AND total > 100`)

	if got := find(t, s, "orders", "status").Const; got != "pending" {
		t.Errorf("status const = %q, want pending", got)
	}
	if got := find(t, s, "orders", "total").Const; got != "100" {
		t.Errorf("total const = %q, want 100", got)
	}
}

func TestRangeAndBetween(t *testing.T) {
	s := mustParse(t, `SELECT * FROM events WHERE occurred_at > $1 AND id BETWEEN $2 AND $3`)

	if got := find(t, s, "events", "occurred_at").Op; got != OpRange {
		t.Errorf("occurred_at op = %q, want range", got)
	}
	if got := find(t, s, "events", "id").Op; got != OpRange {
		t.Errorf("BETWEEN op = %q, want range", got)
	}
}

func TestNullTests(t *testing.T) {
	s := mustParse(t, `SELECT * FROM shipments WHERE delivered_at IS NULL AND tracking_no IS NOT NULL`)

	if got := find(t, s, "shipments", "delivered_at").Op; got != OpIsNull {
		t.Errorf("op = %q, want isnull", got)
	}
	if got := find(t, s, "shipments", "tracking_no").Op; got != OpIsNotNull {
		t.Errorf("op = %q, want isnotnull", got)
	}
}

func TestInList(t *testing.T) {
	s := mustParse(t, `SELECT * FROM orders WHERE status IN ('pending', 'paid')`)
	if got := find(t, s, "orders", "status").Op; got != OpIn {
		t.Errorf("op = %q, want in", got)
	}
}

func TestNotEqualIsNotIndexable(t *testing.T) {
	// Recommending an index for an inequality would be actively wrong: it
	// matches most of the table.
	s := mustParse(t, `SELECT * FROM orders WHERE status <> 'delivered'`)
	if got := find(t, s, "orders", "status").Op; got != OpOther {
		t.Errorf("op = %q, want other for <>", got)
	}
	if got := find(t, s, "orders", "status").Op.Usable(); got {
		t.Error("an inequality must not be treated as usable")
	}
}

func TestLikePrefixOnly(t *testing.T) {
	// Only an anchored pattern can use a btree index.
	prefix := mustParse(t, `SELECT * FROM products WHERE sku LIKE 'ABC%'`)
	if got := find(t, prefix, "products", "sku").Op; got != OpLikePrefix {
		t.Errorf("anchored LIKE op = %q, want like_prefix", got)
	}
	if got := find(t, prefix, "products", "sku").Const; got != "ABC" {
		t.Errorf("prefix const = %q, want ABC", got)
	}

	// An unanchored pattern cannot use a btree, but a trigram index serves it
	// well, so it is classified separately rather than discarded.
	infix := mustParse(t, `SELECT * FROM products WHERE sku LIKE '%ABC%'`)
	got := find(t, infix, "products", "sku").Op
	if got != OpLikeContains {
		t.Errorf("leading-wildcard LIKE op = %q, want like_contains", got)
	}
	if !got.NeedsInvertedIndex() {
		t.Error("an unanchored pattern needs an inverted index, not a btree")
	}
}

func TestContainmentAndTextSearchOperators(t *testing.T) {
	// These cannot be served by a btree at all.
	cases := map[string]PredicateOp{
		`SELECT * FROM events WHERE payload @> $1`:     OpContains,
		`SELECT * FROM events WHERE tags && $1`:        OpContains,
		`SELECT * FROM events WHERE payload ? $1`:      OpContains,
		`SELECT * FROM docs WHERE search_vector @@ $1`: OpTextSearch,
	}
	for sql, want := range cases {
		s := mustParse(t, sql)
		if len(s.Predicates) == 0 {
			t.Errorf("%s: no predicate found", sql)
			continue
		}
		if got := s.Predicates[0].Op; got != want {
			t.Errorf("%s: op = %q, want %q", sql, got, want)
		}
		if !want.NeedsInvertedIndex() {
			t.Errorf("%s: %q should require an inverted index", sql, want)
		}
	}
}

func TestJoinProducesPredicatesOnBothSides(t *testing.T) {
	// Either side of an equi-join may be the one needing an index, so both
	// must be recorded.
	s := mustParse(t, `
		SELECT oi.id
		FROM order_items oi
		JOIN products p ON p.id = oi.product_id
		WHERE oi.order_id = $1`)

	left := find(t, s, "products", "id")
	right := find(t, s, "order_items", "product_id")
	if !left.FromJoin || !right.FromJoin {
		t.Error("join columns must be marked as coming from a join")
	}
	if left.Op != OpEq || right.Op != OpEq {
		t.Errorf("join predicates should be equality, got %q and %q", left.Op, right.Op)
	}
	if got := find(t, s, "order_items", "order_id"); got.FromJoin {
		t.Error("a WHERE predicate must not be marked as a join predicate")
	}
}

func TestOrderByWithDirectionAndLimit(t *testing.T) {
	s := mustParse(t, `SELECT * FROM orders WHERE customer_id = $1 ORDER BY created_at DESC LIMIT 20`)

	if len(s.OrderBy) != 1 {
		t.Fatalf("order by = %v", s.OrderBy)
	}
	if s.OrderBy[0].Column != "created_at" || !s.OrderBy[0].Desc {
		t.Errorf("order by = %+v, want created_at DESC", s.OrderBy[0])
	}
	if !s.HasLimit {
		t.Error("LIMIT should be recorded: it makes an ordering index far more valuable")
	}
	if got := s.OrderByFor("orders"); len(got) != 1 {
		t.Errorf("OrderByFor = %v, want one entry", got)
	}
}

func TestOrderBySpanningTablesIsRejected(t *testing.T) {
	// No single index can satisfy an ordering that spans two tables.
	s := mustParse(t, `
		SELECT * FROM orders o JOIN customers c ON c.id = o.customer_id
		ORDER BY o.created_at, c.name`)
	if got := s.OrderByFor("orders"); got != nil {
		t.Errorf("OrderByFor across tables = %v, want nil", got)
	}
}

func TestPredicatesUnderOrAreFlagged(t *testing.T) {
	// Columns joined by OR must not be merged into one composite index,
	// because a disjunction does not restrict both at once.
	s := mustParse(t, `SELECT * FROM orders WHERE customer_id = $1 OR status = $2`)

	for _, p := range s.Predicates {
		if !p.FromOr {
			t.Errorf("predicate %s.%s should be flagged as coming from OR", p.Table, p.Column)
		}
	}
	if got := s.PredicatesFor("orders"); len(got) != 0 {
		t.Errorf("PredicatesFor returned %d OR predicates, want none", len(got))
	}
}

func TestNegationIsIgnored(t *testing.T) {
	s := mustParse(t, `SELECT * FROM orders WHERE NOT (status = 'x')`)
	if len(s.Predicates) != 0 {
		t.Errorf("predicates under NOT = %v, want none", s.Predicates)
	}
}

func TestCastedColumnStillCounts(t *testing.T) {
	s := mustParse(t, `SELECT * FROM events WHERE customer_id::bigint = $1`)
	if got := find(t, s, "events", "customer_id").Op; got != OpEq {
		t.Errorf("op = %q, want eq through the cast", got)
	}
}

func TestFunctionCallBecomesExpression(t *testing.T) {
	// lower(email) needs an expression index, so it must be recorded as an
	// expression rather than silently attributed to the bare column.
	s := mustParse(t, `SELECT * FROM customers WHERE lower(email) = $1`)

	if len(s.Predicates) != 1 {
		t.Fatalf("predicates = %v", s.Predicates)
	}
	p := s.Predicates[0]
	if p.Expr != "lower(email)" {
		t.Errorf("expr = %q, want lower(email)", p.Expr)
	}
	if p.Column != "" {
		t.Errorf("column = %q, want empty for an expression predicate", p.Column)
	}
}

func TestSubqueryPredicatesAreCollected(t *testing.T) {
	s := mustParse(t, `
		SELECT * FROM orders
		WHERE customer_id IN (SELECT id FROM customers WHERE country = $1)`)

	var sawCountry bool
	for _, p := range s.Predicates {
		if p.Column == "country" {
			sawCountry = true
		}
	}
	if !sawCountry {
		t.Errorf("predicate inside the subquery was lost: %s", s)
	}
}

func TestCTEPredicatesAreCollected(t *testing.T) {
	s := mustParse(t, `
		WITH recent AS (SELECT * FROM events WHERE occurred_at > $1)
		SELECT * FROM recent`)

	var sawOccurred bool
	for _, p := range s.Predicates {
		if p.Column == "occurred_at" {
			sawOccurred = true
		}
	}
	if !sawOccurred {
		t.Errorf("predicate inside the CTE was lost: %s", s)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	// An UPDATE or DELETE scans to find its rows just like a SELECT, so its
	// WHERE clause deserves an index too.
	u := mustParse(t, `UPDATE orders SET status = 'paid' WHERE id = $1`)
	if u.Command != "update" {
		t.Errorf("command = %q, want update", u.Command)
	}
	if got := find(t, u, "orders", "id").Op; got != OpEq {
		t.Errorf("update predicate op = %q", got)
	}

	d := mustParse(t, `DELETE FROM events WHERE occurred_at < $1`)
	if d.Command != "delete" {
		t.Errorf("command = %q, want delete", d.Command)
	}
	if got := find(t, d, "events", "occurred_at").Op; got != OpRange {
		t.Errorf("delete predicate op = %q", got)
	}
}

func TestInsertProducesNoCandidates(t *testing.T) {
	s := mustParse(t, `INSERT INTO orders (customer_id) VALUES ($1)`)
	if s.Command != "insert" {
		t.Errorf("command = %q, want insert", s.Command)
	}
	if len(s.Predicates) != 0 {
		t.Errorf("insert produced predicates: %v", s.Predicates)
	}
	if s.Unparsed == "" {
		t.Error("expected an explanation for why an insert yields nothing")
	}
}

func TestDDLIsReportedNotFailed(t *testing.T) {
	// A workload contains all sorts of statements. Anything unusable must be
	// explained rather than crashing the run.
	s := mustParse(t, `CREATE TABLE t (id int)`)
	if s.Unparsed == "" {
		t.Error("expected DDL to be reported as unusable")
	}
	if len(s.Predicates) != 0 {
		t.Errorf("DDL produced predicates: %v", s.Predicates)
	}
}

func TestInvalidSQLReturnsError(t *testing.T) {
	if _, err := Parse(`SELECT FROM WHERE`); err == nil {
		t.Error("expected an error for invalid SQL")
	}
}

func TestTablesWithPredicatesIsSortedAndFiltered(t *testing.T) {
	s := mustParse(t, `
		SELECT * FROM orders o
		JOIN customers c ON c.id = o.customer_id
		JOIN products p ON true
		WHERE o.status = $1`)

	got := s.TablesWithPredicates()
	// products has no usable predicate, so it must not appear.
	for _, name := range got {
		if name == "products" {
			t.Errorf("products has no predicates but appeared in %v", got)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("TablesWithPredicates not sorted: %v", got)
		}
	}
}

func TestNormalizeAndFingerprint(t *testing.T) {
	norm, err := Normalize(`SELECT * FROM orders WHERE id = 42`)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if strings.Contains(norm, "42") {
		t.Errorf("Normalize left the literal in place: %q", norm)
	}

	// Two statements differing only in literals must fingerprint the same,
	// which is what lets sampled queries be grouped without
	// pg_stat_statements.
	a, err := Fingerprint(`SELECT * FROM orders WHERE id = 1`)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	b, err := Fingerprint(`SELECT * FROM orders WHERE id = 999`)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if a != b {
		t.Errorf("fingerprints differ for the same shape: %s vs %s", a, b)
	}

	c, err := Fingerprint(`SELECT * FROM customers WHERE id = 1`)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if a == c {
		t.Error("different tables must not share a fingerprint")
	}
}

func TestPredicateOpClassification(t *testing.T) {
	// The distinction drives composite column ordering: equality columns may
	// be followed by more selective columns, a range column may not.
	for _, tc := range []struct {
		op       PredicateOp
		usable   bool
		equality bool
	}{
		{OpEq, true, true},
		{OpIn, true, true},
		{OpIsNull, true, true},
		{OpRange, true, false},
		{OpLikePrefix, true, false},
		{OpIsNotNull, true, false},
		{OpOther, false, false},
	} {
		if got := tc.op.Usable(); got != tc.usable {
			t.Errorf("%s.Usable() = %v, want %v", tc.op, got, tc.usable)
		}
		if got := tc.op.IsEquality(); got != tc.equality {
			t.Errorf("%s.IsEquality() = %v, want %v", tc.op, got, tc.equality)
		}
	}
}

// TestRealWorkloadQueries runs the parser over the statements the bundled
// fixture actually issues, since those are the ones the end-to-end test
// depends on being understood correctly.
func TestRealWorkloadQueries(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		table    string
		wantCols []string
	}{
		{
			name:     "orders by customer, newest first",
			sql:      `SELECT id, total, created_at FROM shop.orders WHERE customer_id = $1 ORDER BY created_at DESC LIMIT 20`,
			table:    "shop.orders",
			wantCols: []string{"customer_id"},
		},
		{
			name:     "event feed",
			sql:      `SELECT * FROM shop.events WHERE customer_id = $1 AND event_type = $2 AND occurred_at > $3 ORDER BY occurred_at DESC LIMIT 50`,
			table:    "shop.events",
			wantCols: []string{"customer_id", "event_type", "occurred_at"},
		},
		{
			name:     "lookup by sku",
			sql:      `SELECT id, name, price FROM shop.products WHERE sku = $1`,
			table:    "shop.products",
			wantCols: []string{"sku"},
		},
		{
			name:     "pending orders",
			sql:      `SELECT id FROM shop.orders WHERE status = 'pending' ORDER BY created_at LIMIT 100`,
			table:    "shop.orders",
			wantCols: []string{"status"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustParse(t, tc.sql)
			for _, col := range tc.wantCols {
				p := find(t, s, tc.table, col)
				if !p.Op.Usable() {
					t.Errorf("%s.%s parsed as unusable op %q", tc.table, col, p.Op)
				}
			}
		})
	}
}
