package validator

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolQuerier adapts a pgx connection to the Querier interface.
//
// Validation must run on a single session, not a pool: hypothetical indexes
// exist only in the session that created them, so a pooled call could plan a
// query on a connection where the index does not exist and report a perfectly
// good candidate as useless. Acquire holds one connection for the whole run.
type PoolQuerier struct {
	conn *pgxpool.Conn
}

// Acquire takes a single connection from the pool for the duration of a
// validation run. Release must be called when finished.
func Acquire(ctx context.Context, pool *pgxpool.Pool) (*PoolQuerier, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return &PoolQuerier{conn: conn}, nil
}

// Release returns the connection to the pool. Any hypothetical indexes are
// discarded along with the session state.
func (p *PoolQuerier) Release() {
	if p != nil && p.conn != nil {
		p.conn.Release()
	}
}

// QueryRow runs a single-row query.
//
// Statements with no arguments are sent through the raw protocol rather than
// through pgx's query machinery. A workload statement read from
// pg_stat_statements still contains its $1 placeholders, and both of pgx's
// normal paths try to supply values for them: the extended protocol asks the
// server to bind parameters that were never provided, and the simple protocol
// interpolates arguments into the text and fails with "insufficient
// arguments". EXPLAIN (GENERIC_PLAN) exists precisely to plan a statement
// whose parameters have no values, so the text has to reach the server
// untouched.
func (p *PoolQuerier) QueryRow(ctx context.Context, sql string, args ...any) Row {
	if len(args) == 0 {
		return p.rawRow(ctx, sql)
	}
	return pgxRow{p.conn.QueryRow(ctx, sql, args...)}
}

// Exec runs a statement, discarding any result.
func (p *PoolQuerier) Exec(ctx context.Context, sql string, args ...any) error {
	if len(args) == 0 {
		_, err := p.raw(ctx, sql)
		return err
	}
	// hypopg's entry points are functions, so they are invoked with SELECT
	// and must run as queries rather than commands.
	rows, err := p.conn.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

// raw sends a statement verbatim over the simple query protocol and returns
// the first column of the first row, if any.
func (p *PoolQuerier) raw(ctx context.Context, sql string) (string, error) {
	results, err := p.conn.Conn().PgConn().Exec(ctx, sql).ReadAll()
	if err != nil {
		return "", err
	}
	for _, res := range results {
		if res.Err != nil {
			return "", res.Err
		}
		if len(res.Rows) > 0 && len(res.Rows[0]) > 0 {
			return string(res.Rows[0][0]), nil
		}
	}
	return "", nil
}

// rawRow adapts a verbatim statement to the Row interface.
func (p *PoolQuerier) rawRow(ctx context.Context, sql string) Row {
	value, err := p.raw(ctx, sql)
	return rawRow{value: value, err: err}
}

type rawRow struct {
	value string
	err   error
}

func (r rawRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 0 {
		return nil
	}
	switch t := dest[0].(type) {
	case *string:
		*t = r.value
	case *int64:
		var n int64
		if _, err := fmt.Sscanf(r.value, "%d", &n); err != nil {
			return fmt.Errorf("scan %q as integer: %w", r.value, err)
		}
		*t = n
	case *int:
		var n int
		if _, err := fmt.Sscanf(r.value, "%d", &n); err != nil {
			return fmt.Errorf("scan %q as integer: %w", r.value, err)
		}
		*t = n
	default:
		return errors.New("unsupported destination type for a raw query")
	}
	return nil
}

// pgxRow adapts pgx.Row to the local Row interface.
type pgxRow struct {
	row pgx.Row
}

func (r pgxRow) Scan(dest ...any) error { return r.row.Scan(dest...) }
