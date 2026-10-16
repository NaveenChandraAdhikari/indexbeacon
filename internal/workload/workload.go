// Package workload records which queries a database actually runs, for the
// many servers where pg_stat_statements is unavailable.
//
// Not every database has that extension. It needs a line in postgresql.conf
// and a restart, which on a managed service can mean a maintenance window and
// on someone else's server can mean a conversation that never happens. Without
// a workload idxa can still find unused and duplicate indexes, but it cannot
// recommend new ones, which is the part people want.
//
// Sampling pg_stat_activity fills that gap. It is a cheap read of what is
// running right now, so taking it often enough builds a picture of the
// workload: a query that runs constantly is caught in most samples, and one
// that runs rarely is caught in few. The counts are not exact call counts and
// this package never pretends otherwise, but the ordering they produce is
// what index recommendations actually depend on.
package workload

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Source records how a workload was gathered, so a report can be honest about
// how much to trust it.
type Source string

const (
	// SourceSampled came from watching pg_stat_activity.
	SourceSampled Source = "sampled"
	// SourceStatements came from pg_stat_statements.
	SourceStatements Source = "pg_stat_statements"
)

// Query is one statement shape and how often it was seen.
type Query struct {
	// Fingerprint identifies the statement shape, ignoring literals.
	Fingerprint string `json:"fingerprint"`
	// SQL is the normalised statement text.
	SQL string `json:"sql"`
	// Samples is how many times the statement was caught running.
	Samples int64 `json:"samples"`
	// EstimatedCalls extrapolates a call count from the sample rate. It is
	// an estimate and is documented as one wherever it is displayed.
	EstimatedCalls int64 `json:"estimated_calls"`
	// TotalTimeMS estimates time spent, derived from how long the statement
	// was observed running.
	TotalTimeMS float64 `json:"total_time_ms"`
	// Nodes counts samples per node, which matters when a replica serves a
	// different mix of queries from the primary.
	Nodes map[string]int64 `json:"nodes,omitempty"`
	// FirstSeen and LastSeen bound when the statement was observed.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Literals holds one example statement with its values intact, kept only
	// when the user asked for it. Partial index candidates need a constant,
	// but statement text can contain personal data, so this is opt in.
	Literals string `json:"literals,omitempty"`
}

// Workload is a complete recording.
type Workload struct {
	// Version allows the format to change without breaking old files.
	Version int `json:"version"`
	// Source says how this was gathered.
	Source Source `json:"source"`
	// StartedAt and EndedAt bound the recording.
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	// IntervalMS is how often a sample was taken.
	IntervalMS int64 `json:"interval_ms"`
	// SampleCount is how many sampling rounds completed, which is what turns
	// observation counts into rates.
	SampleCount int64 `json:"sample_count"`
	// Nodes lists the servers sampled.
	Nodes []string `json:"nodes"`
	// Queries are the statements observed, most frequent first.
	Queries []*Query `json:"queries"`
	// Truncated counts statements that could not be recorded because the
	// server had truncated their text, so coverage can be reported honestly.
	Truncated int64 `json:"truncated,omitempty"`
}

// Duration is how long the recording ran.
func (w *Workload) Duration() time.Duration {
	if w.EndedAt.IsZero() {
		return 0
	}
	return w.EndedAt.Sub(w.StartedAt)
}

// Recorder accumulates samples into a workload.
type Recorder struct {
	interval time.Duration
	keep     bool

	started   time.Time
	samples   int64
	nodes     map[string]bool
	queries   map[string]*Query
	truncated int64
}

// NewRecorder returns a Recorder. keepLiterals stores one example statement
// with its values, which partial index candidates need and which may contain
// personal data.
func NewRecorder(interval time.Duration, keepLiterals bool) *Recorder {
	return &Recorder{
		interval: interval,
		keep:     keepLiterals,
		started:  time.Now().UTC(),
		nodes:    map[string]bool{},
		queries:  map[string]*Query{},
	}
}

// Observation is one running statement seen in one sample.
type Observation struct {
	Node string
	// Fingerprint and Normalized come from parsing the statement.
	Fingerprint string
	Normalized  string
	// Raw is the statement as the server reported it, with literals.
	Raw string
	// RunningFor is how long the statement had been executing, used to
	// estimate time spent.
	RunningFor time.Duration
}

// Tick records one completed sampling round.
func (r *Recorder) Tick(node string, observations []Observation) {
	r.samples++
	r.nodes[node] = true
	now := time.Now().UTC()

	for _, obs := range observations {
		if obs.Fingerprint == "" {
			r.truncated++
			continue
		}
		q, ok := r.queries[obs.Fingerprint]
		if !ok {
			q = &Query{
				Fingerprint: obs.Fingerprint,
				SQL:         obs.Normalized,
				Nodes:       map[string]int64{},
				FirstSeen:   now,
			}
			r.queries[obs.Fingerprint] = q
		}
		q.Samples++
		q.LastSeen = now
		q.Nodes[obs.Node]++

		// A statement caught mid-execution has been running for some time;
		// the sample interval bounds how much of it is newly observed.
		elapsed := obs.RunningFor
		if elapsed <= 0 || elapsed > r.interval {
			elapsed = r.interval
		}
		q.TotalTimeMS += float64(elapsed.Milliseconds())

		if r.keep && q.Literals == "" && obs.Raw != "" {
			q.Literals = obs.Raw
		}
	}
}

// NoteTruncated records a statement the server could not report in full.
func (r *Recorder) NoteTruncated() { r.truncated++ }

// Workload finalises the recording.
//
// Sample counts become estimated call counts by assuming a statement seen in
// n of N samples was running for roughly n/N of the recording. Divided by its
// average duration, that gives a call count. It is a rough figure, and the
// scorer treats a sampled workload as lower confidence for exactly this
// reason.
func (r *Recorder) Workload() *Workload {
	w := &Workload{
		Version:     1,
		Source:      SourceSampled,
		StartedAt:   r.started,
		EndedAt:     time.Now().UTC(),
		IntervalMS:  r.interval.Milliseconds(),
		SampleCount: r.samples,
		Truncated:   r.truncated,
	}
	for node := range r.nodes {
		w.Nodes = append(w.Nodes, node)
	}
	sort.Strings(w.Nodes)

	for _, q := range r.queries {
		if q.Samples > 0 && q.TotalTimeMS > 0 {
			avgMS := q.TotalTimeMS / float64(q.Samples)
			if avgMS > 0 {
				// Time the statement was running, divided by how long one
				// execution takes.
				observedMS := float64(q.Samples) * float64(r.interval.Milliseconds())
				q.EstimatedCalls = int64(observedMS / avgMS)
			}
		}
		if q.EstimatedCalls < q.Samples {
			// A statement cannot have run fewer times than it was seen.
			q.EstimatedCalls = q.Samples
		}
		w.Queries = append(w.Queries, q)
	}

	sort.Slice(w.Queries, func(i, j int) bool {
		if w.Queries[i].Samples != w.Queries[j].Samples {
			return w.Queries[i].Samples > w.Queries[j].Samples
		}
		return w.Queries[i].Fingerprint < w.Queries[j].Fingerprint
	})
	return w
}

// Save writes a workload as gzipped JSON.
func Save(path string, w *Workload) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create workload file: %w", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	enc.SetIndent("", "  ")
	if err := enc.Encode(w); err != nil {
		gz.Close()
		return fmt.Errorf("write workload: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finish workload file: %w", err)
	}
	return nil
}

// Load reads a workload file, accepting both gzipped and plain JSON so a file
// someone has decompressed by hand still works.
func Load(path string) (*Workload, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open workload file: %w", err)
	}
	defer f.Close()

	buffered := bufio.NewReader(f)
	var reader io.Reader = buffered

	// gzip files begin with 0x1f 0x8b.
	if magic, err := buffered.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, fmt.Errorf("read workload file: %w", err)
		}
		defer gz.Close()
		reader = gz
	}

	var w Workload
	if err := json.NewDecoder(reader).Decode(&w); err != nil {
		return nil, fmt.Errorf("parse workload file %s: %w", path, err)
	}
	if w.Version == 0 {
		return nil, fmt.Errorf("%s does not look like an idxa workload file", path)
	}
	return &w, nil
}

// Summary describes a workload in one line for the command line.
func (w *Workload) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d statement shapes from %d samples over %s",
		len(w.Queries), w.SampleCount, w.Duration().Round(time.Second))
	if len(w.Nodes) > 1 {
		fmt.Fprintf(&b, " across %d nodes", len(w.Nodes))
	}
	if w.Truncated > 0 {
		fmt.Fprintf(&b, "; %d statements were truncated by the server and could not be used", w.Truncated)
	}
	return b.String()
}
