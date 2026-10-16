package collector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// Tables collects table statistics.
func Tables(ctx context.Context, pool *pgxpool.Pool, f Filter) ([]Table, error) {
	rows, err := pool.Query(ctx, db.SQLTables)
	if err != nil {
		return nil, fmt.Errorf("tables: %w", err)
	}
	defer rows.Close()
	var out []Table
	for rows.Next() {
		var t Table
		var relpages int32
		var reltuples float32
		if err := rows.Scan(&t.Schema, &t.Name, &t.OID, &t.Kind, &relpages, &reltuples,
			&t.LiveTuples, &t.DeadTuples, &t.SeqScan, &t.SeqTupRead, &t.IdxScan, &t.IdxTupFetch,
			&t.Inserts, &t.Updates, &t.Deletes, &t.HotUpdates, &t.TableBytes, &t.IndexBytes); err != nil {
			return nil, fmt.Errorf("tables: %w", err)
		}
		t.RelPages = int64(relpages)
		if reltuples > 0 {
			t.RelTuples = int64(reltuples)
		}
		if !f.Allows(t.Schema) {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
