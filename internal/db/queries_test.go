package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestQueriesAreReadOnly is the static half of idxa's read-only guarantee:
// every SQL constant in queries.go must be a read-only statement.
func TestQueriesAreReadOnly(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "queries.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	allowedPrefixes := []string{"SELECT", "WITH", "EXPLAIN", "SET", "SHOW"}
	hypopgWhitelist := []string{
		"SELECT indexrelid, indexname FROM hypopg_create_index($1)",
		"SELECT hypopg_reset()",
		"SELECT indexrelid, indexname FROM hypopg_list_indexes()",
		"SELECT hypopg_relation_size($1)",
	}
	forbidden := []string{"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "CREATE", "TRUNCATE", "GRANT", "REVOKE", "COPY", "VACUUM", "REINDEX", "CLUSTER", "LOCK", "REFRESH", "CALL", "DO "}

	count := 0
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "SQL") || i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: SQL constant must be a string literal", name.Name)
				continue
			}
			sql, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Errorf("%s: %v", name.Name, err)
				continue
			}
			count++
			trimmed := strings.TrimSpace(sql)
			upper := strings.ToUpper(trimmed)
			okPrefix := false
			for _, p := range allowedPrefixes {
				if strings.HasPrefix(upper, p) {
					okPrefix = true
					break
				}
			}
			if !okPrefix {
				t.Errorf("%s does not start with an allowed verb: %q", name.Name, trimmed)
			}
			if strings.Contains(upper, "HYPOPG_") {
				whitelisted := false
				for _, w := range hypopgWhitelist {
					if trimmed == w {
						whitelisted = true
					}
				}
				if !whitelisted {
					t.Errorf("%s uses hypopg outside the whitelist: %q", name.Name, trimmed)
				}
			}
			// A statement that begins with SELECT/WITH could still smuggle a
			// data-modifying CTE; reject any write keyword at a token boundary.
			for _, kw := range forbidden {
				for _, tok := range strings.FieldsFunc(upper, func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' || r == '(' || r == ')' || r == ',' }) {
					if tok == strings.TrimSpace(kw) {
						t.Errorf("%s contains forbidden keyword %s: %q", name.Name, kw, trimmed)
					}
				}
			}
		}
		return true
	})
	if count < 10 {
		t.Fatalf("expected to inspect at least 10 SQL constants, saw %d", count)
	}
}
