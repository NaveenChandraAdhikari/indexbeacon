package collector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// DefaultStatementLimit caps how many pg_stat_statements rows are read.
const DefaultStatementLimit = 5000

// Statements reads the top statements by total execution time.
func Statements(ctx context.Context, pool *pgxpool.Pool, limit int) ([]Statement, error) {
	if limit <= 0 {
		limit = DefaultStatementLimit
	}
	rows, err := pool.Query(ctx, db.SQLStatements, limit)
	if err != nil {
		return nil, fmt.Errorf("pg_stat_statements: %w", err)
	}
	defer rows.Close()
	var out []Statement
	for rows.Next() {
		var s Statement
		var qid *int64
		var query *string
		if err := rows.Scan(&qid, &query, &s.Calls, &s.TotalExecTime, &s.MeanExecTime, &s.Rows, &s.SharedBlksHit, &s.SharedBlksRead); err != nil {
			return nil, fmt.Errorf("pg_stat_statements: %w", err)
		}
		if qid != nil {
			s.QueryID = *qid
		}
		if query != nil {
			s.Query = *query
		}
		if s.Query == "" || s.Query == "<insufficient privilege>" {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
