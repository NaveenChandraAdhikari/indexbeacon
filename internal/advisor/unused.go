// Package advisor turns collected snapshots into report findings.
package advisor

import (
	"fmt"
	"sort"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/ident"
	"github.com/indexbeacon/indexbeacon/internal/report"
)

// UnusedOptions tune the unused-index detector.
type UnusedOptions struct {
	// MinStatsAge below which confidence is low.
	MinStatsAge time.Duration
	// MinSizeBytes hides tiny indexes (default 0 = show all).
	MinSizeBytes int64
}

// Unused finds indexes with zero scans summed across all snapshots.
// The first snapshot is the primary and provides definitions and sizes.
// Primary-key, unique, constraint-backing and invalid indexes are excluded.
// withDefaults fills in a sensible threshold. Without it a caller that omits
// the option produces the nonsense warning "shorter than 0s".
func (o UnusedOptions) withDefaults() UnusedOptions {
	if o.MinStatsAge <= 0 {
		o.MinStatsAge = time.Hour
	}
	return o
}

func Unused(snaps []*collector.Snapshot, o UnusedOptions) ([]report.UnusedIndex, []string) {
	o = o.withDefaults()
	if len(snaps) == 0 {
		return nil, nil
	}
	primary := snaps[0]
	var warnings []string

	// Per-node scan counts keyed by schema.index.
	type key struct{ schema, index string }
	scans := map[key][]report.PerNodeCount{}
	seen := map[key]int{}
	for _, s := range snaps {
		for _, ix := range s.Indexes {
			k := key{ix.Schema, ix.Name}
			scans[k] = append(scans[k], report.PerNodeCount{Node: s.Node.Name, Count: ix.IdxScan})
			seen[k]++
		}
	}

	conf := confidenceFor(snaps, o.MinStatsAge)
	tableWrites := map[string]int64{}
	for _, s := range snaps {
		for _, t := range s.Tables {
			tableWrites[t.QualifiedName()] += t.Writes()
		}
	}

	var out []report.UnusedIndex
	for _, ix := range primary.Indexes {
		k := key{ix.Schema, ix.Name}
		if seen[k] != len(snaps) {
			warnings = append(warnings, fmt.Sprintf("index %s.%s is not present on every node; skipped", ix.Schema, ix.Name))
			continue
		}
		if ix.Primary || ix.Unique || ix.ConstraintBacked || !ix.Valid {
			continue
		}
		if ix.SizeBytes < o.MinSizeBytes {
			continue
		}
		var total int64
		for _, c := range scans[k] {
			total += c.Count
		}
		if total != 0 {
			continue
		}
		reason := "zero index scans on every node since stats reset"
		if len(snaps) == 1 {
			reason = "zero index scans since stats reset"
		}
		out = append(out, report.UnusedIndex{
			Schema:     ix.Schema,
			Table:      ix.Table,
			Index:      ix.Name,
			Definition: ix.Definition,
			SizeBytes:  ix.SizeBytes,
			Scans:      scans[k],
			TableWrite: tableWrites[ix.QualifiedTable()],
			Reason:     reason,
			Confidence: conf,
			DropSQL:    "DROP INDEX CONCURRENTLY " + ident.Qualified(ix.Schema, ix.Name) + ";",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SizeBytes != out[j].SizeBytes {
			return out[i].SizeBytes > out[j].SizeBytes
		}
		return out[i].Index < out[j].Index
	})
	if conf == report.ConfidenceLow {
		warnings = append(warnings, fmt.Sprintf("statistics on at least one node have only been collecting for less than %s, so an index used only occasionally may be listed here; wait until the window covers your weekly and monthly jobs before dropping anything", humanAge(o.MinStatsAge)))
	}
	return out, warnings
}

// confidenceFor derives report confidence from the shortest stats window.
func confidenceFor(snaps []*collector.Snapshot, minAge time.Duration) report.Confidence {
	c := report.ConfidenceHigh
	for _, s := range snaps {
		age, known := s.StatsWindow()
		switch {
		case !known:
			return report.ConfidenceLow
		case age < minAge:
			return report.ConfidenceLow
		case age < 7*24*time.Hour && c == report.ConfidenceHigh:
			c = report.ConfidenceMedium
		}
	}
	return c
}

// humanAge renders a duration the way a person would say it, since Go's
// default rendering produces things like "1h0m0s".
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "an hour"
		}
		return fmt.Sprintf("%d hours", hours)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "a day"
		}
		return fmt.Sprintf("%d days", days)
	}
}
