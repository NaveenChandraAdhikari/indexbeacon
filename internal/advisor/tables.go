package advisor

import (
	"sort"

	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/report"
)

// HotTables ranks tables by sequential-scan pressure, summing counters across
// snapshots. Only tables with at least minRows rows are considered.
func HotTables(snaps []*collector.Snapshot, minRows int64, limit int) []report.HotTable {
	if len(snaps) == 0 {
		return nil
	}
	type agg struct {
		seq, seqTup, idx int64
	}
	sums := map[string]*agg{}
	for _, s := range snaps {
		for _, t := range s.Tables {
			a := sums[t.QualifiedName()]
			if a == nil {
				a = &agg{}
				sums[t.QualifiedName()] = a
			}
			a.seq += t.SeqScan
			a.seqTup += t.SeqTupRead
			a.idx += t.IdxScan
		}
	}
	indexCount := map[string]int{}
	for _, ix := range snaps[0].Indexes {
		indexCount[ix.QualifiedTable()]++
	}
	var out []report.HotTable
	for _, t := range snaps[0].Tables {
		a := sums[t.QualifiedName()]
		if t.Rows() < minRows || a.seq == 0 {
			continue
		}
		ratio := float64(a.seq) / float64(a.seq+a.idx)
		out = append(out, report.HotTable{
			Schema:     t.Schema,
			Table:      t.Name,
			Rows:       t.Rows(),
			SeqScans:   a.seq,
			SeqTupRead: a.seqTup,
			IdxScans:   a.idx,
			SeqRatio:   ratio,
			TableBytes: t.TableBytes,
			IndexCount: indexCount[t.QualifiedName()],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SeqTupRead != out[j].SeqTupRead {
			return out[i].SeqTupRead > out[j].SeqTupRead
		}
		return out[i].Table < out[j].Table
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
