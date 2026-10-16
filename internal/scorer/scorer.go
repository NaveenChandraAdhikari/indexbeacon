// Package scorer estimates what a proposed index is worth.
//
// The model mirrors how the planner itself decides between a sequential scan
// and an index scan, using the same cost constants, because the goal is to
// predict what the planner will do rather than to invent a separate notion of
// value. Where hypopg is available the estimates here are replaced by the
// planner's own numbers; this package is what makes idxa useful on the many
// servers where that extension cannot be installed.
package scorer

import (
	"math"
	"strings"

	"github.com/indexbeacon/indexbeacon/internal/candidates"
	"github.com/indexbeacon/indexbeacon/internal/parser"
)

// PostgreSQL's default cost constants. Using the same values as the planner
// keeps the comparison meaningful.
const (
	seqPageCost     = 1.0
	randomPageCost  = 4.0
	cpuTupleCost    = 0.01
	cpuIndexCost    = 0.005
	cpuOperatorCost = 0.0025

	// blockSize is PostgreSQL's page size, used to convert row counts into
	// page counts.
	blockSize = 8192

	// btreeEntryOverhead approximates the per-entry bytes a btree spends on
	// the item pointer and tuple header.
	btreeEntryOverhead = 12

	// btreeFillFactor is the default leaf page fill target, so an index is
	// larger than the raw key bytes suggest.
	btreeFillFactor = 0.9
)

// Confidence expresses how much weight to put on an estimate.
type Confidence string

const (
	// ConfidenceHigh means the statistics window is long and the workload
	// came from pg_stat_statements.
	ConfidenceHigh Confidence = "high"
	// ConfidenceMedium means the numbers are usable but the workload was
	// sampled or the window is short.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceLow means selectivity fell back to defaults or statistics
	// are missing, so the ranking is a guess.
	ConfidenceLow Confidence = "low"
)

// TableStats is what the scorer needs to know about a relation.
type TableStats struct {
	// Rows is the estimated live row count.
	Rows float64
	// Pages is relpages, the planner's own page count for the table.
	Pages float64
	// SeqScans and SeqTupRead are the sequential scan counters, summed
	// across nodes.
	SeqScans   int64
	SeqTupRead int64
	// WritesPerSec is the insert, update and delete rate, which is what an
	// extra index costs to maintain.
	WritesPerSec float64
}

// ColumnStats is the planner's statistics for one column.
type ColumnStats struct {
	// NDistinct follows pg_stats semantics: a positive value is a count, a
	// negative value is a fraction of the table.
	NDistinct float64
	// NullFrac is the fraction of rows where the column is null.
	NullFrac float64
	// Correlation is the physical ordering correlation, between -1 and 1.
	// A well-correlated column makes an index scan much cheaper because the
	// heap fetches are close to sequential.
	Correlation float64
	// AvgWidth is the average column width in bytes.
	AvgWidth float64
	// MostCommonFreq is the frequency of the single most common value, used
	// when a query pins the column to a known literal.
	MostCommonFreq float64
	// Known is false when the column has never been analyzed.
	Known bool
}

// DistinctValues resolves NDistinct against a row count.
func (c ColumnStats) DistinctValues(rows float64) float64 {
	switch {
	case c.NDistinct > 0:
		return c.NDistinct
	case c.NDistinct < 0:
		return math.Max(1, -c.NDistinct*rows)
	default:
		return 0
	}
}

// Input is everything needed to score one candidate.
type Input struct {
	// Table statistics for the candidate's relation.
	Table TableStats
	// Columns maps column name to its statistics.
	Columns map[string]ColumnStats
	// WindowSeconds is how long the statistics have been accumulating,
	// which converts call counts into rates.
	WindowSeconds float64
	// Sampled is true when the workload came from sampling rather than from
	// pg_stat_statements, which lowers confidence.
	Sampled bool
	// Shapes maps a query fingerprint to its parsed shape, so selectivity
	// can be computed from the actual predicates.
	Shapes map[string]*parser.Shape
}

// Score is the result of evaluating a candidate.
type Score struct {
	// Benefit is the estimated planner cost saved per second.
	Benefit float64 `json:"benefit"`
	// WritePenalty is the estimated cost per second of maintaining the
	// index as rows change.
	WritePenalty float64 `json:"write_penalty"`
	// Net is Benefit minus WritePenalty, the number recommendations are
	// ranked by.
	Net float64 `json:"net"`
	// EstimatedBytes is the predicted on-disk size.
	EstimatedBytes int64 `json:"estimated_bytes"`
	// Selectivity is the estimated fraction of rows the index would match.
	Selectivity float64 `json:"selectivity"`
	// RowsMatched is Selectivity times the table's row count.
	RowsMatched float64 `json:"rows_matched"`
	// SeqScanShare is the fraction of the table's sequential scans that the
	// covered queries account for, capped at one.
	SeqScanShare float64 `json:"seq_scan_share"`
	// Confidence qualifies the whole estimate.
	Confidence Confidence `json:"confidence"`
	// Reasons explains in words why the candidate scored as it did, so a
	// recommendation can justify itself.
	Reasons []string `json:"reasons,omitempty"`
}

// Score evaluates one candidate against collected statistics.
func Score_(c *candidates.Candidate, in Input) Score {
	return ScoreCandidate(c, in)
}

// ScoreCandidate evaluates one candidate against collected statistics.
func ScoreCandidate(c *candidates.Candidate, in Input) Score {
	var s Score

	rows := math.Max(in.Table.Rows, 1)
	pages := math.Max(in.Table.Pages, 1)
	window := in.WindowSeconds
	if window <= 0 {
		window = 1
	}

	// Selectivity of the whole candidate: the product of its columns'
	// selectivities, floored so a single row is the most optimistic case.
	sel, known := s.estimateSelectivity(c, in, rows)
	s.Selectivity = sel
	s.RowsMatched = math.Max(1, sel*rows)

	// The planner's sequential scan cost for this table.
	seqCost := seqPageCost*pages + cpuTupleCost*rows

	// The index scan cost. Heap fetches dominate, and how expensive they are
	// depends on physical correlation: a perfectly correlated column reads
	// nearly sequentially, an uncorrelated one pays random page cost per row.
	corr := s.leadingCorrelation(c, in)
	heapPages := math.Min(s.RowsMatched, pages)
	pageCost := randomPageCost*(1-math.Abs(corr)) + seqPageCost*math.Abs(corr)
	indexHeight := math.Max(1, math.Log2(math.Max(rows, 2)))
	idxCost := heapPages*pageCost +
		cpuIndexCost*s.RowsMatched +
		cpuTupleCost*s.RowsMatched +
		cpuOperatorCost*indexHeight

	savingPerExecution := math.Max(0, seqCost-idxCost)

	// Benefit accrues per execution of every query the index serves, but only
	// for executions that are actually paying for a sequential scan.
	//
	// This bound matters more than it looks. A query can run half a million
	// times against a table that the server has scanned sequentially only
	// twice, which means the planner is already serving it from an existing
	// index and there is nothing to save. Multiplying calls by the full
	// sequential scan cost in that situation invents an enormous benefit for
	// an index nobody needs, and those phantom recommendations crowd out the
	// real ones. Capping the benefiting executions at the number of observed
	// sequential scans keeps the estimate tied to something measured.
	var calls int64
	for _, q := range c.Queries {
		calls += q.Calls
	}
	benefiting := float64(calls)
	if in.Table.SeqScans > 0 && float64(in.Table.SeqScans) < benefiting {
		benefiting = float64(in.Table.SeqScans)
	}
	s.Benefit = benefiting / window * savingPerExecution

	// Maintenance cost: every insert, update and delete must also update the
	// index, and a wider index costs more.
	width := 1 + 0.15*float64(len(c.Columns)-1)
	s.WritePenalty = in.Table.WritesPerSec * (cpuIndexCost + randomPageCost*0.1) * width

	s.Net = s.Benefit - s.WritePenalty
	s.EstimatedBytes = s.estimateSize(c, in, rows, sel)

	if calls > 0 && in.Table.SeqScans > 0 {
		// The share of this candidate's executions that could have been a
		// sequential scan of the table.
		s.SeqScanShare = math.Min(1, float64(in.Table.SeqScans)/float64(calls))
	}

	s.Confidence = confidenceFor(in, known)
	s.Reasons = s.explain(c, in, seqCost, idxCost, calls)
	return s
}

// estimateSelectivity multiplies the selectivity of each indexed column,
// using the predicates of the queries the candidate serves.
func (s *Score) estimateSelectivity(c *candidates.Candidate, in Input, rows float64) (float64, bool) {
	sel := 1.0
	allKnown := true

	for _, col := range c.Columns {
		op := s.opForColumn(c, in, col.Name)
		if op == "" {
			// A trailing ordering column does not restrict rows.
			continue
		}
		cs, ok := in.Columns[col.Name]
		if !ok || !cs.Known {
			allKnown = false
			sel *= defaultSelectivity(op)
			continue
		}
		sel *= columnSelectivity(op, cs, rows)
	}

	if c.Predicate != "" {
		// A partial index only contains the matching rows, which makes it
		// both smaller and more selective.
		if col, val := parsePredicate(c.Predicate); col != "" {
			if cs, ok := in.Columns[col]; ok && cs.Known && cs.MostCommonFreq > 0 {
				_ = val
				sel *= cs.MostCommonFreq
			}
		}
	}

	// Never claim to match less than one row.
	if sel < 1/rows {
		sel = 1 / rows
	}
	if sel > 1 {
		sel = 1
	}
	return sel, allKnown
}

// opForColumn finds how a column is constrained by the queries this candidate
// serves, which decides its selectivity.
func (s *Score) opForColumn(c *candidates.Candidate, in Input, column string) parser.PredicateOp {
	table := c.QualifiedTable()
	best := parser.PredicateOp("")
	for _, q := range c.Queries {
		shape, ok := in.Shapes[q.Fingerprint]
		if !ok {
			continue
		}
		for _, p := range shape.PredicatesFor(table) {
			if p.Column != column {
				continue
			}
			if best == "" || rankOp(p.Op) > rankOp(best) {
				best = p.Op
			}
		}
	}
	return best
}

func rankOp(op parser.PredicateOp) int {
	switch op {
	case parser.OpEq:
		return 5
	case parser.OpIn:
		return 4
	case parser.OpIsNull:
		return 3
	case parser.OpRange, parser.OpLikePrefix:
		return 2
	case parser.OpContains, parser.OpTextSearch, parser.OpLikeContains:
		// These cannot be served by a btree, so they are ranked alongside a
		// range predicate for the purpose of choosing a leading column.
		return 2
	default:
		return 1
	}
}

// columnSelectivity estimates the fraction of rows a predicate keeps, using
// the same rules the planner uses.
func columnSelectivity(op parser.PredicateOp, cs ColumnStats, rows float64) float64 {
	switch op {
	case parser.OpEq:
		distinct := cs.DistinctValues(rows)
		if distinct <= 0 {
			return defaultSelectivity(op)
		}
		return math.Min(1, (1-cs.NullFrac)/distinct)
	case parser.OpIn:
		distinct := cs.DistinctValues(rows)
		if distinct <= 0 {
			return defaultSelectivity(op)
		}
		// Without the list length, assume a handful of values.
		return math.Min(1, 3*(1-cs.NullFrac)/distinct)
	case parser.OpIsNull:
		return math.Max(cs.NullFrac, 1/math.Max(rows, 1))
	case parser.OpIsNotNull:
		return math.Max(0.01, 1-cs.NullFrac)
	case parser.OpRange:
		// The planner's default for an unbounded inequality.
		return 0.3333333
	case parser.OpLikePrefix:
		return 0.01
	case parser.OpContains:
		// Containment on jsonb or arrays is normally looking for a specific
		// key or element, which matches a small share of rows. The planner
		// itself assumes 0.001 for these operators.
		return 0.005
	case parser.OpTextSearch:
		// A full-text match against a real query is selective; PostgreSQL's
		// own default for @@ is 0.005.
		return 0.005
	case parser.OpLikeContains:
		// An unanchored pattern served by a trigram index. Less selective
		// than an anchored one, but far from matching everything.
		return 0.02
	default:
		return 1
	}
}

func defaultSelectivity(op parser.PredicateOp) float64 {
	switch op {
	case parser.OpEq:
		return 0.005
	case parser.OpIn:
		return 0.015
	case parser.OpRange:
		return 0.3333333
	case parser.OpLikePrefix:
		return 0.01
	case parser.OpContains, parser.OpTextSearch:
		return 0.005
	case parser.OpLikeContains:
		return 0.02
	case parser.OpIsNull:
		return 0.05
	case parser.OpIsNotNull:
		return 0.95
	default:
		return 1
	}
}

// leadingCorrelation returns the physical correlation of the index's first
// column, which is what determines how sequential the heap fetches are.
func (s *Score) leadingCorrelation(c *candidates.Candidate, in Input) float64 {
	if len(c.Columns) == 0 {
		return 0
	}
	if cs, ok := in.Columns[c.Columns[0].Name]; ok && cs.Known {
		return cs.Correlation
	}
	return 0
}

// estimateSize predicts the on-disk size of the index.
func (s *Score) estimateSize(c *candidates.Candidate, in Input, rows, sel float64) int64 {
	var width float64
	for _, col := range c.Columns {
		if cs, ok := in.Columns[col.Name]; ok && cs.AvgWidth > 0 {
			width += cs.AvgWidth
		} else {
			// A reasonable default for an unanalyzed column.
			width += 8
		}
	}
	entries := rows
	if c.Predicate != "" {
		// A partial index holds only the matching rows.
		entries = math.Max(1, rows*sel)
	}
	bytes := entries * (width + btreeEntryOverhead) / btreeFillFactor
	return int64(math.Ceil(bytes))
}

// explain renders the reasoning in words, so a recommendation can justify
// itself rather than presenting a bare number.
func (s *Score) explain(c *candidates.Candidate, in Input, seqCost, idxCost float64, calls int64) []string {
	var out []string

	if in.Table.SeqScans > 0 {
		out = append(out, formatf("table has %s sequential scans reading %s rows",
			humanCount(float64(in.Table.SeqScans)), humanCount(float64(in.Table.SeqTupRead))))
	}
	if calls > 0 {
		out = append(out, formatf("%s calls across %d quer%s in the statistics window",
			humanCount(float64(calls)), len(c.Queries), plural(len(c.Queries))))
	}
	if in.Table.SeqScans > 0 && float64(in.Table.SeqScans) < float64(calls) {
		out = append(out, formatf(
			"only %s of those could have been sequential scans, so the benefit is capped accordingly",
			humanCount(float64(in.Table.SeqScans))))
	}
	if s.Selectivity > 0 {
		out = append(out, formatf("estimated to match %s of %s rows (%.3f%%)",
			humanCount(s.RowsMatched), humanCount(in.Table.Rows), s.Selectivity*100))
	}
	if seqCost > idxCost {
		out = append(out, formatf("planner cost per execution falls from %s to %s",
			humanCount(seqCost), humanCount(idxCost)))
	} else {
		out = append(out, "the index is not cheaper than a sequential scan for these queries")
	}
	if in.Table.WritesPerSec > 0 {
		out = append(out, formatf("table takes %.1f writes per second, which this index must also maintain",
			in.Table.WritesPerSec))
	}
	return out
}

// confidenceFor grades the estimate.
func confidenceFor(in Input, statsKnown bool) Confidence {
	switch {
	case !statsKnown:
		return ConfidenceLow
	case in.Sampled:
		return ConfidenceMedium
	case in.WindowSeconds < 3600:
		return ConfidenceMedium
	default:
		return ConfidenceHigh
	}
}

// parsePredicate pulls the column and literal out of a simple partial index
// predicate of the form `col = 'value'`.
func parsePredicate(pred string) (col, val string) {
	parts := strings.SplitN(pred, "=", 2)
	if len(parts) != 2 {
		return "", ""
	}
	col = strings.Trim(strings.TrimSpace(parts[0]), `"`)
	val = strings.Trim(strings.TrimSpace(parts[1]), "'")
	return col, val
}
