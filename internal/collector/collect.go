package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// Options tune a collection run.
type Options struct {
	Filter         Filter
	StatementLimit int
	// SkipStatements disables reading pg_stat_statements even if available.
	SkipStatements bool
}

// Collect gathers a full snapshot from one node.
func Collect(ctx context.Context, node *db.Node, o Options) (*Snapshot, error) {
	snap := &Snapshot{Node: node.Info, CollectedAt: time.Now()}
	var err error
	if snap.Tables, err = Tables(ctx, node.Pool, o.Filter); err != nil {
		return nil, err
	}
	if snap.Indexes, err = Indexes(ctx, node.Pool, o.Filter); err != nil {
		return nil, err
	}
	if snap.Columns, err = Columns(ctx, node.Pool, o.Filter); err != nil {
		return nil, err
	}
	if !o.SkipStatements {
		switch {
		case !node.Info.HasExtension("pg_stat_statements"):
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("%s: pg_stat_statements is not installed in database %s", node.Info.Name, node.Info.Database))
		case !node.Info.PSSPreloaded():
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("%s: pg_stat_statements is installed but not preloaded", node.Info.Name))
		default:
			stmts, err := Statements(ctx, node.Pool, o.StatementLimit)
			if err != nil {
				snap.Warnings = append(snap.Warnings, fmt.Sprintf("%s: %v", node.Info.Name, err))
			} else {
				snap.Statements = stmts
				snap.StatementsSource = "pg_stat_statements"
			}
		}
	}
	return snap, nil
}
