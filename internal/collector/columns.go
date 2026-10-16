package collector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// Columns collects column metadata and planner statistics.
func Columns(ctx context.Context, pool *pgxpool.Pool, f Filter) ([]Column, error) {
	rows, err := pool.Query(ctx, db.SQLColumns)
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var c Column
		var nullFrac, nDistinct, corr *float32
		var avgWidth *int32
		var topMCV float32
		if err := rows.Scan(&c.Schema, &c.Table, &c.Num, &c.Name, &c.Type, &c.TypeOID, &c.NotNull,
			&nullFrac, &nDistinct, &corr, &avgWidth, &topMCV); err != nil {
			return nil, fmt.Errorf("columns: %w", err)
		}
		if nullFrac != nil {
			c.HasStats = true
			c.NullFrac = float64(*nullFrac)
		}
		if nDistinct != nil {
			c.NDistinct = float64(*nDistinct)
		}
		if corr != nil {
			c.Correlation = float64(*corr)
		}
		if avgWidth != nil {
			c.AvgWidth = int(*avgWidth)
		}
		c.TopMCVFreq = float64(topMCV)
		if !f.Allows(c.Schema) {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
