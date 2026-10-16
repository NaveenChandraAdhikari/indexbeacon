// Package report defines the output model shared by every idxa command and
// renders it as a table, JSON, Markdown or SQL.
package report

import (
	"time"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

// Kind identifies which command produced the report.
type Kind string

const (
	KindAnalyze    Kind = "analyze"
	KindUnused     Kind = "unused"
	KindDuplicates Kind = "duplicates"
)

// Confidence reflects how much the underlying statistics can be trusted.
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// NodeSummary is the per-node header of a report.
type NodeSummary struct {
	Name         string        `json:"name"`
	DSN          string        `json:"dsn"`
	Role         db.Role       `json:"role"`
	Version      string        `json:"version"`
	StatsWindow  time.Duration `json:"stats_window_ns"`
	StatsKnown   bool          `json:"stats_window_known"`
	HasPSS       bool          `json:"pg_stat_statements"`
	HasHypoPG    bool          `json:"hypopg"`
	StatementCnt int           `json:"statements"`
}

// PerNodeCount is a counter broken down by node.
type PerNodeCount struct {
	Node  string `json:"node"`
	Count int64  `json:"count"`
}

// UnusedIndex is one index with zero scans across every node.
type UnusedIndex struct {
	Schema     string         `json:"schema"`
	Table      string         `json:"table"`
	Index      string         `json:"index"`
	Definition string         `json:"definition"`
	SizeBytes  int64          `json:"size_bytes"`
	Scans      []PerNodeCount `json:"scans_per_node"`
	TableWrite int64          `json:"table_writes"`
	Reason     string         `json:"reason"`
	Confidence Confidence     `json:"confidence"`
	DropSQL    string         `json:"drop_sql"`
}

// DuplicateGroup describes an index made redundant by another.
type DuplicateGroup struct {
	Schema        string `json:"schema"`
	Table         string `json:"table"`
	Redundant     string `json:"redundant_index"`
	RedundantDef  string `json:"redundant_definition"`
	RedundantSize int64  `json:"redundant_size_bytes"`
	CoveredBy     string `json:"covered_by"`
	CoveredByDef  string `json:"covered_by_definition"`
	Kind          string `json:"kind"` // exact | prefix
	Reason        string `json:"reason"`
	DropSQL       string `json:"drop_sql,omitempty"`
	Scans         int64  `json:"redundant_scans"`
}

// HotTable is a table with high sequential-scan pressure (table-only mode).
type HotTable struct {
	Schema     string  `json:"schema"`
	Table      string  `json:"table"`
	Rows       int64   `json:"rows"`
	SeqScans   int64   `json:"seq_scans"`
	SeqTupRead int64   `json:"seq_tup_read"`
	IdxScans   int64   `json:"idx_scans"`
	SeqRatio   float64 `json:"seq_scan_ratio"`
	TableBytes int64   `json:"table_bytes"`
	IndexCount int     `json:"index_count"`
}

// CoveredQuery is one workload statement that a recommendation helps.
type CoveredQuery struct {
	QueryID     int64   `json:"queryid,omitempty"`
	Fingerprint string  `json:"fingerprint,omitempty"`
	Calls       int64   `json:"calls"`
	TotalTimeMS float64 `json:"total_time_ms"`
	Query       string  `json:"query"`
}

// Recommendation is a missing-index suggestion.
type Recommendation struct {
	Rank            int            `json:"rank"`
	Schema          string         `json:"schema"`
	Table           string         `json:"table"`
	Columns         []string       `json:"columns"` // with direction suffix when not ASC
	Predicate       string         `json:"predicate,omitempty"`
	Include         []string       `json:"include,omitempty"`
	IndexName       string         `json:"index_name"`
	Method          string         `json:"method,omitempty"`
	CreateSQL       string         `json:"create_sql"`
	Score           float64        `json:"score"`
	Benefit         float64        `json:"benefit"`
	WritePenalty    float64        `json:"write_penalty"`
	EstimatedBytes  int64          `json:"estimated_bytes"`
	Confidence      Confidence     `json:"confidence"`
	Rules           []string       `json:"rules"`
	Queries         []CoveredQuery `json:"queries"`
	Alternatives    []string       `json:"alternatives,omitempty"`
	ExistingSimilar string         `json:"existing_similar,omitempty"`
	Validated       bool           `json:"validated"`
	PlanUsesIndex   *bool          `json:"plan_uses_index,omitempty"`
	ValidatedGain   float64        `json:"validated_gain,omitempty"`
	Notes           []string       `json:"notes,omitempty"`
}

// Report is the top-level output document.
type Report struct {
	Kind            Kind             `json:"kind"`
	Version         string           `json:"idxa_version"`
	GeneratedAt     time.Time        `json:"generated_at"`
	Nodes           []NodeSummary    `json:"nodes"`
	Warnings        []string         `json:"warnings,omitempty"`
	Mode            string           `json:"mode,omitempty"` // analyze: "table-only" | "heuristic" | "validated"
	Recommendations []Recommendation `json:"recommendations,omitempty"`
	HotTables       []HotTable       `json:"hot_tables,omitempty"`
	Unused          []UnusedIndex    `json:"unused,omitempty"`
	Duplicates      []DuplicateGroup `json:"duplicates,omitempty"`
}

// NodeSummaryFrom builds a NodeSummary from node info.
func NodeSummaryFrom(info db.Info, statements int) NodeSummary {
	age, known := info.StatsAge()
	return NodeSummary{
		Name:         info.Name,
		DSN:          info.DSN,
		Role:         info.Role,
		Version:      info.ServerVersion,
		StatsWindow:  age,
		StatsKnown:   known,
		HasPSS:       info.HasExtension("pg_stat_statements") && info.PSSPreloaded(),
		HasHypoPG:    info.HasExtension("hypopg"),
		StatementCnt: statements,
	}
}
