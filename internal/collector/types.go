// Package collector reads catalog and statistics views from a node into
// plain Go structures. It never issues anything but SELECT statements.
package collector

import (
	"time"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// Table is one row of pg_class joined with pg_stat_user_tables.
type Table struct {
	Schema      string `json:"schema"`
	Name        string `json:"name"`
	OID         uint32 `json:"oid"`
	Kind        string `json:"kind"` // r | p | m
	RelPages    int64  `json:"relpages"`
	RelTuples   int64  `json:"reltuples"`
	LiveTuples  int64  `json:"n_live_tup"`
	DeadTuples  int64  `json:"n_dead_tup"`
	SeqScan     int64  `json:"seq_scan"`
	SeqTupRead  int64  `json:"seq_tup_read"`
	IdxScan     int64  `json:"idx_scan"`
	IdxTupFetch int64  `json:"idx_tup_fetch"`
	Inserts     int64  `json:"n_tup_ins"`
	Updates     int64  `json:"n_tup_upd"`
	Deletes     int64  `json:"n_tup_del"`
	HotUpdates  int64  `json:"n_tup_hot_upd"`
	TableBytes  int64  `json:"table_bytes"`
	IndexBytes  int64  `json:"index_bytes"`
}

// QualifiedName returns schema.name.
func (t Table) QualifiedName() string { return t.Schema + "." + t.Name }

// Rows is the best available row-count estimate.
func (t Table) Rows() int64 {
	if t.LiveTuples > 0 {
		return t.LiveTuples
	}
	if t.RelTuples > 0 {
		return t.RelTuples
	}
	return 0
}

// Writes is the total number of row modifications since stats reset.
func (t Table) Writes() int64 { return t.Inserts + t.Updates + t.Deletes }

// Index is one row of pg_index joined with pg_stat_user_indexes.
type Index struct {
	Schema           string   `json:"schema"`
	Table            string   `json:"table"`
	TableOID         uint32   `json:"table_oid"`
	Name             string   `json:"name"`
	OID              uint32   `json:"oid"`
	AccessMethod     string   `json:"am"`
	Unique           bool     `json:"unique"`
	Primary          bool     `json:"primary"`
	Valid            bool     `json:"valid"`
	NKeyColumns      int      `json:"nkey_columns"`
	Definition       string   `json:"definition"`
	Predicate        string   `json:"predicate,omitempty"`
	Columns          []string `json:"columns"` // key columns then INCLUDE columns, as rendered by pg_get_indexdef
	Options          []int16  `json:"options"` // pg_index.indoption per column (bit0 DESC, bit1 NULLS FIRST)
	Opclasses        []uint32 `json:"opclasses"`
	Collations       []uint32 `json:"collations"`
	ConstraintBacked bool     `json:"constraint_backed"`
	IdxScan          int64    `json:"idx_scan"`
	IdxTupRead       int64    `json:"idx_tup_read"`
	IdxTupFetch      int64    `json:"idx_tup_fetch"`
	SizeBytes        int64    `json:"size_bytes"`
}

// QualifiedTable returns schema.table.
func (i Index) QualifiedTable() string { return i.Schema + "." + i.Table }

// QualifiedName returns schema.index.
func (i Index) QualifiedName() string { return i.Schema + "." + i.Name }

// KeyColumns returns only the key part of the index (no INCLUDE columns).
func (i Index) KeyColumns() []string {
	if i.NKeyColumns <= 0 || i.NKeyColumns > len(i.Columns) {
		return i.Columns
	}
	return i.Columns[:i.NKeyColumns]
}

// IncludeColumns returns the INCLUDE part of the index.
func (i Index) IncludeColumns() []string {
	if i.NKeyColumns <= 0 || i.NKeyColumns >= len(i.Columns) {
		return nil
	}
	return i.Columns[i.NKeyColumns:]
}

// Column is one attribute with its pg_stats row when available.
type Column struct {
	Schema      string    `json:"schema"`
	Table       string    `json:"table"`
	Num         int16     `json:"attnum"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	TypeOID     uint32    `json:"type_oid"`
	NotNull     bool      `json:"not_null"`
	HasStats    bool      `json:"has_stats"`
	NullFrac    float64   `json:"null_frac"`
	NDistinct   float64   `json:"n_distinct"` // pg_stats semantics: <0 means fraction of rows
	Correlation float64   `json:"correlation"`
	AvgWidth    int       `json:"avg_width"`
	TopMCVFreq  float64   `json:"top_mcv_freq"`
	MCVFreqs    []float64 `json:"-"`
}

// DistinctValues resolves pg_stats.n_distinct against the table row count.
func (c Column) DistinctValues(rows int64) float64 {
	switch {
	case !c.HasStats || c.NDistinct == 0:
		if rows > 0 {
			return float64(rows) // unknown: assume unique
		}
		return 1
	case c.NDistinct < 0:
		return max(1, -c.NDistinct*float64(rows))
	default:
		return max(1, c.NDistinct)
	}
}

// Statement is one row of pg_stat_statements.
type Statement struct {
	QueryID        int64   `json:"queryid"`
	Query          string  `json:"query"`
	Calls          int64   `json:"calls"`
	TotalExecTime  float64 `json:"total_exec_time_ms"`
	MeanExecTime   float64 `json:"mean_exec_time_ms"`
	Rows           int64   `json:"rows"`
	SharedBlksHit  int64   `json:"shared_blks_hit"`
	SharedBlksRead int64   `json:"shared_blks_read"`
}

// Snapshot is everything collected from one node.
type Snapshot struct {
	Node        db.Info     `json:"node"`
	CollectedAt time.Time   `json:"collected_at"`
	Tables      []Table     `json:"tables"`
	Indexes     []Index     `json:"indexes"`
	Columns     []Column    `json:"columns"`
	Statements  []Statement `json:"statements"`
	// StatementsSource is "pg_stat_statements" or "" when unavailable.
	StatementsSource string   `json:"statements_source"`
	Warnings         []string `json:"warnings,omitempty"`
}

// StatsWindow is the age of the cumulative counters, if known.
func (s *Snapshot) StatsWindow() (time.Duration, bool) { return s.Node.StatsAge() }

// TableByName finds a table by schema.name.
func (s *Snapshot) TableByName(schema, name string) *Table {
	for i := range s.Tables {
		if s.Tables[i].Schema == schema && s.Tables[i].Name == name {
			return &s.Tables[i]
		}
	}
	return nil
}

// ColumnsOf returns the columns of a table in attnum order.
func (s *Snapshot) ColumnsOf(schema, table string) []Column {
	var out []Column
	for _, c := range s.Columns {
		if c.Schema == schema && c.Table == table {
			out = append(out, c)
		}
	}
	return out
}

// IndexesOf returns the indexes of a table.
func (s *Snapshot) IndexesOf(schema, table string) []Index {
	var out []Index
	for _, ix := range s.Indexes {
		if ix.Schema == schema && ix.Table == table {
			out = append(out, ix)
		}
	}
	return out
}

// Filter restricts which schemas are collected.
type Filter struct {
	Schemas        []string
	ExcludeSchemas []string
}

// Allows reports whether schema passes the filter.
func (f Filter) Allows(schema string) bool {
	for _, s := range f.ExcludeSchemas {
		if s == schema {
			return false
		}
	}
	if len(f.Schemas) == 0 {
		return true
	}
	for _, s := range f.Schemas {
		if s == schema {
			return true
		}
	}
	return false
}
