package validator

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoExplainAnalyzeAnywhere is the most important test in this repository.
//
// EXPLAIN ANALYZE executes the statement it is given. idxa is an advisory
// tool that people point at production databases, and it promises to be
// strictly read-only: it plans queries, it never runs them. A single
// EXPLAIN ANALYZE would break that promise silently, replaying arbitrary
// workload statements, including expensive ones, against a live server.
//
// The mistake is easy to make, because EXPLAIN ANALYZE is what one normally
// reaches for when investigating a plan. This test makes it impossible to
// introduce without the build failing.
func TestNoExplainAnalyzeAnywhere(t *testing.T) {
	root := repoRoot(t)

	// Matches EXPLAIN ANALYZE and EXPLAIN (..., ANALYZE, ...) in any case
	// and with any spacing.
	pattern := regexp.MustCompile(`(?i)EXPLAIN\s*(\(\s*[^)]*\bANALYZE\b|ANALYZE\b)`)

	var offences []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".sql") {
			return nil
		}
		// This file necessarily contains the phrase it is guarding against.
		if strings.HasSuffix(path, "safety_test.go") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			code := stripComment(line)
			if pattern.MatchString(code) {
				rel, _ := filepath.Rel(root, path)
				offences = append(offences, rel+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offences) > 0 {
		t.Errorf("EXPLAIN ANALYZE executes the statement rather than planning it, "+
			"which would make idxa run arbitrary production queries. Found in:\n  %s",
			strings.Join(offences, "\n  "))
	}
}

// TestSafetyGuardCatchesViolations proves the guard above actually works,
// since a regular expression that matches nothing would pass silently.
func TestSafetyGuardCatchesViolations(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)EXPLAIN\s*(\(\s*[^)]*\bANALYZE\b|ANALYZE\b)`)

	shouldMatch := []string{
		`EXPLAIN ANALYZE SELECT 1`,
		`explain analyze select 1`,
		`EXPLAIN (ANALYZE) SELECT 1`,
		`EXPLAIN (ANALYZE, BUFFERS) SELECT 1`,
		`EXPLAIN (FORMAT JSON, ANALYZE) SELECT 1`,
		`EXPLAIN  (  ANALYZE  )  SELECT 1`,
	}
	for _, s := range shouldMatch {
		if !pattern.MatchString(s) {
			t.Errorf("guard failed to catch %q", s)
		}
	}

	shouldNotMatch := []string{
		`EXPLAIN (FORMAT JSON) SELECT 1`,
		`EXPLAIN (FORMAT JSON, GENERIC_PLAN) SELECT 1`,
		`ANALYZE orders`, // a plain ANALYZE is a statistics refresh, not EXPLAIN
		`SELECT * FROM pg_stats`,
	}
	for _, s := range shouldNotMatch {
		if pattern.MatchString(s) {
			t.Errorf("guard wrongly flagged %q", s)
		}
	}
}

// TestExplainHelperNeverRequestsAnalyze checks the one place that builds an
// EXPLAIN string.
func TestExplainHelperNeverRequestsAnalyze(t *testing.T) {
	q := &fakeQuerier{
		rows: map[string]string{},
	}
	ctx := t.Context()

	_, _ = Explain(ctx, q, "SELECT 1", false)
	_, _ = Explain(ctx, q, "SELECT 1", true)

	for _, sql := range q.seen {
		upper := strings.ToUpper(sql)
		if strings.Contains(upper, "ANALYZE") {
			t.Errorf("Explain issued a statement containing ANALYZE: %s", sql)
		}
		if !strings.HasPrefix(upper, "EXPLAIN (FORMAT JSON") {
			t.Errorf("unexpected explain prefix: %s", sql)
		}
	}
}

// stripComment removes a trailing Go or SQL line comment so that prose
// mentioning the forbidden phrase does not fail the build.
func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		line = line[:i]
	}
	if i := strings.Index(line, "--"); i >= 0 {
		line = line[:i]
	}
	return line
}

// repoRoot walks up from the package directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the module root")
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
