package collector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// Indexes collects index definitions and usage statistics.
func Indexes(ctx context.Context, pool *pgxpool.Pool, f Filter) ([]Index, error) {
	rows, err := pool.Query(ctx, db.SQLIndexes)
	if err != nil {
		return nil, fmt.Errorf("indexes: %w", err)
	}
	defer rows.Close()
	var out []Index
	for rows.Next() {
		var ix Index
		var nkey, natts int16
		if err := rows.Scan(&ix.Schema, &ix.Table, &ix.TableOID, &ix.Name, &ix.OID, &ix.AccessMethod,
			&ix.Unique, &ix.Primary, &ix.Valid, &nkey, &natts, &ix.Definition, &ix.Predicate,
			&ix.Columns, &ix.Options, &ix.Opclasses, &ix.Collations, &ix.ConstraintBacked,
			&ix.IdxScan, &ix.IdxTupRead, &ix.IdxTupFetch, &ix.SizeBytes); err != nil {
			return nil, fmt.Errorf("indexes: %w", err)
		}
		ix.NKeyColumns = int(nkey)
		if !f.Allows(ix.Schema) {
			continue
		}
		out = append(out, ix)
	}
	return out, rows.Err()
}
