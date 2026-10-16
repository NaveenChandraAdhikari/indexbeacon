package candidates

import (
	"strings"
	"testing"

	"github.com/indexbeacon/indexbeacon/internal/parser"
)

func shapeOf(t *testing.T, sql string) *parser.Shape {
	t.Helper()
	s, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return s
}

func gen(t *testing.T, sql string, opts Options) []*Candidate {
	t.Helper()
	return Generate(shapeOf(t, sql), QueryRef{Fingerprint: sql, SQL: sql, Calls: 100}, opts)
}

// keys renders candidates for readable assertions.
func keys(cands []*Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Key()
	}
	return out
}

func hasKey(cands []*Candidate, want string) bool {
	for _, c := range cands {
		if c.Key() == want {
			return true
		}
	}
	return false
}

func TestSingleEqualityCandidate(t *testing.T) {
	got := gen(t, `SELECT * FROM shop.orders WHERE customer_id = $1`, Options{})
	if !hasKey(got, "shop.orders(customer_id)") {
		t.Errorf("candidates = %v, want shop.orders(customer_id)", keys(got))
	}
}

func TestEqualityColumnsPrecedeRange(t *testing.T) {
	// A range column must come after every equality column, otherwise the
	// columns beyond it cannot narrow the scan.
	got := gen(t, `
		SELECT * FROM shop.events
		WHERE customer_id = $1 AND event_type = $2 AND occurred_at > $3`, Options{})

	var found *Candidate
	for _, c := range got {
		if len(c.Columns) == 3 {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("no three-column candidate in %v", keys(got))
	}
	if last := found.Columns[2].Name; last != "occurred_at" {
		t.Errorf("range column is at position %v, want last: %v", last, found.ColumnNames())
	}
	for _, c := range found.Columns[:2] {
		if c.Name == "occurred_at" {
			t.Errorf("range column appears before an equality column: %v", found.ColumnNames())
		}
	}
}

func TestEqualityPlusOrderByForLimitQueries(t *testing.T) {
	// The "latest N for this key" pattern. The index must carry the sort
	// column so the planner can stop after the limit.
	got := gen(t, `
		SELECT * FROM shop.orders
		WHERE customer_id = $1 ORDER BY created_at DESC LIMIT 20`, Options{})

	if !hasKey(got, "shop.orders(customer_id,created_at desc)") {
		t.Errorf("candidates = %v, want the ordering column included with DESC", keys(got))
	}
}

func TestDescendingDirectionIsPreserved(t *testing.T) {
	got := gen(t, `SELECT * FROM t WHERE a = $1 ORDER BY b DESC`, Options{})
	for _, c := range got {
		for _, col := range c.Columns {
			if col.Name == "b" && !col.Desc {
				t.Errorf("descending order lost in %s", c.Key())
			}
		}
	}
}

func TestSelectivityOrdersEqualityColumns(t *testing.T) {
	// The more selective column belongs first: it eliminates more rows
	// immediately, and the resulting prefix serves more queries.
	opts := Options{Selectivity: func(table, col string) (float64, bool) {
		switch col {
		case "customer_id":
			return 0.00005, true // 20k distinct customers
		case "event_type":
			return 0.2, true // five event types
		}
		return 0, false
	}}
	got := Generate(shapeOf(t, `
		SELECT * FROM shop.events WHERE event_type = $1 AND customer_id = $2`),
		QueryRef{Fingerprint: "f"}, opts)

	var composite *Candidate
	for _, c := range got {
		if len(c.Columns) == 2 {
			composite = c
		}
	}
	if composite == nil {
		t.Fatalf("no composite candidate in %v", keys(got))
	}
	if composite.Columns[0].Name != "customer_id" {
		t.Errorf("column order = %v, want the selective column first", composite.ColumnNames())
	}
}

func TestGenerationIsDeterministicWithoutStatistics(t *testing.T) {
	sql := `SELECT * FROM t WHERE b = $1 AND a = $2 AND c = $3`
	first := keys(gen(t, sql, Options{}))
	for i := 0; i < 5; i++ {
		if got := keys(gen(t, sql, Options{})); !equalSlices(got, first) {
			t.Fatalf("run %d differed:\n %v\n %v", i, got, first)
		}
	}
}

func TestJoinKeysProduceCandidatesOnBothSides(t *testing.T) {
	got := gen(t, `
		SELECT * FROM order_items oi
		JOIN products p ON p.id = oi.product_id
		WHERE oi.order_id = $1`, Options{})

	if !hasKey(got, "products(id)") {
		t.Errorf("missing join-key candidate on products: %v", keys(got))
	}
	if !hasKey(got, "order_items(product_id)") {
		t.Errorf("missing join-key candidate on order_items: %v", keys(got))
	}
}

func TestPartialIndexRequiresLiteralAndFlag(t *testing.T) {
	sql := `SELECT * FROM shop.orders WHERE status = 'pending' ORDER BY created_at`

	// Off by default.
	if got := gen(t, sql, Options{}); anyPartial(got) {
		t.Errorf("partial candidate generated without the flag: %v", keys(got))
	}

	got := gen(t, sql, Options{Partial: true})
	var partial *Candidate
	for _, c := range got {
		if c.Predicate != "" {
			partial = c
		}
	}
	if partial == nil {
		t.Fatalf("no partial candidate in %v", keys(got))
	}
	if partial.Predicate != "status = 'pending'" {
		t.Errorf("predicate = %q", partial.Predicate)
	}
	// The pinned column must not also be a key column: it is constant
	// throughout the index.
	for _, c := range partial.Columns {
		if c.Name == "status" {
			t.Errorf("pinned column repeated as a key column: %v", partial.ColumnNames())
		}
	}
}

func TestPartialNotGeneratedForParameters(t *testing.T) {
	// A parameter placeholder gives no constant to build a partial index on.
	got := gen(t, `SELECT * FROM orders WHERE status = $1 ORDER BY created_at`, Options{Partial: true})
	if anyPartial(got) {
		t.Errorf("partial candidate built from a parameter: %v", keys(got))
	}
}

func TestNonIndexablePredicatesProduceNothing(t *testing.T) {
	if got := gen(t, `SELECT * FROM orders WHERE status <> 'x'`, Options{}); len(got) != 0 {
		t.Errorf("inequality produced candidates: %v", keys(got))
	}

	// An unanchored pattern is no longer dismissed: a btree cannot serve it,
	// but a trigram index can, so it produces a GIN candidate instead.
	got := gen(t, `SELECT * FROM orders WHERE name LIKE '%x%'`, Options{})
	if len(got) != 1 || got[0].Method != MethodGIN {
		t.Errorf("unanchored LIKE should propose a GIN index, got %v", keys(got))
	}
}

func TestColumnCountIsCapped(t *testing.T) {
	got := gen(t, `SELECT * FROM t WHERE a=$1 AND b=$2 AND c=$3 AND d=$4 AND e=$5 AND f=$6`,
		Options{MaxColumns: 4})
	for _, c := range got {
		if len(c.Columns) > 4 {
			t.Errorf("candidate %s has %d columns, cap is 4", c.Key(), len(c.Columns))
		}
	}
}

func TestNoDuplicateColumnsWithinACandidate(t *testing.T) {
	// A column filtered and then ordered by must appear once, not twice.
	got := gen(t, `SELECT * FROM t WHERE a = $1 ORDER BY a`, Options{})
	for _, c := range got {
		seen := map[string]bool{}
		for _, col := range c.Columns {
			if seen[col.Name] {
				t.Errorf("candidate %s repeats column %s", c.Key(), col.Name)
			}
			seen[col.Name] = true
		}
	}
}

func TestMergeCollapsesIdenticalCandidates(t *testing.T) {
	a := gen(t, `SELECT * FROM orders WHERE customer_id = $1`, Options{})
	b := Generate(shapeOf(t, `SELECT id FROM orders WHERE customer_id = $2`),
		QueryRef{Fingerprint: "other", Calls: 50}, Options{})

	merged := Merge(append(a, b...), nil)
	var target *Candidate
	for _, c := range merged {
		if c.Key() == "orders(customer_id)" {
			target = c
		}
	}
	if target == nil {
		t.Fatalf("candidate missing after merge: %v", keys(merged))
	}
	if len(target.Queries) != 2 {
		t.Errorf("queries = %d, want both statements recorded", len(target.Queries))
	}
	if got := target.Calls(); got != 150 {
		t.Errorf("calls = %d, want 150 combined", got)
	}
}

func TestMergeAbsorbsPrefixes(t *testing.T) {
	// Recommending both (customer_id) and (customer_id, created_at) wastes
	// disk and write throughput: the longer index serves both queries.
	short := &Candidate{Table: "orders", Columns: []Column{{Name: "customer_id"}},
		Queries: []QueryRef{{Fingerprint: "short", Calls: 10}}}
	long := &Candidate{Table: "orders",
		Columns: []Column{{Name: "customer_id"}, {Name: "created_at"}},
		Queries: []QueryRef{{Fingerprint: "long", Calls: 20}}}

	merged := Merge([]*Candidate{short, long}, nil)
	if len(merged) != 1 {
		t.Fatalf("merge kept %d candidates, want 1: %v", len(merged), keys(merged))
	}
	if merged[0].Key() != "orders(customer_id,created_at)" {
		t.Errorf("kept %s, want the longer index", merged[0].Key())
	}
	if len(merged[0].Queries) != 2 {
		t.Errorf("absorbed candidate's queries were lost: %v", merged[0].Queries)
	}
	if len(merged[0].Alternatives) == 0 {
		t.Error("expected the shorter index to be recorded as an alternative")
	}
}

func TestMergeKeepsIndependentCandidates(t *testing.T) {
	// Same table, different leading column: neither is redundant.
	a := &Candidate{Table: "orders", Columns: []Column{{Name: "customer_id"}}}
	b := &Candidate{Table: "orders", Columns: []Column{{Name: "status"}}}
	if got := Merge([]*Candidate{a, b}, nil); len(got) != 2 {
		t.Errorf("merged unrelated candidates: %v", keys(got))
	}
}

func TestMergeDropsCandidatesAlreadyIndexed(t *testing.T) {
	c := &Candidate{Schema: "shop", Table: "orders", Columns: []Column{{Name: "customer_id"}}}
	existing := []ExistingIndex{{
		Schema: "shop", Table: "orders", Name: "orders_customer_id_idx",
		Columns: []string{"customer_id", "created_at"}, Valid: true, Scans: 500,
	}}

	if got := Merge([]*Candidate{c}, existing); len(got) != 0 {
		t.Errorf("candidate survived despite an existing covering index: %v", keys(got))
	}
}

func TestExistingButUnusedIndexIsReportedNotDropped(t *testing.T) {
	// The valuable finding here is not "create an index" but "you already
	// have one and the planner is not using it".
	c := &Candidate{Schema: "shop", Table: "orders", Columns: []Column{{Name: "customer_id"}}}
	existing := []ExistingIndex{{
		Schema: "shop", Table: "orders", Name: "orders_customer_id_idx",
		Columns: []string{"customer_id"}, Valid: true, Scans: 0,
	}}

	got := Merge([]*Candidate{c}, existing)
	if len(got) != 1 {
		t.Fatalf("expected the candidate to be kept with a note, got %v", keys(got))
	}
	if !strings.Contains(got[0].Note, "never been used") {
		t.Errorf("note = %q, want an explanation of the unused index", got[0].Note)
	}
}

func TestInvalidIndexDoesNotCover(t *testing.T) {
	// An invalid index is left behind by a failed concurrent build and the
	// planner ignores it, so it must not suppress a recommendation.
	c := &Candidate{Table: "orders", Columns: []Column{{Name: "customer_id"}}}
	existing := []ExistingIndex{{
		Table: "orders", Name: "broken", Columns: []string{"customer_id"},
		Valid: false, Scans: 0,
	}}
	if got := Merge([]*Candidate{c}, existing); len(got) != 1 {
		t.Errorf("an invalid index suppressed a needed recommendation: %v", keys(got))
	}
}

func TestPartialIndexDoesNotCoverFullCandidate(t *testing.T) {
	c := &Candidate{Table: "orders", Columns: []Column{{Name: "customer_id"}}}
	existing := []ExistingIndex{{
		Table: "orders", Name: "partial_idx", Columns: []string{"customer_id"},
		Predicate: "status = 'pending'", Valid: true, Scans: 10,
	}}
	if got := Merge([]*Candidate{c}, existing); len(got) != 1 {
		t.Errorf("a partial index wrongly covered a full candidate: %v", keys(got))
	}
}

func TestDDLIsValidAndSafe(t *testing.T) {
	c := &Candidate{Schema: "shop", Table: "orders",
		Columns:   []Column{{Name: "customer_id"}, {Name: "created_at", Desc: true}},
		Predicate: "status = 'pending'"}

	ddl := c.DDL()
	for _, want := range []string{
		"CREATE INDEX CONCURRENTLY", // must never lock out writes
		"IF NOT EXISTS",             // re-running a script must be safe
		"shop.orders",
		"customer_id",
		"created_at DESC",
		"WHERE status = 'pending'",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL missing %q:\n%s", want, ddl)
		}
	}
}

func TestHypoDDLOmitsConcurrently(t *testing.T) {
	// hypopg rejects CONCURRENTLY and IF NOT EXISTS.
	c := &Candidate{Table: "orders", Columns: []Column{{Name: "customer_id"}}}
	ddl := c.HypoDDL()
	if strings.Contains(ddl, "CONCURRENTLY") || strings.Contains(ddl, "IF NOT EXISTS") {
		t.Errorf("hypopg DDL must be a plain CREATE INDEX: %s", ddl)
	}
}

func TestReservedWordsAreQuoted(t *testing.T) {
	c := &Candidate{Table: "orders", Columns: []Column{{Name: "order"}, {Name: "user"}}}
	ddl := c.DDL()
	if !strings.Contains(ddl, `"order"`) || !strings.Contains(ddl, `"user"`) {
		t.Errorf("reserved words must be quoted or the DDL is invalid:\n%s", ddl)
	}
}

func TestIndexNameFitsIdentifierLimit(t *testing.T) {
	long := strings.Repeat("a", 40)
	c := &Candidate{Table: long, Columns: []Column{{Name: long}, {Name: long}}}
	name := c.IndexName()
	if len(name) > 63 {
		t.Errorf("index name is %d bytes, PostgreSQL truncates at 63: %s", len(name), name)
	}
	// The name must stay stable across runs.
	if name != c.IndexName() {
		t.Error("index name is not deterministic")
	}
}

func TestIndexNamesDifferForDifferentCandidates(t *testing.T) {
	a := &Candidate{Table: "orders", Columns: []Column{{Name: "customer_id"}}}
	b := &Candidate{Table: "orders", Columns: []Column{{Name: "status"}}}
	if a.IndexName() == b.IndexName() {
		t.Errorf("different candidates share the index name %s", a.IndexName())
	}
}

func TestIsPrefixOf(t *testing.T) {
	short := &Candidate{Table: "t", Columns: []Column{{Name: "a"}}}
	long := &Candidate{Table: "t", Columns: []Column{{Name: "a"}, {Name: "b"}}}
	other := &Candidate{Table: "t", Columns: []Column{{Name: "b"}, {Name: "a"}}}

	if !short.IsPrefixOf(long) {
		t.Error("(a) should be a prefix of (a, b)")
	}
	if long.IsPrefixOf(short) {
		t.Error("(a, b) is not a prefix of (a)")
	}
	if short.IsPrefixOf(other) {
		t.Error("(a) is not a prefix of (b, a): leading column differs")
	}

	partial := &Candidate{Table: "t", Columns: []Column{{Name: "a"}}, Predicate: "x"}
	if partial.IsPrefixOf(long) {
		t.Error("a partial index must not be treated as a prefix of a full one")
	}
}

func anyPartial(cands []*Candidate) bool {
	for _, c := range cands {
		if c.Predicate != "" {
			return true
		}
	}
	return false
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
