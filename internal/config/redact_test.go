package config

import (
	"strings"
	"testing"
)

// TestRedactRemovesPasswords is a security test. Anything that renders a
// connection string as text, including error messages and log lines, goes
// through Redact, so a gap here leaks credentials into places people paste
// into bug reports.
func TestRedactRemovesPasswords(t *testing.T) {
	secret := "sup3rs3cret"

	cases := []string{
		"postgres://user:" + secret + "@localhost:5432/app",
		"postgresql://user:" + secret + "@host/db?sslmode=require",
		"host=localhost user=app password=" + secret + " dbname=app",
		"host=localhost password='" + secret + "' dbname=app",
		`host=localhost password="` + secret + `" dbname=app`,
		"postgres://user@host/db?password=" + secret,
		"PASSWORD=" + secret + " host=localhost",
	}

	for _, dsn := range cases {
		got := Redact(dsn)
		if strings.Contains(got, secret) {
			t.Errorf("password survived redaction:\n  in:  %s\n  out: %s", dsn, got)
		}
		if !strings.Contains(got, redactedMark) {
			t.Errorf("no redaction marker in output:\n  in:  %s\n  out: %s", dsn, got)
		}
	}
}

func TestRedactKeepsUsefulDetail(t *testing.T) {
	// A redacted string still has to be recognisable, or it cannot be used
	// to tell two databases apart in the interface.
	got := Redact("postgres://appuser:secret@db.internal:5432/orders")
	for _, want := range []string{"appuser", "db.internal", "5432", "orders"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction removed %q, leaving %q unusable for identification", want, got)
		}
	}
}

func TestRedactLeavesPasswordlessStringsAlone(t *testing.T) {
	cases := []string{
		"postgres://user@localhost/db",
		"host=localhost dbname=app",
		"",
	}
	for _, dsn := range cases {
		if got := Redact(dsn); got != dsn && !strings.Contains(got, dsn) {
			// A URL may be normalised, which is acceptable, but nothing
			// should be marked as redacted.
			if strings.Contains(got, redactedMark) {
				t.Errorf("Redact(%q) = %q, marked a redaction where there was no password", dsn, got)
			}
		}
	}
}

func TestRedactHandlesEnvironmentReferences(t *testing.T) {
	// An environment reference is not itself a secret, but redacting it is
	// harmless and safer than trying to be clever.
	got := Redact("postgres://user:${PGPASSWORD}@host/db")
	if strings.Contains(got, "PGPASSWORD}") && !strings.Contains(got, redactedMark) {
		t.Logf("environment reference left in place: %s", got)
	}
}

func TestDescribeProducesAShortLabel(t *testing.T) {
	cases := map[string]string{
		"postgres://idxa:pw@localhost:5499/idxa":                       "idxa@localhost:5499/idxa",
		"postgres://localhost/app":                                     "localhost/app",
		"host=db.internal port=5432 user=app dbname=orders password=x": "app@db.internal:5432/orders",
	}
	for dsn, want := range cases {
		if got := Describe(dsn); got != want {
			t.Errorf("Describe(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func TestDescribeNeverLeaksThePassword(t *testing.T) {
	secret := "topsecret"
	cases := []string{
		"postgres://user:" + secret + "@host:5432/db",
		"host=h user=u password=" + secret + " dbname=d",
	}
	for _, dsn := range cases {
		if got := Describe(dsn); strings.Contains(got, secret) {
			t.Errorf("Describe leaked the password: %s", got)
		}
	}
}

func TestHasInlinePassword(t *testing.T) {
	withPassword := []string{
		"postgres://user:pw@host/db",
		"host=h password=pw dbname=d",
	}
	for _, dsn := range withPassword {
		if !HasInlinePassword(dsn) {
			t.Errorf("HasInlinePassword(%q) = false, want true", dsn)
		}
	}

	without := []string{
		"postgres://user@host/db",
		"host=h dbname=d",
		"postgres://user:${PGPASSWORD}@host/db",
	}
	for _, dsn := range without {
		if HasInlinePassword(dsn) {
			t.Errorf("HasInlinePassword(%q) = true, want false", dsn)
		}
	}
}
