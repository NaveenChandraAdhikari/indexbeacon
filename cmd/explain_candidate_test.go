package cmd

import "testing"

func TestParseIndexSpec(t *testing.T) {
	cases := []struct {
		spec      string
		schema    string
		table     string
		columns   []string
		desc      []bool
		predicate string
	}{
		{"orders(customer_id)", "", "orders", []string{"customer_id"}, []bool{false}, ""},
		{"shop.orders(customer_id)", "shop", "orders", []string{"customer_id"}, []bool{false}, ""},
		{"shop.orders(a, b DESC)", "shop", "orders", []string{"a", "b"}, []bool{false, true}, ""},
		{"  shop.orders ( a , b ) ", "shop", "orders", []string{"a", "b"}, []bool{false, false}, ""},
		{"shop.orders(a) WHERE status = 'x'", "shop", "orders", []string{"a"}, []bool{false}, "status = 'x'"},
		{`shop.orders("a b")`, "shop", "orders", []string{"a b"}, []bool{false}, ""},
	}

	for _, tc := range cases {
		got, err := parseIndexSpec(tc.spec)
		if err != nil {
			t.Errorf("%q: %v", tc.spec, err)
			continue
		}
		if got.Schema != tc.schema || got.Table != tc.table {
			t.Errorf("%q: table = %s.%s", tc.spec, got.Schema, got.Table)
		}
		if len(got.Columns) != len(tc.columns) {
			t.Errorf("%q: columns = %v", tc.spec, got.ColumnNames())
			continue
		}
		for i, name := range tc.columns {
			if got.Columns[i].Name != name {
				t.Errorf("%q: column %d = %q, want %q", tc.spec, i, got.Columns[i].Name, name)
			}
			if got.Columns[i].Desc != tc.desc[i] {
				t.Errorf("%q: column %d direction = %v", tc.spec, i, got.Columns[i].Desc)
			}
		}
		if got.Predicate != tc.predicate {
			t.Errorf("%q: predicate = %q, want %q", tc.spec, got.Predicate, tc.predicate)
		}
	}
}

func TestParseIndexSpecRejectsNonsense(t *testing.T) {
	// The error has to say what the right form looks like, because this is
	// the first thing a user gets wrong.
	for _, spec := range []string{"", "orders", "orders()", "()", "just some words"} {
		_, err := parseIndexSpec(spec)
		if err == nil {
			t.Errorf("%q was accepted", spec)
			continue
		}
		if spec != "" && !contains(err.Error(), "schema.table") && !contains(err.Error(), "no columns") {
			t.Errorf("%q: error should show the expected form, got %v", spec, err)
		}
	}
}

func TestParseIndexSpecKeepsOpclass(t *testing.T) {
	got, err := parseIndexSpec("docs(title gin_trgm_ops)")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Columns[0].Opclass != "gin_trgm_ops" {
		t.Errorf("opclass = %q", got.Columns[0].Opclass)
	}
}

func TestOneLineCollapsesAndTruncates(t *testing.T) {
	if got := oneLine("SELECT\n  1\n  FROM t"); got != "SELECT 1 FROM t" {
		t.Errorf("oneLine = %q", got)
	}
	long := ""
	for i := 0; i < 50; i++ {
		long += "SELECT column_name, "
	}
	if got := oneLine(long); len(got) > 140 {
		t.Errorf("oneLine returned %d characters, want it truncated", len(got))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
