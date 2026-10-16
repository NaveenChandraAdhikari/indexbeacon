package candidates

import "testing"

// These tests pin down three false-positive classes found by running the
// advisor against a live server. Each one produced a confident, well-scored
// recommendation that was worthless, and each is easy to reintroduce.

func TestUniqueIndexMakesLongerCandidateRedundant(t *testing.T) {
	// PostgreSQL issues `SELECT 1 FROM t WHERE id = $1 FOR KEY SHARE` for
	// foreign key checks, hundreds of thousands of times. Joined with another
	// predicate that produces a candidate on (id, customer_id), which looks
	// enormously valuable by call count. It is worthless: id is the primary
	// key, so at most one row matches and the second column narrows nothing.
	c := &Candidate{
		Schema: "shop", Table: "orders",
		Columns: []Column{{Name: "id"}, {Name: "customer_id"}},
	}
	existing := []ExistingIndex{{
		Schema: "shop", Table: "orders", Name: "orders_pkey",
		Columns: []string{"id"}, Unique: true, Valid: true, Scans: 1000,
	}}

	if got := Merge([]*Candidate{c}, existing); len(got) != 0 {
		t.Errorf("candidate extending a unique index survived: %v", keys(got))
	}
}

func TestNonUniqueIndexDoesNotSuppressLongerCandidate(t *testing.T) {
	// The rule above must not overreach. Extending a non-unique index is a
	// legitimate recommendation, because the extra column really does narrow
	// the scan further.
	c := &Candidate{
		Schema: "shop", Table: "orders",
		Columns: []Column{{Name: "customer_id"}, {Name: "created_at"}},
	}
	existing := []ExistingIndex{{
		Schema: "shop", Table: "orders", Name: "orders_customer_id_idx",
		Columns: []string{"customer_id"}, Unique: false, Valid: true, Scans: 1000,
	}}

	if got := Merge([]*Candidate{c}, existing); len(got) != 1 {
		t.Errorf("extending a non-unique index was wrongly suppressed: %v", keys(got))
	}
}

func TestUniqueIndexOnDifferentLeadingColumnDoesNotSuppress(t *testing.T) {
	c := &Candidate{
		Schema: "shop", Table: "orders",
		Columns: []Column{{Name: "customer_id"}, {Name: "id"}},
	}
	existing := []ExistingIndex{{
		Schema: "shop", Table: "orders", Name: "orders_pkey",
		Columns: []string{"id"}, Unique: true, Valid: true, Scans: 1000,
	}}

	// The unique index is not a leading prefix here, so it cannot help this
	// candidate's queries.
	if got := Merge([]*Candidate{c}, existing); len(got) != 1 {
		t.Errorf("unique index on a trailing column wrongly suppressed: %v", keys(got))
	}
}

func TestDirectionVariantsCollapseToOne(t *testing.T) {
	// A query ordering by created_at DESC and another ordering ascending
	// produce two candidates that differ only in sort direction. A btree can
	// be scanned backwards, so they are the same index, and recommending both
	// means building two indexes where one would do.
	asc := &Candidate{
		Table:   "orders",
		Columns: []Column{{Name: "customer_id"}, {Name: "created_at"}},
		Queries: []QueryRef{{Fingerprint: "asc", Calls: 10}},
	}
	desc := &Candidate{
		Table:   "orders",
		Columns: []Column{{Name: "customer_id"}, {Name: "created_at", Desc: true}},
		Queries: []QueryRef{{Fingerprint: "desc", Calls: 500}},
	}

	got := Merge([]*Candidate{asc, desc}, nil)
	if len(got) != 1 {
		t.Fatalf("direction variants were not collapsed: %v", keys(got))
	}
	// The direction most queries asked for should be the one recommended.
	if !got[0].Columns[1].Desc {
		t.Errorf("kept %s, but far more calls wanted the descending variant", got[0].Key())
	}
	if len(got[0].Queries) != 2 {
		t.Errorf("the collapsed variant's queries were lost: %v", got[0].Queries)
	}
}

func TestDirectionVariantsDoNotCollapseAcrossDifferentColumns(t *testing.T) {
	a := &Candidate{Table: "t", Columns: []Column{{Name: "a"}, {Name: "b"}}}
	b := &Candidate{Table: "t", Columns: []Column{{Name: "b"}, {Name: "a"}}}
	if got := Merge([]*Candidate{a, b}, nil); len(got) != 2 {
		t.Errorf("different column orders were wrongly collapsed: %v", keys(got))
	}
}

func TestDirectionVariantsRespectPartialPredicates(t *testing.T) {
	full := &Candidate{Table: "t", Columns: []Column{{Name: "a"}}}
	partial := &Candidate{Table: "t", Columns: []Column{{Name: "a"}}, Predicate: "b = 'x'"}
	if got := Merge([]*Candidate{full, partial}, nil); len(got) != 2 {
		t.Errorf("a partial and a full index were wrongly collapsed: %v", keys(got))
	}
}
