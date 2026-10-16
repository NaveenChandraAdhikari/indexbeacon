package validator

import "testing"

func TestPlannableRewritesTypedPlaceholders(t *testing.T) {
	cases := map[string]string{
		"SELECT 1 FROM t WHERE at > now() - interval $2":                "SELECT 1 FROM t WHERE at > now() - $2::interval",
		"SELECT 1 FROM t WHERE d = date $1 AND ts < TIMESTAMP $3":       "SELECT 1 FROM t WHERE d = $1::date AND ts < $3::TIMESTAMP",
		"SELECT 1 FROM t WHERE at > now() - interval '7 days'":          "SELECT 1 FROM t WHERE at > now() - interval '7 days'",
		"SELECT interval_days FROM t WHERE interval_days = $1":          "SELECT interval_days FROM t WHERE interval_days = $1",
		"SELECT 1 FROM t WHERE x = $1":                                  "SELECT 1 FROM t WHERE x = $1",
		"SELECT 1 FROM t WHERE created > $1::timestamptz - interval $2": "SELECT 1 FROM t WHERE created > $1::timestamptz - $2::interval",
	}
	for in, want := range cases {
		if got := plannable(in); got != want {
			t.Errorf("plannable(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
