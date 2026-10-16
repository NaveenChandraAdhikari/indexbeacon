package scorer

import (
	"math"
	"strings"
	"testing"

	"github.com/indexbeacon/indexbeacon/internal/candidates"
	"github.com/indexbeacon/indexbeacon/internal/parser"
)

// build assembles a candidate plus the statistics to score it against.
func build(t *testing.T, sql string, calls int64, tbl TableStats, cols map[string]ColumnStats) (*candidates.Candidate, Input) {
	t.Helper()
	shape, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ref := candidates.QueryRef{Fingerprint: "f1", SQL: sql, Calls: calls}
	cands := candidates.Generate(shape, ref, candidates.Options{})
	if len(cands) == 0 {
		t.Fatalf("no candidates for %q", sql)
	}
	in := Input{
		Table:         tbl,
		Columns:       cols,
		WindowSeconds: 3600,
		Shapes:        map[string]*parser.Shape{"f1": shape},
	}
	return cands[0], in
}

// bigTable is a table worth indexing: many rows, heavily scanned.
func bigTable() TableStats {
	return TableStats{
		Rows: 1_000_000, Pages: 15_000,
		SeqScans: 12_000, SeqTupRead: 5_000_000_000,
		WritesPerSec: 5,
	}
}

func selectiveColumn() map[string]ColumnStats {
	return map[string]ColumnStats{
		"customer_id": {NDistinct: 20000, Correlation: 0.1, AvgWidth: 8, Known: true},
		"created_at":  {NDistinct: -0.9, Correlation: 0.95, AvgWidth: 8, Known: true},
		"status":      {NDistinct: 5, Correlation: 0.3, AvgWidth: 9, MostCommonFreq: 0.9, Known: true},
	}
}

func TestSelectiveColumnOnLargeTableScoresWell(t *testing.T) {
	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 100000, bigTable(), selectiveColumn())
	got := ScoreCandidate(c, in)

	if got.Benefit <= 0 {
		t.Errorf("benefit = %v, want positive for a selective column on a scanned table", got.Benefit)
	}
	if got.Net <= 0 {
		t.Errorf("net = %v, want positive", got.Net)
	}
	// 1M rows over 20k distinct customers is about 50 rows per customer.
	if got.RowsMatched > 1000 {
		t.Errorf("rows matched = %v, expected roughly 50", got.RowsMatched)
	}
}

func TestUnselectiveColumnScoresPoorly(t *testing.T) {
	// A column with five distinct values matches a fifth of the table, so an
	// index scan is not obviously better than reading the table.
	cols := map[string]ColumnStats{
		"status": {NDistinct: 5, Correlation: 0.1, AvgWidth: 9, Known: true},
	}
	c, in := build(t, `SELECT * FROM orders WHERE status = $1`, 100000, bigTable(), cols)
	got := ScoreCandidate(c, in)

	selective, selectiveIn := build(t, `SELECT * FROM orders WHERE customer_id = $1`,
		100000, bigTable(), selectiveColumn())
	better := ScoreCandidate(selective, selectiveIn)

	if got.Net >= better.Net {
		t.Errorf("unselective column scored %v, selective scored %v; ordering is wrong",
			got.Net, better.Net)
	}
}

func TestMoreCallsMeansMoreBenefit(t *testing.T) {
	// Benefit is per unit time, so a query run ten times as often is worth
	// ten times as much.
	low, lowIn := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 1000, bigTable(), selectiveColumn())
	high, highIn := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 100000, bigTable(), selectiveColumn())

	lowScore := ScoreCandidate(low, lowIn)
	highScore := ScoreCandidate(high, highIn)

	if highScore.Benefit <= lowScore.Benefit {
		t.Errorf("benefit did not increase with call count: %v vs %v",
			lowScore.Benefit, highScore.Benefit)
	}
}

func TestSmallTableIsNotWorthIndexing(t *testing.T) {
	// Reading fifty rows costs less than descending an index, so the model
	// must not claim a saving.
	small := TableStats{Rows: 50, Pages: 1, SeqScans: 1000, WritesPerSec: 1}
	cols := map[string]ColumnStats{"customer_id": {NDistinct: 50, AvgWidth: 8, Known: true}}
	c, in := build(t, `SELECT * FROM tiny WHERE customer_id = $1`, 10000, small, cols)

	if got := ScoreCandidate(c, in); got.Benefit > 0 {
		t.Errorf("benefit = %v on a 50-row table, want none", got.Benefit)
	}
}

func TestWritePenaltyGrowsWithWidthAndWriteRate(t *testing.T) {
	cols := selectiveColumn()

	narrow, narrowIn := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 100, bigTable(), cols)
	wide, wideIn := build(t, `SELECT * FROM orders WHERE customer_id = $1 ORDER BY created_at DESC`,
		100, bigTable(), cols)

	narrowScore := ScoreCandidate(narrow, narrowIn)
	wideScore := ScoreCandidate(wide, wideIn)
	if len(wide.Columns) <= len(narrow.Columns) {
		t.Skip("expected the ordered candidate to be wider")
	}
	if wideScore.WritePenalty <= narrowScore.WritePenalty {
		t.Errorf("a wider index should cost more to maintain: %v vs %v",
			wideScore.WritePenalty, narrowScore.WritePenalty)
	}

	busy := bigTable()
	busy.WritesPerSec = 500
	_, busyIn := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 100, busy, cols)
	busyScore := ScoreCandidate(narrow, busyIn)
	if busyScore.WritePenalty <= narrowScore.WritePenalty {
		t.Errorf("a busier table should raise the maintenance cost: %v vs %v",
			busyScore.WritePenalty, narrowScore.WritePenalty)
	}
}

func TestHighWriteRateCanOutweighBenefit(t *testing.T) {
	// An index on a rarely queried but constantly written table is a net
	// loss, and the model must be able to say so.
	tbl := bigTable()
	tbl.WritesPerSec = 50000
	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 1, tbl, selectiveColumn())

	if got := ScoreCandidate(c, in); got.Net >= 0 {
		t.Errorf("net = %v, expected a negative score when writes dominate", got.Net)
	}
}

func TestCorrelationReducesIndexCost(t *testing.T) {
	// A physically correlated column makes heap fetches nearly sequential,
	// which is why a timestamp column indexes so well on append-only tables.
	uncorrelated := map[string]ColumnStats{
		"customer_id": {NDistinct: 200, Correlation: 0, AvgWidth: 8, Known: true},
	}
	correlated := map[string]ColumnStats{
		"customer_id": {NDistinct: 200, Correlation: 1, AvgWidth: 8, Known: true},
	}

	c1, in1 := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 10000, bigTable(), uncorrelated)
	c2, in2 := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 10000, bigTable(), correlated)

	if ScoreCandidate(c2, in2).Benefit <= ScoreCandidate(c1, in1).Benefit {
		t.Error("a correlated column should yield a cheaper index scan and more benefit")
	}
}

func TestSizeEstimateTracksColumnWidth(t *testing.T) {
	narrow := map[string]ColumnStats{"customer_id": {NDistinct: 1000, AvgWidth: 8, Known: true}}
	wide := map[string]ColumnStats{"customer_id": {NDistinct: 1000, AvgWidth: 200, Known: true}}

	c1, in1 := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 100, bigTable(), narrow)
	c2, in2 := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 100, bigTable(), wide)

	small := ScoreCandidate(c1, in1).EstimatedBytes
	big := ScoreCandidate(c2, in2).EstimatedBytes
	if big <= small {
		t.Errorf("wider column should give a bigger index: %d vs %d", big, small)
	}
	// A million 8-byte entries is on the order of 20MB, not 20KB or 2GB.
	if small < 10*1024*1024 || small > 100*1024*1024 {
		t.Errorf("size estimate %s is implausible for 1M narrow rows", HumanBytes(small))
	}
}

func TestPartialIndexIsEstimatedSmaller(t *testing.T) {
	cols := selectiveColumn()
	full := &candidates.Candidate{Table: "orders", Columns: []candidates.Column{{Name: "created_at"}}}
	partial := &candidates.Candidate{
		Table:     "orders",
		Columns:   []candidates.Column{{Name: "created_at"}},
		Predicate: "status = 'pending'",
	}
	in := Input{Table: bigTable(), Columns: cols, WindowSeconds: 3600}

	fullSize := ScoreCandidate(full, in).EstimatedBytes
	partialSize := ScoreCandidate(partial, in).EstimatedBytes
	if partialSize >= fullSize {
		t.Errorf("partial index should be smaller: %s vs %s",
			HumanBytes(partialSize), HumanBytes(fullSize))
	}
}

func TestConfidenceReflectsDataQuality(t *testing.T) {
	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 1000, bigTable(), selectiveColumn())

	in.WindowSeconds = 7200
	in.Sampled = false
	if got := ScoreCandidate(c, in).Confidence; got != ConfidenceHigh {
		t.Errorf("confidence = %q, want high for a long window from query statistics", got)
	}

	in.Sampled = true
	if got := ScoreCandidate(c, in).Confidence; got != ConfidenceMedium {
		t.Errorf("confidence = %q, want medium for a sampled workload", got)
	}

	in.Sampled = false
	in.WindowSeconds = 60
	if got := ScoreCandidate(c, in).Confidence; got != ConfidenceMedium {
		t.Errorf("confidence = %q, want medium for a short window", got)
	}

	// Unanalyzed columns mean the selectivity is a guess.
	in.WindowSeconds = 7200
	in.Columns = map[string]ColumnStats{}
	if got := ScoreCandidate(c, in).Confidence; got != ConfidenceLow {
		t.Errorf("confidence = %q, want low without column statistics", got)
	}
}

func TestMissingStatisticsStillProducesAnEstimate(t *testing.T) {
	// A table that has never been analyzed must not crash the scorer, it
	// must fall back to the planner's own default selectivities.
	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 10000, bigTable(), map[string]ColumnStats{})
	got := ScoreCandidate(c, in)

	if math.IsNaN(got.Net) || math.IsInf(got.Net, 0) {
		t.Errorf("net = %v with no statistics", got.Net)
	}
	if got.Selectivity <= 0 || got.Selectivity > 1 {
		t.Errorf("selectivity = %v, want a fraction", got.Selectivity)
	}
}

func TestSelectivityIsAlwaysAFraction(t *testing.T) {
	cases := []struct {
		name string
		cols map[string]ColumnStats
	}{
		{"zero distinct", map[string]ColumnStats{"customer_id": {NDistinct: 0, Known: true}}},
		{"negative fraction", map[string]ColumnStats{"customer_id": {NDistinct: -1, Known: true}}},
		{"all null", map[string]ColumnStats{"customer_id": {NDistinct: 1, NullFrac: 1, Known: true}}},
		{"unknown", map[string]ColumnStats{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 100, bigTable(), tc.cols)
			got := ScoreCandidate(c, in)
			if got.Selectivity <= 0 || got.Selectivity > 1 {
				t.Errorf("selectivity = %v, must stay within (0,1]", got.Selectivity)
			}
			if got.RowsMatched < 1 {
				t.Errorf("rows matched = %v, must be at least one", got.RowsMatched)
			}
		})
	}
}

func TestRangePredicateIsLessSelectiveThanEquality(t *testing.T) {
	cols := map[string]ColumnStats{
		"occurred_at": {NDistinct: -0.9, Correlation: 0.9, AvgWidth: 8, Known: true},
	}
	eq, eqIn := build(t, `SELECT * FROM events WHERE occurred_at = $1`, 1000, bigTable(), cols)
	rng, rngIn := build(t, `SELECT * FROM events WHERE occurred_at > $1`, 1000, bigTable(), cols)

	eqSel := ScoreCandidate(eq, eqIn).Selectivity
	rngSel := ScoreCandidate(rng, rngIn).Selectivity
	if rngSel <= eqSel {
		t.Errorf("range selectivity %v should exceed equality %v", rngSel, eqSel)
	}
}

func TestSeqScanShareIsCapped(t *testing.T) {
	tbl := bigTable()
	tbl.SeqScans = 100
	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 999999, tbl, selectiveColumn())

	if got := ScoreCandidate(c, in).SeqScanShare; got > 1 {
		t.Errorf("share = %v, must be capped at 1", got)
	}
}

func TestExplanationIsProduced(t *testing.T) {
	// A recommendation nobody understands does not get acted on.
	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 100000, bigTable(), selectiveColumn())
	got := ScoreCandidate(c, in)

	if len(got.Reasons) == 0 {
		t.Fatal("expected the score to explain itself")
	}
	joined := strings.Join(got.Reasons, " ")
	for _, want := range []string{"sequential scans", "calls", "match"} {
		if !strings.Contains(joined, want) {
			t.Errorf("explanation missing %q:\n%s", want, joined)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:               "512 bytes",
		2048:              "2.0 KiB",
		5 * 1024 * 1024:   "5.0 MiB",
		3 * 1024746 * 700: "2.0 GiB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanCount(t *testing.T) {
	cases := map[float64]string{
		5:       "5",
		42:      "42",
		1500:    "1.5k",
		2000000: "2M",
		5.67e9:  "5.7G",
	}
	for in, want := range cases {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%v) = %q, want %q", in, got, want)
		}
	}
	if got := humanCount(math.NaN()); got != "unknown" {
		t.Errorf("humanCount(NaN) = %q", got)
	}
}

func TestDistinctValues(t *testing.T) {
	// pg_stats reports a negative n_distinct as a fraction of the table.
	if got := (ColumnStats{NDistinct: -0.5}).DistinctValues(1000); got != 500 {
		t.Errorf("negative n_distinct resolved to %v, want 500", got)
	}
	if got := (ColumnStats{NDistinct: 42}).DistinctValues(1000); got != 42 {
		t.Errorf("positive n_distinct resolved to %v, want 42", got)
	}
	if got := (ColumnStats{NDistinct: 0}).DistinctValues(1000); got != 0 {
		t.Errorf("unknown n_distinct resolved to %v, want 0", got)
	}
}

// TestBenefitIsCappedByObservedSequentialScans pins down the most damaging
// false positive found by running the advisor against a live server.
//
// PostgreSQL's own foreign key checks run `SELECT 1 FROM t WHERE id = $1 FOR
// KEY SHARE` hundreds of thousands of times. Multiplying that call count by a
// full sequential scan cost produced a benefit two orders of magnitude larger
// than any real recommendation, for an index the server plainly did not need:
// the table had only been scanned sequentially a couple of thousand times in
// the same window, because the planner was already using the primary key.
func TestBenefitIsCappedByObservedSequentialScans(t *testing.T) {
	tbl := bigTable()
	tbl.SeqScans = 10 // the table is essentially never scanned
	tbl.SeqTupRead = 1000

	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 500_000, tbl, selectiveColumn())
	capped := ScoreCandidate(c, in)

	// The same query against a table that really is being scanned.
	scanned := bigTable()
	scanned.SeqScans = 500_000
	_, scannedIn := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 500_000, scanned, selectiveColumn())
	real := ScoreCandidate(c, scannedIn)

	if capped.Benefit >= real.Benefit {
		t.Errorf("benefit was not capped: %v on an unscanned table vs %v on a scanned one",
			capped.Benefit, real.Benefit)
	}
	// Ten scans out of five hundred thousand calls is a factor of fifty
	// thousand, so the capped benefit must be dramatically smaller.
	if capped.Benefit > real.Benefit/1000 {
		t.Errorf("cap was too weak: %v vs %v", capped.Benefit, real.Benefit)
	}
}

func TestBenefitIsNotCappedWhenScansExceedCalls(t *testing.T) {
	// Parallel workers each count a sequential scan, so scans can exceed
	// calls. The cap must never inflate the benefit above the call count.
	tbl := bigTable()
	tbl.SeqScans = 1_000_000

	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 1000, tbl, selectiveColumn())
	got := ScoreCandidate(c, in)

	uncapped := bigTable()
	uncapped.SeqScans = 1000
	_, uncappedIn := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 1000, uncapped, selectiveColumn())
	same := ScoreCandidate(c, uncappedIn)

	if got.Benefit != same.Benefit {
		t.Errorf("benefit changed when scans exceeded calls: %v vs %v", got.Benefit, same.Benefit)
	}
}

func TestSeqScanShareReportsCoverage(t *testing.T) {
	tbl := bigTable()
	tbl.SeqScans = 250
	c, in := build(t, `SELECT * FROM orders WHERE customer_id = $1`, 1000, tbl, selectiveColumn())

	got := ScoreCandidate(c, in).SeqScanShare
	if got <= 0 || got > 1 {
		t.Fatalf("share = %v, want a fraction", got)
	}
	if got < 0.2 || got > 0.3 {
		t.Errorf("share = %v, want about 0.25 for 250 scans across 1000 calls", got)
	}
}
