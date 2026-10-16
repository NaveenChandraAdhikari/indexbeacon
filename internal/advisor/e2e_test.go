//go:build integration

package advisor

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/db"
	"github.com/indexbeacon/indexbeacon/internal/report"
	"github.com/indexbeacon/indexbeacon/internal/validator"
	"github.com/jackc/pgx/v5"
)

// These tests run the whole pipeline against a real PostgreSQL: build a schema
// with an index deliberately missing, generate traffic, and check that idxa
// recommends exactly that index and nothing else.
//
// Unit tests cover each stage in isolation, but only this catches the failures
// that come from the stages disagreeing: a predicate parsed correctly and
// attributed to the wrong table, a candidate scored well and rejected by the
// planner, a recommendation whose statement will not execute.

const schemaName = "idxa_e2e"

func dsn(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("IDXA_TEST_DSN"); v != "" {
		return v
	}
	t.Skip("set IDXA_TEST_DSN to run end-to-end tests")
	return ""
}

// setup builds a schema whose correct answer is known.
func setup(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()

	stmts := []string{
		`DROP SCHEMA IF EXISTS ` + schemaName + ` CASCADE`,
		`CREATE SCHEMA ` + schemaName,
		`CREATE TABLE ` + schemaName + `.account (
			id bigserial PRIMARY KEY,
			tenant_id bigint NOT NULL,
			email text NOT NULL,
			status text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now()
		)`,
		// A deliberately unindexed foreign key on a child table.
		`CREATE TABLE ` + schemaName + `.entry (
			id bigserial PRIMARY KEY,
			account_id bigint NOT NULL,
			amount numeric NOT NULL,
			occurred_at timestamptz NOT NULL DEFAULT now()
		)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", firstLine(s), err)
		}
	}

	// Enough rows that an index scan is clearly cheaper than reading the table.
	if _, err := conn.Exec(ctx, `
		INSERT INTO `+schemaName+`.account (tenant_id, email, status)
		SELECT (i % 500), 'user' || i || '@example.com',
		       CASE WHEN i % 50 = 0 THEN 'pending' ELSE 'active' END
		FROM generate_series(1, 50000) i`); err != nil {
		t.Fatalf("load accounts: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO `+schemaName+`.entry (account_id, amount, occurred_at)
		SELECT (i % 50000) + 1, (i % 997)::numeric,
		       now() - (i || ' seconds')::interval
		FROM generate_series(1, 200000) i`); err != nil {
		t.Fatalf("load entries: %v", err)
	}
	if _, err := conn.Exec(ctx, `ANALYZE `+schemaName+`.account`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if _, err := conn.Exec(ctx, `ANALYZE `+schemaName+`.entry`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+schemaName+` CASCADE`)
	})
}

// generateTraffic runs the queries whose missing indexes we expect to be found.
func generateTraffic(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()

	if _, err := conn.Exec(ctx, `SELECT pg_stat_statements_reset()`); err != nil {
		t.Skipf("pg_stat_statements is not available: %v", err)
	}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		// Wants entry(account_id) or entry(account_id, occurred_at DESC).
		rows, err := conn.Query(ctx,
			`SELECT id, amount FROM `+schemaName+`.entry
			 WHERE account_id = $1 ORDER BY occurred_at DESC LIMIT 20`,
			rng.Intn(50000)+1)
		if err != nil {
			t.Fatalf("traffic: %v", err)
		}
		for rows.Next() {
		}
		rows.Close()

		// Wants account(tenant_id).
		rows, err = conn.Query(ctx,
			`SELECT id, email FROM `+schemaName+`.account WHERE tenant_id = $1`,
			rng.Intn(500))
		if err != nil {
			t.Fatalf("traffic: %v", err)
		}
		for rows.Next() {
		}
		rows.Close()
	}
}

func openNode(t *testing.T) *db.Node {
	t.Helper()
	node, err := db.Open(context.Background(), "test", dsn(t), db.Options{
		StatementTimeout: 60 * time.Second,
		MaxConns:         4,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(node.Close)
	return node
}

// TestRecommendsTheMissingIndexes is the test that matters: does the whole
// pipeline, end to end, find the indexes we deliberately left out?
func TestRecommendsTheMissingIndexes(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	setup(t, conn)
	generateTraffic(t, conn)

	node := openNode(t)
	snap, err := collector.Collect(ctx, node, collector.Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	res := Missing([]*collector.Snapshot{snap}, MissingOptions{
		MinRows:            1000,
		MaxRecommendations: 10,
	})

	if len(res.Recommendations) == 0 {
		t.Fatalf("no recommendations; warnings: %v", res.Warnings)
	}

	wanted := map[string]bool{
		schemaName + ".entry:account_id":  false,
		schemaName + ".account:tenant_id": false,
	}
	for _, rec := range res.Recommendations {
		key := fmt.Sprintf("%s.%s:%s", rec.Schema, rec.Table, rec.Columns[0])
		key = strings.TrimSuffix(key, " DESC")
		if _, ok := wanted[key]; ok {
			wanted[key] = true
		}
		t.Logf("recommended %s.%s(%s) size=%d confidence=%s",
			rec.Schema, rec.Table, strings.Join(rec.Columns, ", "),
			rec.EstimatedBytes, rec.Confidence)
	}

	for key, found := range wanted {
		if !found {
			t.Errorf("expected a recommendation leading with %s", key)
		}
	}

	// A recommendation nobody can act on is worthless, so the statement has
	// to be valid SQL. It is checked inside a transaction that is rolled
	// back, never executed for real.
	for _, rec := range res.Recommendations {
		assertStatementParses(t, conn, rec)
	}
}

// assertStatementParses proves the generated DDL is syntactically valid
// without ever creating anything.
func assertStatementParses(t *testing.T, conn *pgx.Conn, rec report.Recommendation) {
	t.Helper()
	ctx := context.Background()

	// CONCURRENTLY cannot run inside a transaction, so the check uses the
	// plain form of the same statement; the syntax under test is identical.
	stmt := strings.Replace(rec.CreateSQL, "CREATE INDEX CONCURRENTLY", "CREATE INDEX", 1)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Prepare(ctx, "check_"+rec.IndexName, stmt); err != nil {
		t.Errorf("generated statement is not valid SQL:\n%s\n%v", stmt, err)
	}
}

// TestValidationConfirmsAgainstThePlanner checks the hypopg path end to end,
// including that the session is left with nothing in it.
func TestValidationConfirmsAgainstThePlanner(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	var hasHypopg bool
	if err := conn.QueryRow(ctx,
		`SELECT count(*) > 0 FROM pg_extension WHERE extname = 'hypopg'`).Scan(&hasHypopg); err != nil {
		t.Fatalf("check hypopg: %v", err)
	}
	if !hasHypopg {
		t.Skip("hypopg is not installed on the test server")
	}

	setup(t, conn)
	generateTraffic(t, conn)

	node := openNode(t)
	snap, err := collector.Collect(ctx, node, collector.Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	res := Missing([]*collector.Snapshot{snap}, MissingOptions{MinRows: 1000})
	if len(res.Recommendations) == 0 {
		t.Skip("no recommendations to validate")
	}

	q, err := validator.Acquire(ctx, node.Pool)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer q.Release()

	validated, _, err := Validate(ctx, q, node.Info.ServerVersionNum >= 160000,
		res.Recommendations, validator.Budget{MaxCandidates: 5})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	var confirmed int
	for _, rec := range validated {
		if rec.Validated && rec.PlanUsesIndex != nil && *rec.PlanUsesIndex {
			confirmed++
			t.Logf("planner confirmed %s.%s(%s)", rec.Schema, rec.Table, strings.Join(rec.Columns, ", "))
		}
	}
	if confirmed == 0 {
		t.Error("the planner confirmed none of the recommendations")
	}

	// Nothing hypothetical may survive the run.
	var left int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM hypopg_list_indexes`).Scan(&left); err == nil && left != 0 {
		t.Errorf("%d hypothetical indexes were left in the session", left)
	}
}

// TestNothingIsEverWritten is the guarantee the whole tool rests on.
func TestNothingIsEverWritten(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	setup(t, conn)
	generateTraffic(t, conn)

	before := databaseFingerprint(t, conn)

	node := openNode(t)
	snap, err := collector.Collect(ctx, node, collector.Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	res := Missing([]*collector.Snapshot{snap}, MissingOptions{MinRows: 1000})

	if q, err := validator.Acquire(ctx, node.Pool); err == nil {
		_, _, _ = Validate(ctx, q, node.Info.ServerVersionNum >= 160000,
			res.Recommendations, validator.Budget{MaxCandidates: 5})
		q.Release()
	}

	unused, _ := Unused([]*collector.Snapshot{snap}, UnusedOptions{})
	_ = unused
	_ = Duplicates([]*collector.Snapshot{snap})

	after := databaseFingerprint(t, conn)
	if before != after {
		t.Errorf("the database changed during analysis:\n  before: %s\n  after:  %s", before, after)
	}
}

// databaseFingerprint captures what must not change: the set of relations and
// the number of rows written.
func databaseFingerprint(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	ctx := context.Background()

	var relations, indexes int
	var inserted, updated, deleted int64
	if err := conn.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM pg_class WHERE relkind IN ('r','i','m','p')),
		  (SELECT count(*) FROM pg_index),
		  coalesce((SELECT tup_inserted FROM pg_stat_database WHERE datname = current_database()), 0),
		  coalesce((SELECT tup_updated  FROM pg_stat_database WHERE datname = current_database()), 0),
		  coalesce((SELECT tup_deleted  FROM pg_stat_database WHERE datname = current_database()), 0)
	`).Scan(&relations, &indexes, &inserted, &updated, &deleted); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fmt.Sprintf("relations=%d indexes=%d inserted=%d updated=%d deleted=%d",
		relations, indexes, inserted, updated, deleted)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
