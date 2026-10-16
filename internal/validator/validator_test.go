package validator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/candidates"
)

// fakeQuerier stands in for a database connection, returning canned plans.
type fakeQuerier struct {
	// rows maps a SQL substring to the value the single-column row returns.
	rows map[string]string
	// seen records every statement issued, so tests can assert on them.
	seen []string
	// execErr forces Exec to fail.
	execErr error
	// createOID is the identifier handed back by hypopg_create_index.
	createOID int64
	// planAfter, when set, is returned for explains issued after a
	// hypothetical index has been created.
	planAfter    string
	hypoCreated  int
	hypoDropped  int
	resets       int
	failCreateOn string
}

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > len(r.vals) {
		return errors.New("not enough values")
	}
	for i, d := range dest {
		switch t := d.(type) {
		case *string:
			s, _ := r.vals[i].(string)
			*t = s
		case *int64:
			switch v := r.vals[i].(type) {
			case int64:
				*t = v
			case int:
				*t = int64(v)
			}
		case *int:
			switch v := r.vals[i].(type) {
			case int:
				*t = v
			case int64:
				*t = int(v)
			}
		}
	}
	return nil
}

func (f *fakeQuerier) QueryRow(ctx context.Context, sql string, args ...any) Row {
	f.seen = append(f.seen, sql)

	switch {
	case strings.Contains(sql, "hypopg_create_index"):
		if f.failCreateOn != "" && len(args) > 0 {
			if ddl, ok := args[0].(string); ok && strings.Contains(ddl, f.failCreateOn) {
				return fakeRow{err: errors.New("hypopg refused the index")}
			}
		}
		f.hypoCreated++
		f.createOID++
		return fakeRow{vals: []any{f.createOID, "<13625>btree_fake"}}
	case strings.Contains(sql, "hypopg_relation_size"):
		return fakeRow{vals: []any{int64(4096)}}
	case strings.Contains(sql, "hypopg_list_indexes"):
		return fakeRow{vals: []any{f.hypoCreated - f.hypoDropped}}
	case strings.HasPrefix(sql, "EXPLAIN"):
		if f.hypoCreated > f.hypoDropped && f.planAfter != "" {
			return fakeRow{vals: []any{f.planAfter}}
		}
		for frag, plan := range f.rows {
			if strings.Contains(sql, frag) {
				return fakeRow{vals: []any{plan}}
			}
		}
		return fakeRow{vals: []any{planJSON(1000, "Seq Scan", "")}}
	}
	return fakeRow{err: errors.New("unexpected query: " + sql)}
}

func (f *fakeQuerier) Exec(ctx context.Context, sql string, args ...any) error {
	f.seen = append(f.seen, sql)
	if f.execErr != nil {
		return f.execErr
	}
	if strings.Contains(sql, "hypopg_reset") {
		f.resets++
		f.hypoDropped = f.hypoCreated
	}
	if strings.Contains(sql, "hypopg_drop_index") {
		f.hypoDropped++
	}
	return nil
}

// planJSON builds an EXPLAIN result with a given cost, node type and index.
func planJSON(cost float64, nodeType, indexName string) string {
	idx := ""
	if indexName != "" {
		idx = `, "Index Name": "` + indexName + `"`
	}
	return `[{"Plan": {"Node Type": "` + nodeType + `", "Total Cost": ` + ftoa(cost) + idx + `}}]`
}

func ftoa(f float64) string {
	s := ""
	whole := int(f)
	if whole == 0 {
		s = "0"
	}
	for n := whole; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s + ".00"
}

func candidate() *candidates.Candidate {
	return &candidates.Candidate{
		Schema: "shop", Table: "orders",
		Columns: []candidates.Column{{Name: "customer_id"}},
		Queries: []candidates.QueryRef{{
			Fingerprint: "f1",
			SQL:         "SELECT * FROM shop.orders WHERE customer_id = $1",
			Calls:       100,
		}},
	}
}

func TestParsePlanExtractsCostAndIndex(t *testing.T) {
	p, err := parsePlan(planJSON(1234, "Index Scan", "orders_pkey"))
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if p.TotalCost != 1234 {
		t.Errorf("cost = %v, want 1234", p.TotalCost)
	}
	if !p.UsesIndex("orders_pkey") {
		t.Errorf("index not detected: %v", p.Indexes)
	}
	if p.HasSeqScan() {
		t.Error("an index scan plan reported a sequential scan")
	}
}

func TestParsePlanFindsIndexNestedInSubplan(t *testing.T) {
	// An index can appear well below the top node. A shallow check would
	// report a working recommendation as unused.
	nested := `[{"Plan": {
		"Node Type": "Aggregate", "Total Cost": 500.00,
		"Plans": [
			{"Node Type": "Nested Loop", "Total Cost": 400.00,
			 "Plans": [
				{"Node Type": "Seq Scan", "Total Cost": 100.00},
				{"Node Type": "Index Scan", "Total Cost": 8.00, "Index Name": "<13625>btree_orders_customer_id"}
			 ]}
		]}}]`

	p, err := parsePlan(nested)
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if !p.UsesHypotheticalIndex() {
		t.Errorf("nested hypothetical index missed: %v", p.Indexes)
	}
	if !p.HasSeqScan() {
		t.Error("expected the nested sequential scan to be seen too")
	}
	if len(p.NodeTypes) != 4 {
		t.Errorf("node types = %v, want all four", p.NodeTypes)
	}
}

func TestParsePlanRejectsMalformedJSON(t *testing.T) {
	if _, err := parsePlan(`not json`); err == nil {
		t.Error("expected an error for malformed plan JSON")
	}
	if _, err := parsePlan(`[]`); err == nil {
		t.Error("expected an error for an empty plan array")
	}
}

func TestUsesHypotheticalIndexDistinguishesRealIndexes(t *testing.T) {
	// hypopg names its indexes with a bracketed object identifier; a real
	// index never looks like that. Confusing the two would report a
	// candidate as validated when the planner actually chose an existing
	// index instead.
	real, _ := parsePlan(planJSON(10, "Index Scan", "orders_pkey"))
	if real.UsesHypotheticalIndex() {
		t.Error("a real index was mistaken for a hypothetical one")
	}
	hypo, _ := parsePlan(planJSON(10, "Index Scan", "<13625>btree_orders_customer_id"))
	if !hypo.UsesHypotheticalIndex() {
		t.Error("a hypothetical index was not recognised")
	}
}

func TestExplainUsesGenericPlanWhenRequested(t *testing.T) {
	q := &fakeQuerier{rows: map[string]string{}}
	ctx := t.Context()

	if _, err := Explain(ctx, q, "SELECT 1", true); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(q.seen[0], "GENERIC_PLAN") {
		t.Errorf("generic plan not requested: %s", q.seen[0])
	}

	q2 := &fakeQuerier{rows: map[string]string{}}
	if _, err := Explain(ctx, q2, "SELECT 1", false); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if strings.Contains(q2.seen[0], "GENERIC_PLAN") {
		t.Errorf("generic plan requested when it should not be: %s", q2.seen[0])
	}
}

func TestValidateDetectsAnImprovement(t *testing.T) {
	q := &fakeQuerier{
		rows:      map[string]string{"orders": planJSON(5000, "Seq Scan", "")},
		planAfter: planJSON(12, "Index Scan", "<13625>btree_orders_customer_id"),
	}
	v := New(q, true, Budget{})

	results, err := v.ValidateAll(t.Context(), []*candidates.Candidate{candidate()})
	if err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	r := results[0]
	if !r.Validated {
		t.Error("expected the candidate to be validated")
	}
	if !r.Used {
		t.Error("the planner chose the hypothetical index, but it was not recorded")
	}
	if r.Gain <= 0 {
		t.Errorf("gain = %v, want positive when cost falls from 5000 to 12", r.Gain)
	}
	if r.SizeBytes != 4096 {
		t.Errorf("size = %d, want hypopg's estimate", r.SizeBytes)
	}
}

func TestValidateReportsUnusedCandidate(t *testing.T) {
	// The planner declining the index is the most valuable negative signal
	// the tool can produce: a heuristic score can be confident and wrong.
	q := &fakeQuerier{
		rows:      map[string]string{"orders": planJSON(5000, "Seq Scan", "")},
		planAfter: planJSON(5000, "Seq Scan", ""),
	}
	v := New(q, true, Budget{})

	results, err := v.ValidateAll(t.Context(), []*candidates.Candidate{candidate()})
	if err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
	if results[0].Used {
		t.Error("candidate reported as used when the plan did not change")
	}
	if results[0].Gain != 0 {
		t.Errorf("gain = %v, want zero for an unused index", results[0].Gain)
	}
}

func TestHypotheticalIndexesAreAlwaysCleanedUp(t *testing.T) {
	q := &fakeQuerier{
		rows:      map[string]string{"orders": planJSON(5000, "Seq Scan", "")},
		planAfter: planJSON(12, "Index Scan", "<13625>btree_fake"),
	}
	v := New(q, true, Budget{})

	if _, err := v.ValidateAll(t.Context(), []*candidates.Candidate{candidate(), candidate()}); err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
	if q.resets == 0 {
		t.Error("hypopg_reset was never issued")
	}
	left, err := v.OutstandingHypotheticals(t.Context())
	if err != nil {
		t.Fatalf("OutstandingHypotheticals: %v", err)
	}
	if left != 0 {
		t.Errorf("%d hypothetical indexes left behind in the session", left)
	}
}

func TestCleanupHappensEvenWhenCreateFails(t *testing.T) {
	q := &fakeQuerier{
		rows:         map[string]string{"orders": planJSON(5000, "Seq Scan", "")},
		failCreateOn: "customer_id",
	}
	v := New(q, true, Budget{})

	results, _ := v.ValidateAll(t.Context(), []*candidates.Candidate{candidate()})
	if q.resets == 0 {
		t.Error("hypopg_reset must run even when index creation fails")
	}
	if len(results) == 1 && results[0].Validated {
		t.Error("a candidate whose index could not be created must not be marked validated")
	}
}

func TestBudgetLimitsQueriesPerCandidate(t *testing.T) {
	c := candidate()
	for i := 0; i < 20; i++ {
		c.Queries = append(c.Queries, candidates.QueryRef{
			Fingerprint: "extra", SQL: "SELECT * FROM shop.orders WHERE customer_id = $1", Calls: 1,
		})
	}
	q := &fakeQuerier{
		rows:      map[string]string{"orders": planJSON(5000, "Seq Scan", "")},
		planAfter: planJSON(12, "Index Scan", "<13625>btree_fake"),
	}
	v := New(q, true, Budget{MaxQueriesPer: 3})

	results, err := v.ValidateAll(t.Context(), []*candidates.Candidate{c})
	if err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
	if len(results[0].Queries) > 3 {
		t.Errorf("planned %d queries, budget was 3", len(results[0].Queries))
	}
}

func TestBudgetDeadlineIsHonoured(t *testing.T) {
	// A validation run against production must be bounded in time, not just
	// in count.
	q := &slowQuerier{fakeQuerier: fakeQuerier{
		rows:      map[string]string{"orders": planJSON(5000, "Seq Scan", "")},
		planAfter: planJSON(12, "Index Scan", "<13625>btree_fake"),
	}, delay: 30 * time.Millisecond}

	var many []*candidates.Candidate
	for i := 0; i < 50; i++ {
		many = append(many, candidate())
	}

	v := New(q, true, Budget{Deadline: 100 * time.Millisecond})
	start := time.Now()
	if _, err := v.ValidateAll(t.Context(), many); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ValidateAll: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("run took %v despite a 100ms deadline", elapsed)
	}
}

func TestEmptyCandidateListIsHandled(t *testing.T) {
	q := &fakeQuerier{rows: map[string]string{}}
	v := New(q, true, Budget{})
	results, err := v.ValidateAll(t.Context(), nil)
	if err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want none", results)
	}
}

func TestSkipReasonExplainsParameterisedStatements(t *testing.T) {
	got := skipReason(errors.New(`could not determine data type of parameter $1`))
	if !strings.Contains(got, "GENERIC_PLAN") {
		t.Errorf("skip reason = %q, want a mention of the PostgreSQL 16 feature that fixes it", got)
	}
}

func TestRenderPlanTextIsReadable(t *testing.T) {
	p, _ := parsePlan(planJSON(1234, "Index Scan", "orders_pkey"))
	out := RenderPlanText(p)
	for _, want := range []string{"total cost", "nodes", "indexes", "orders_pkey"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered plan missing %q:\n%s", want, out)
		}
	}

	seq, _ := parsePlan(planJSON(99, "Seq Scan", ""))
	if !strings.Contains(RenderPlanText(seq), "indexes: none") {
		t.Error("a plan with no indexes should say so explicitly")
	}
}

// slowQuerier adds latency so deadline behaviour can be tested.
type slowQuerier struct {
	fakeQuerier
	delay time.Duration
}

func (s *slowQuerier) QueryRow(ctx context.Context, sql string, args ...any) Row {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return fakeRow{err: ctx.Err()}
	}
	return s.fakeQuerier.QueryRow(ctx, sql, args...)
}

func (s *slowQuerier) Exec(ctx context.Context, sql string, args ...any) error {
	return s.fakeQuerier.Exec(ctx, sql, args...)
}

// TestUnsupportedAccessMethodIsNotReportedAsRejected covers a mislabelling
// that would have made every GIN recommendation look worthless.
//
// hypopg can only create btree, brin and hash indexes; it refuses gin, gist
// and spgist outright. Treating that refusal as a planning result would tell
// the user the planner declined their index, when the planner was never shown
// it. "Unproven" and "rejected" are very different things to act on.
func TestUnsupportedAccessMethodIsNotReportedAsRejected(t *testing.T) {
	q := &fakeQuerier{rows: map[string]string{}}
	v := New(q, true, Budget{})

	gin := &candidates.Candidate{
		Schema: "shop", Table: "events",
		Method:  candidates.MethodGIN,
		Columns: []candidates.Column{{Name: "payload", Opclass: candidates.OpclassJSONBPathOps}},
		Queries: []candidates.QueryRef{{Fingerprint: "f", SQL: "SELECT 1", Calls: 10}},
	}

	results, err := v.ValidateAll(t.Context(), []*candidates.Candidate{gin})
	if err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
	r := results[0]

	if r.Validated {
		t.Error("a GIN candidate cannot be validated, but was marked as such")
	}
	if r.Used {
		t.Error("an untested candidate must not be reported as used")
	}
	if !strings.Contains(r.Note, "could not be confirmed") {
		t.Errorf("note = %q, expected it to say the check could not be made", r.Note)
	}
	if q.hypoCreated != 0 {
		t.Error("no hypothetical index should have been attempted for an unsupported type")
	}
}

func TestSupportedMethodsAreValidatedNormally(t *testing.T) {
	for _, method := range []candidates.AccessMethod{"", candidates.MethodBTree, candidates.MethodBRIN} {
		q := &fakeQuerier{
			rows:      map[string]string{"": planJSON(5000, "Seq Scan", "")},
			planAfter: planJSON(10, "Index Scan", "<1>btree_x"),
		}
		v := New(q, true, Budget{})
		c := candidate()
		c.Method = method

		results, err := v.ValidateAll(t.Context(), []*candidates.Candidate{c})
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if !results[0].Validated {
			t.Errorf("%q should be validated by hypopg", method)
		}
	}
}
