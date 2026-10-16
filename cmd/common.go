package cmd

import (
	"context"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/report"
)

func collectorOptions(skipStatements bool) collector.Options {
	return collector.Options{
		Filter: collector.Filter{
			Schemas:        opts.Schemas,
			ExcludeSchemas: opts.ExcludeSchemas,
		},
		SkipStatements: skipStatements,
	}
}

// collectAll opens every node and collects a snapshot from each.
func collectAll(ctx context.Context, skipStatements bool) ([]*collector.Snapshot, error) {
	nodes, err := openNodes(ctx)
	if err != nil {
		return nil, err
	}
	defer closeNodes(nodes)
	snaps := make([]*collector.Snapshot, 0, len(nodes))
	for _, n := range nodes {
		logf("collecting from %s", n.Info.Name)
		s, err := collector.Collect(ctx, n, collectorOptions(skipStatements))
		if err != nil {
			return nil, err
		}
		logf("%s: %d tables, %d indexes, %d columns, %d statements", n.Info.Name, len(s.Tables), len(s.Indexes), len(s.Columns), len(s.Statements))
		snaps = append(snaps, s)
	}
	return snaps, nil
}

func newReport(kind report.Kind, snaps []*collector.Snapshot) *report.Report {
	r := &report.Report{Kind: kind, Version: version, GeneratedAt: time.Now().UTC()}
	for _, s := range snaps {
		r.Nodes = append(r.Nodes, report.NodeSummaryFrom(s.Node, len(s.Statements)))
		r.Warnings = append(r.Warnings, s.Warnings...)
	}
	return r
}

func writeReport(r *report.Report) error {
	w, closeFn, err := outputWriter()
	if err != nil {
		return err
	}
	if err := report.Render(w, r, report.Format(opts.Format)); err != nil {
		_ = closeFn()
		return err
	}
	return closeFn()
}
