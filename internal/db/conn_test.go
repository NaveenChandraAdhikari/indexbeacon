package db

import "testing"

func TestRedactDSN(t *testing.T) {
	cases := map[string]string{
		"postgres://alice:secret@db.example:5433/app?sslmode=disable": "alice@db.example:5433/app",
		"host=localhost user=bob password=pw dbname=x":                "bob@localhost:5432/x",
		"not a dsn %%%": "<invalid dsn>",
	}
	for in, want := range cases {
		if got := RedactDSN(in); got != want {
			t.Errorf("RedactDSN(%q) = %q, want %q", in, got, want)
		}
		if want != "<invalid dsn>" && (contains(RedactDSN(in), "secret") || contains(RedactDSN(in), "pw")) {
			t.Errorf("password leaked in %q", RedactDSN(in))
		}
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
