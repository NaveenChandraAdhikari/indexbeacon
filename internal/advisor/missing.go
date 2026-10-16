package advisor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/indexbeacon/indexbeacon/internal/candidates"
	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/parser"
	"github.com/indexbeacon/indexbeacon/internal/report"
	"github.com/indexbeacon/indexbeacon/internal/scorer"
	"github.com/indexbeacon/indexbeacon/internal/workload"
)

// MissingOptions tunes the missing-index analysis.
type MissingOptions struct {
	// MinRows skips tables smaller than this. A sequential scan of a small
	// table is cheaper than an index lookup, so recommending one is wrong.
	MinRows int64
	// MaxRecommendations caps how many recommendations are returned.
	MaxRecommendations int
	// MaxColumns caps composite index width.
	MaxColumns int
	// Partial enables partial index candidates.
	Partial bool
	// MinNet drops candidates whose net score falls below this, so marginal
	// suggestions do not crowd out real ones.
	MinNet float64
	// MaxTables limits how many tables are considered on very wide schemas,
	// keeping the analysis bounded. Tables are chosen by scan pressure.
	MaxTables int
	// Workload, when set, replaces whatever the nodes reported with a
	// recording made by the sample command.
	Workload *workload.Workload
}

func (o MissingOptions) withDefaults() MissingOptions {
	if o.MinRows <= 0 {
		o.MinRows = 1000
	}
	if o.MaxRecommendations <= 0 {
		o.MaxRecommendations = 10
	}
	if o.MaxColumns <= 0 {
		o.MaxColumns = candidates.MaxKeyColumns
	}
	if o.MaxTables <= 0 {
		o.MaxTables = 2000
	}
	return o
}

// FromWorkload converts a sampled recording into the statement form the
// analysis works from, so a sampled workload and pg_stat_statements follow
// exactly the same path from here on.
func FromWorkload(w *workload.Workload) []collector.Statement {
	if w == nil {
		return nil
	}
	out := make([]collector.Statement, 0, len(w.Queries))
	for _, q := range w.Queries {
		text := q.SQL
		if q.Literals != "" {
			// A recording kept with literals lets partial index candidates be
			// generated, which the normalised form cannot support.
			text = q.Literals
		}
		mean := 0.0
		if q.EstimatedCalls > 0 {
			mean = q.TotalTimeMS / float64(q.EstimatedCalls)
		}
		out = append(out, collector.Statement{
			Query:         text,
			Calls:         q.EstimatedCalls,
			TotalExecTime: q.TotalTimeMS,
			MeanExecTime:  mean,
		})
	}
	return out
}

// MissingResult carries the recommendations plus anything the caller should
// tell the user about coverage.
type MissingResult struct {
	Recommendations []report.Recommendation
	Warnings        []string
	// Parsed and Skipped count how many workload statements could be used,
	// so the report can be honest about how much of the workload it saw.
	Parsed  int
	Skipped int
}

// Missing analyses the workload and proposes indexes.
//
// The pipeline is: parse each statement, generate candidates per statement,
// merge them into a non-redundant set, score each against table and column
// statistics, then rank. Counters are summed across nodes first, because an
// index used only on one replica is still an index in use, and a table
// scanned on three replicas is scanned three times.
func Missing(snaps []*collector.Snapshot, opts MissingOptions) MissingResult {
	opts = opts.withDefaults()
	var res MissingResult

	if len(snaps) == 0 {
		return res
	}

	merged := mergeSnapshots(snaps)
	if opts.Workload != nil {
		merged.statements = FromWorkload(opts.Workload)
		// Sampling gives estimates, not counts, which the scorer reflects as
		// lower confidence.
		merged.sampled = true
		if d := opts.Workload.Duration().Seconds(); d > 0 {
			merged.windowSeconds = d
		}
	}
	if len(merged.statements) == 0 {
		return res
	}

	// Tables worth considering, chosen by scan pressure so a very wide
	// schema does not turn into an unbounded analysis.
	eligible := eligibleTables(merged, opts)
	resolveSchema := schemaResolver(merged)

	// Parse the workload once and keep the shapes for scoring.
	shapes := map[string]*parser.Shape{}
	var allCandidates []*candidates.Candidate

	genOpts := candidates.Options{
		MaxColumns: opts.MaxColumns,
		Partial:    opts.Partial,
		// Column types decide the index type. Without this every candidate
		// would be a btree, which the planner cannot use for containment,
		// overlap or full-text predicates.
		ColumnType: func(table, column string) (candidates.ColumnType, bool) {
			c, ok := merged.columns[table+"."+column]
			if !ok {
				return candidates.ColumnType{}, false
			}
			return candidates.ColumnType{
				Name:        c.Type,
				Correlation: c.Correlation,
				Known:       c.HasStats,
			}, true
		},
		Rows: func(table string) int64 {
			return merged.tables[table].rows
		},
		Selectivity: func(table, column string) (float64, bool) {
			cs, ok := merged.columns[table+"."+column]
			if !ok || !cs.HasStats {
				return 0, false
			}
			t, ok := merged.tables[table]
			if !ok {
				return 0, false
			}
			distinct := cs.DistinctValues(t.rows)
			if distinct <= 0 {
				return 0, false
			}
			return 1 / distinct, true
		},
	}

	for _, st := range merged.statements {
		shape, err := parser.Parse(st.Query)
		if err != nil {
			res.Skipped++
			continue
		}
		if shape.Unparsed != "" || len(shape.Predicates) == 0 {
			res.Skipped++
			continue
		}
		res.Parsed++
		shape.Qualify(resolveSchema)

		fingerprint, err := parser.Fingerprint(st.Query)
		if err != nil {
			fingerprint = st.Query
		}
		shapes[fingerprint] = shape

		ref := candidates.QueryRef{
			Fingerprint: fingerprint,
			SQL:         st.Query,
			Calls:       st.Calls,
			TotalTimeMS: st.TotalExecTime,
		}

		for _, c := range candidates.Generate(shape, ref, genOpts) {
			if !eligible[c.QualifiedTable()] {
				continue
			}
			allCandidates = append(allCandidates, c)
		}
	}

	if res.Skipped > 0 {
		res.Warnings = append(res.Warnings, formatSkipped(res.Parsed, res.Skipped))
	}
	if len(allCandidates) == 0 {
		return res
	}

	final := candidates.Merge(allCandidates, merged.existingIndexes())

	// Score and rank.
	type scored struct {
		cand  *candidates.Candidate
		score scorer.Score
	}
	var results []scored
	for _, c := range final {
		t, ok := merged.tables[c.QualifiedTable()]
		if !ok {
			continue
		}
		in := scorer.Input{
			Table: scorer.TableStats{
				Rows:         float64(t.rows),
				Pages:        float64(t.pages),
				SeqScans:     t.seqScan,
				SeqTupRead:   t.seqTupRead,
				WritesPerSec: t.writesPerSec(merged.windowSeconds),
			},
			Columns:       merged.columnStatsFor(c.QualifiedTable()),
			WindowSeconds: merged.windowSeconds,
			Sampled:       merged.sampled,
			Shapes:        shapes,
		}
		s := scorer.ScoreCandidate(c, in)
		if s.Net < opts.MinNet {
			continue
		}
		results = append(results, scored{cand: c, score: s})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score.Net != results[j].score.Net {
			return results[i].score.Net > results[j].score.Net
		}
		// Break ties deterministically so output is stable between runs.
		return results[i].cand.Key() < results[j].cand.Key()
	})

	if len(results) > opts.MaxRecommendations {
		results = results[:opts.MaxRecommendations]
	}

	for i, r := range results {
		res.Recommendations = append(res.Recommendations, toRecommendation(i+1, r.cand, r.score))
	}
	return res
}

// toRecommendation converts a scored candidate into the report model.
func toRecommendation(rank int, c *candidates.Candidate, s scorer.Score) report.Recommendation {
	cols := make([]string, 0, len(c.Columns))
	for _, col := range c.Columns {
		if col.Desc {
			cols = append(cols, col.Name+" DESC")
		} else {
			cols = append(cols, col.Name)
		}
	}

	rules := map[string]bool{}
	queries := make([]report.CoveredQuery, 0, len(c.Queries))
	for _, q := range c.Queries {
		if q.Rule != "" {
			rules[q.Rule] = true
		}
		queries = append(queries, report.CoveredQuery{
			Fingerprint: q.Fingerprint,
			Calls:       q.Calls,
			TotalTimeMS: q.TotalTimeMS,
			Query:       q.SQL,
		})
	}
	sort.Slice(queries, func(i, j int) bool { return queries[i].Calls > queries[j].Calls })

	ruleList := make([]string, 0, len(rules))
	for r := range rules {
		ruleList = append(ruleList, r)
	}
	sort.Strings(ruleList)

	notes := s.Reasons
	if c.Reason != "" {
		notes = append([]string{c.Reason}, notes...)
	}
	if ext := c.RequiresExtension(); ext != "" {
		notes = append(notes, "this index needs the "+ext+
			" extension: CREATE EXTENSION "+ext+";")
	}

	rec := report.Recommendation{
		Rank:           rank,
		Schema:         c.Schema,
		Table:          c.Table,
		Columns:        cols,
		Predicate:      c.Predicate,
		Include:        c.Include,
		IndexName:      c.IndexName(),
		CreateSQL:      c.DDL(),
		Score:          s.Net,
		Benefit:        s.Benefit,
		WritePenalty:   s.WritePenalty,
		EstimatedBytes: s.EstimatedBytes,
		Confidence:     report.Confidence(s.Confidence),
		Rules:          ruleList,
		Queries:        queries,
		Alternatives:   c.Alternatives,
		Method:         string(c.MethodName()),
		Notes:          notes,
	}
	if c.Note != "" {
		rec.Notes = append([]string{c.Note}, rec.Notes...)
	}
	return rec
}

// tableAgg holds counters summed across nodes plus estimates from the primary.
type tableAgg struct {
	rows       int64
	pages      int64
	seqScan    int64
	seqTupRead int64
	idxScan    int64
	writes     int64
}

func (t tableAgg) writesPerSec(window float64) float64 {
	if window <= 0 {
		return 0
	}
	return float64(t.writes) / window
}

// mergedStats is the multi-node view the analysis works from.
type mergedStats struct {
	tables     map[string]tableAgg
	columns    map[string]collector.Column
	indexes    []collector.Index
	statements []collector.Statement
	// windowSeconds is the statistics window, taken as the longest across
	// nodes so rates are not overstated.
	windowSeconds float64
	sampled       bool
}

// mergeSnapshots sums counters across nodes and takes planner estimates from
// the first node that has them.
//
// Summing matters: the same table scanned on a primary and two replicas is
// under three times the pressure, and an index used only on one replica is
// not unused. Estimates such as row counts and column statistics are not
// summed, because they describe the same data on every node.
func mergeSnapshots(snaps []*collector.Snapshot) mergedStats {
	m := mergedStats{
		tables:  map[string]tableAgg{},
		columns: map[string]collector.Column{},
	}

	for _, s := range snaps {
		for _, t := range s.Tables {
			key := t.QualifiedName()
			agg := m.tables[key]
			// Counters accumulate across nodes.
			agg.seqScan += t.SeqScan
			agg.seqTupRead += t.SeqTupRead
			agg.idxScan += t.IdxScan
			agg.writes += t.Writes()
			// Estimates describe the same rows everywhere; keep the largest
			// so an unanalyzed replica cannot zero them out.
			if r := t.Rows(); r > agg.rows {
				agg.rows = r
			}
			if t.RelPages > agg.pages {
				agg.pages = t.RelPages
			}
			m.tables[key] = agg
		}

		for _, c := range s.Columns {
			key := c.Schema + "." + c.Table + "." + c.Name
			if prev, ok := m.columns[key]; ok && prev.HasStats {
				continue
			}
			m.columns[key] = c
		}

		if window, ok := s.StatsWindow(); ok {
			if secs := window.Seconds(); secs > m.windowSeconds {
				m.windowSeconds = secs
			}
		}
		if s.StatementsSource != "" && s.StatementsSource != "pg_stat_statements" {
			m.sampled = true
		}
	}

	// Index metadata comes from the primary, with usage counters summed.
	scans := map[string]int64{}
	for _, s := range snaps {
		for _, ix := range s.Indexes {
			scans[ix.QualifiedName()] += ix.IdxScan
		}
	}
	seen := map[string]bool{}
	for _, s := range snaps {
		for _, ix := range s.Indexes {
			if seen[ix.QualifiedName()] {
				continue
			}
			seen[ix.QualifiedName()] = true
			ix.IdxScan = scans[ix.QualifiedName()]
			m.indexes = append(m.indexes, ix)
		}
	}

	m.statements = mergeStatements(snaps)
	if m.windowSeconds <= 0 {
		m.windowSeconds = 3600 // a conservative default when unknown
	}
	return m
}

// mergeStatements combines pg_stat_statements rows across nodes, joining on
// the query identifier where it agrees and falling back to the statement text
// when it does not.
func mergeStatements(snaps []*collector.Snapshot) []collector.Statement {
	byKey := map[string]*collector.Statement{}
	var order []string

	for _, s := range snaps {
		for _, st := range s.Statements {
			key := statementKey(st)
			prev, ok := byKey[key]
			if !ok {
				clone := st
				byKey[key] = &clone
				order = append(order, key)
				continue
			}
			prev.Calls += st.Calls
			prev.TotalExecTime += st.TotalExecTime
			prev.Rows += st.Rows
			if prev.Calls > 0 {
				prev.MeanExecTime = prev.TotalExecTime / float64(prev.Calls)
			}
		}
	}

	out := make([]collector.Statement, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TotalExecTime > out[j].TotalExecTime })
	return out
}

// statementKey identifies a statement across nodes. The query identifier is
// derived from object identifiers, which match on physical replicas but not
// on logically replicated ones, so the normalised text is the fallback.
func statementKey(st collector.Statement) string {
	if st.QueryID != 0 {
		if fp, err := parser.Fingerprint(st.Query); err == nil {
			return fp
		}
	}
	return strings.Join(strings.Fields(st.Query), " ")
}

// eligibleTables selects the tables worth analysing: large enough to matter,
// and, on very wide schemas, the ones under the most scan pressure.
func eligibleTables(m mergedStats, opts MissingOptions) map[string]bool {
	type entry struct {
		name     string
		pressure int64
	}
	var list []entry
	for name, t := range m.tables {
		if t.rows < opts.MinRows {
			continue
		}
		list = append(list, entry{name: name, pressure: t.seqTupRead})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].pressure != list[j].pressure {
			return list[i].pressure > list[j].pressure
		}
		return list[i].name < list[j].name
	})
	if len(list) > opts.MaxTables {
		list = list[:opts.MaxTables]
	}

	out := make(map[string]bool, len(list))
	for _, e := range list {
		out[e.name] = true
	}
	return out
}

// existingIndexes renders collected indexes in the form the merge step needs.
func (m mergedStats) existingIndexes() []candidates.ExistingIndex {
	out := make([]candidates.ExistingIndex, 0, len(m.indexes))
	for _, ix := range m.indexes {
		if ix.AccessMethod != "btree" {
			// Only a btree can serve the candidates idxa generates.
			continue
		}
		keys := ix.KeyColumns()
		desc := make([]bool, len(keys))
		for i := range keys {
			if i < len(ix.Options) {
				// pg_index.indoption bit 0 marks a descending column.
				desc[i] = ix.Options[i]&1 == 1
			}
		}
		out = append(out, candidates.ExistingIndex{
			Schema:     ix.Schema,
			Table:      ix.Table,
			Name:       ix.Name,
			Columns:    stripDirections(keys),
			Descending: desc,
			Predicate:  ix.Predicate,
			Unique:     ix.Unique || ix.Primary,
			Valid:      ix.Valid,
			Scans:      ix.IdxScan,
		})
	}
	return out
}

// stripDirections removes any direction suffix pg_get_indexdef rendered into
// a column expression, leaving the bare column name.
func stripDirections(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		c = strings.TrimSpace(c)
		for _, suffix := range []string{" DESC", " ASC", " NULLS FIRST", " NULLS LAST"} {
			c = strings.TrimSuffix(strings.TrimSpace(c), suffix)
		}
		out[i] = strings.Trim(strings.TrimSpace(c), `"`)
	}
	return out
}

// columnStatsFor renders the column statistics for one table in the form the
// scorer expects.
func (m mergedStats) columnStatsFor(table string) map[string]scorer.ColumnStats {
	out := map[string]scorer.ColumnStats{}
	prefix := table + "."
	rows := m.tables[table].rows
	for key, c := range m.columns {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		name := key[len(prefix):]
		if strings.Contains(name, ".") {
			continue
		}
		out[name] = scorer.ColumnStats{
			NDistinct:      c.NDistinct,
			NullFrac:       c.NullFrac,
			Correlation:    c.Correlation,
			AvgWidth:       float64(c.AvgWidth),
			MostCommonFreq: c.TopMCVFreq,
			Known:          c.HasStats,
		}
		_ = rows
	}
	return out
}

// formatSkipped explains workload coverage in words.
func formatSkipped(parsed, skipped int) string {
	total := parsed + skipped
	if total == 0 {
		return ""
	}
	pct := 100 * float64(parsed) / float64(total)
	return sprintf("analysed %d of %d workload statements (%.0f%%); the rest were writes, DDL, or statements no index can help",
		parsed, total, pct)
}

// sprintf is a local alias so the file needs only one formatting import.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// schemaResolver maps a bare table name to the schema it lives in, using the
// tables that were actually collected. Statements rarely name a schema, so
// this is what connects a query to its statistics.
//
// When the same name exists in several schemas the query is ambiguous
// without search_path, which the advisor does not know. public is preferred
// because it is on every default search_path; failing that the lowest
// schema name wins, so the choice is at least stable between runs.
func schemaResolver(m mergedStats) func(name string) string {
	bySchema := map[string][]string{}
	for key := range m.tables {
		dot := strings.LastIndex(key, ".")
		if dot <= 0 {
			continue
		}
		schema, name := key[:dot], key[dot+1:]
		bySchema[name] = append(bySchema[name], schema)
	}
	return func(name string) string {
		schemas := bySchema[name]
		switch len(schemas) {
		case 0:
			return ""
		case 1:
			return schemas[0]
		}
		sort.Strings(schemas)
		for _, s := range schemas {
			if s == "public" {
				return s
			}
		}
		return schemas[0]
	}
}
