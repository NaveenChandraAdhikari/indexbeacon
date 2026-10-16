package advisor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/ident"
	"github.com/indexbeacon/indexbeacon/internal/report"
)

// colSig is the comparable identity of one key column.
type colSig struct {
	expr      string
	opclass   uint32
	collation uint32
	option    int16
}

func keySignature(ix collector.Index) []colSig {
	keys := ix.KeyColumns()
	sigs := make([]colSig, len(keys))
	for i, c := range keys {
		s := colSig{expr: c}
		if i < len(ix.Opclasses) {
			s.opclass = ix.Opclasses[i]
		}
		if i < len(ix.Collations) {
			s.collation = ix.Collations[i]
		}
		if i < len(ix.Options) {
			s.option = ix.Options[i]
		}
		sigs[i] = s
	}
	return sigs
}

// isPrefix reports whether a is a (non-strict) prefix of b.
func isPrefix(a, b []colSig) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsAll(haystack, needles []string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Duplicates finds exact duplicates and strict-prefix redundancy. It never
// proposes dropping a primary key, unique or constraint-backing index.
// Scan counts are summed across snapshots; the first snapshot defines the
// catalog.
func Duplicates(snaps []*collector.Snapshot) []report.DuplicateGroup {
	if len(snaps) == 0 {
		return nil
	}
	scans := map[string]int64{}
	for _, s := range snaps {
		for _, ix := range s.Indexes {
			scans[ix.QualifiedName()] += ix.IdxScan
		}
	}
	byTable := map[string][]collector.Index{}
	var tables []string
	for _, ix := range snaps[0].Indexes {
		if !ix.Valid {
			continue
		}
		k := ix.QualifiedTable()
		if _, ok := byTable[k]; !ok {
			tables = append(tables, k)
		}
		byTable[k] = append(byTable[k], ix)
	}
	sort.Strings(tables)

	var out []report.DuplicateGroup
	for _, t := range tables {
		ixs := byTable[t]
		sigs := make([][]colSig, len(ixs))
		for i, ix := range ixs {
			sigs[i] = keySignature(ix)
		}
		for i, a := range ixs {
			if a.Primary || a.Unique || a.ConstraintBacked {
				continue // never drop
			}
			var best *report.DuplicateGroup
			for j, b := range ixs {
				if i == j || a.AccessMethod != b.AccessMethod || a.Predicate != b.Predicate {
					continue
				}
				if !isPrefix(sigs[i], sigs[j]) {
					continue
				}
				if !containsAll(b.Columns, a.IncludeColumns()) {
					continue
				}
				exact := len(sigs[i]) == len(sigs[j]) && containsAll(a.Columns, b.IncludeColumns())
				if exact && !(b.Primary || b.Unique || b.ConstraintBacked) {
					// Both droppable and identical: keep the more-used one,
					// tiebreak on name so only one direction is reported.
					if scans[a.QualifiedName()] > scans[b.QualifiedName()] {
						continue
					}
					if scans[a.QualifiedName()] == scans[b.QualifiedName()] && a.Name < b.Name {
						continue
					}
				}
				g := report.DuplicateGroup{
					Schema:        a.Schema,
					Table:         a.Table,
					Redundant:     a.Name,
					RedundantDef:  a.Definition,
					RedundantSize: a.SizeBytes,
					CoveredBy:     b.Name,
					CoveredByDef:  b.Definition,
					Scans:         scans[a.QualifiedName()],
					DropSQL:       "DROP INDEX CONCURRENTLY " + ident.Qualified(a.Schema, a.Name) + ";",
				}
				if exact {
					g.Kind = "exact"
					g.Reason = fmt.Sprintf("identical key columns %s", describeKeys(a))
				} else {
					g.Kind = "prefix"
					g.Reason = fmt.Sprintf("key columns %s are a leading prefix of %s", describeKeys(a), describeKeys(b))
				}
				if b.Primary {
					g.Reason += " (primary key)"
				} else if b.Unique {
					g.Reason += " (unique)"
				}
				// Prefer reporting against the exact match, else the shortest covering index.
				if best == nil || (g.Kind == "exact" && best.Kind != "exact") ||
					(g.Kind == best.Kind && len(sigs[j]) < len(keySignature(indexByName(ixs, best.CoveredBy)))) {
					gg := g
					best = &gg
				}
			}
			if best != nil {
				out = append(out, *best)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RedundantSize > out[j].RedundantSize })
	return out
}

func indexByName(ixs []collector.Index, name string) collector.Index {
	for _, ix := range ixs {
		if ix.Name == name {
			return ix
		}
	}
	return collector.Index{}
}

func describeKeys(ix collector.Index) string {
	return "(" + strings.Join(ix.KeyColumns(), ", ") + ")"
}
