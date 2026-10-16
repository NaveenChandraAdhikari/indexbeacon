package workload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecorderCountsAndRanks(t *testing.T) {
	r := NewRecorder(time.Second, false)

	// A statement seen in most rounds must outrank one seen once, because
	// that ordering is what index recommendations depend on.
	for i := 0; i < 10; i++ {
		r.Tick("primary", []Observation{
			{Node: "primary", Fingerprint: "hot", Normalized: "SELECT 1", RunningFor: 50 * time.Millisecond},
		})
	}
	r.Tick("primary", []Observation{
		{Node: "primary", Fingerprint: "rare", Normalized: "SELECT 2", RunningFor: 50 * time.Millisecond},
	})

	w := r.Workload()
	if len(w.Queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(w.Queries))
	}
	if w.Queries[0].Fingerprint != "hot" {
		t.Errorf("order = %v, want the frequent statement first", w.Queries[0].Fingerprint)
	}
	if w.Queries[0].Samples != 10 {
		t.Errorf("samples = %d, want 10", w.Queries[0].Samples)
	}
	if w.SampleCount != 11 {
		t.Errorf("sample count = %d, want 11 rounds", w.SampleCount)
	}
	if w.Source != SourceSampled {
		t.Errorf("source = %q, want sampled", w.Source)
	}
}

func TestEstimatedCallsNeverBelowSamples(t *testing.T) {
	// A statement cannot have run fewer times than it was seen running.
	r := NewRecorder(time.Second, false)
	for i := 0; i < 5; i++ {
		r.Tick("primary", []Observation{
			{Node: "primary", Fingerprint: "slow", Normalized: "SELECT 1",
				RunningFor: 10 * time.Second}, // longer than the interval
		})
	}
	w := r.Workload()
	q := w.Queries[0]
	if q.EstimatedCalls < q.Samples {
		t.Errorf("estimated calls %d is below samples %d", q.EstimatedCalls, q.Samples)
	}
}

func TestFastQueriesEstimateMoreCallsThanSlowOnes(t *testing.T) {
	// Two statements seen equally often, one finishing quickly, mean the
	// fast one must have run far more times to be caught as often.
	r := NewRecorder(time.Second, false)
	for i := 0; i < 10; i++ {
		r.Tick("primary", []Observation{
			{Node: "primary", Fingerprint: "fast", Normalized: "SELECT 1", RunningFor: 5 * time.Millisecond},
			{Node: "primary", Fingerprint: "slow", Normalized: "SELECT 2", RunningFor: 900 * time.Millisecond},
		})
	}

	w := r.Workload()
	byPrint := map[string]*Query{}
	for _, q := range w.Queries {
		byPrint[q.Fingerprint] = q
	}
	if byPrint["fast"].EstimatedCalls <= byPrint["slow"].EstimatedCalls {
		t.Errorf("fast=%d slow=%d; a quick query seen as often must have run more times",
			byPrint["fast"].EstimatedCalls, byPrint["slow"].EstimatedCalls)
	}
}

func TestTruncatedStatementsAreCountedNotGuessed(t *testing.T) {
	// A statement the server cut short cannot be parsed, and guessing at the
	// missing tail would produce an index for a query that does not exist.
	r := NewRecorder(time.Second, false)
	r.Tick("primary", []Observation{
		{Node: "primary"}, // no fingerprint: truncated or unparsable
		{Node: "primary", Fingerprint: "ok", Normalized: "SELECT 1"},
	})

	w := r.Workload()
	if w.Truncated != 1 {
		t.Errorf("truncated = %d, want 1", w.Truncated)
	}
	if len(w.Queries) != 1 {
		t.Errorf("queries = %d, want only the parsable one", len(w.Queries))
	}
	if !strings.Contains(w.Summary(), "truncated") {
		t.Errorf("the summary should disclose truncation: %s", w.Summary())
	}
}

func TestLiteralsAreOptIn(t *testing.T) {
	// Statement text can carry personal data, so keeping an example with its
	// values must be a deliberate choice.
	off := NewRecorder(time.Second, false)
	off.Tick("primary", []Observation{{
		Node: "primary", Fingerprint: "f", Normalized: "SELECT $1",
		Raw: "SELECT 'alice@example.com'",
	}})
	if got := off.Workload().Queries[0].Literals; got != "" {
		t.Errorf("literals kept without asking: %q", got)
	}

	on := NewRecorder(time.Second, true)
	on.Tick("primary", []Observation{{
		Node: "primary", Fingerprint: "f", Normalized: "SELECT $1",
		Raw: "SELECT 'alice@example.com'",
	}})
	if got := on.Workload().Queries[0].Literals; got == "" {
		t.Error("literals were requested but not kept")
	}
}

func TestPerNodeCountsAreKept(t *testing.T) {
	// A replica often serves a different mix from the primary, and knowing
	// which node ran what is the point of sampling several.
	r := NewRecorder(time.Second, false)
	r.Tick("primary", []Observation{{Node: "primary", Fingerprint: "f", Normalized: "SELECT 1"}})
	r.Tick("replica", []Observation{{Node: "replica", Fingerprint: "f", Normalized: "SELECT 1"}})
	r.Tick("replica", []Observation{{Node: "replica", Fingerprint: "f", Normalized: "SELECT 1"}})

	w := r.Workload()
	nodes := w.Queries[0].Nodes
	if nodes["primary"] != 1 || nodes["replica"] != 2 {
		t.Errorf("per-node counts = %v", nodes)
	}
	if len(w.Nodes) != 2 {
		t.Errorf("nodes = %v, want both", w.Nodes)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workload.json.gz")

	r := NewRecorder(500*time.Millisecond, false)
	r.Tick("primary", []Observation{{Node: "primary", Fingerprint: "f",
		Normalized: "SELECT * FROM orders WHERE customer_id = $1"}})
	original := r.Workload()

	if err := Save(path, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The file may contain query text, so it must not be world readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600: recordings can contain query text", perm)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Queries) != 1 || loaded.Queries[0].SQL != original.Queries[0].SQL {
		t.Errorf("round trip lost data: %+v", loaded.Queries)
	}
	if loaded.IntervalMS != 500 {
		t.Errorf("interval = %d, want 500", loaded.IntervalMS)
	}
}

func TestLoadAcceptsPlainJSON(t *testing.T) {
	// Someone will decompress a recording to look at it; that file should
	// still work.
	dir := t.TempDir()
	gzPath := filepath.Join(dir, "w.json.gz")

	r := NewRecorder(time.Second, false)
	r.Tick("primary", []Observation{{Node: "primary", Fingerprint: "f", Normalized: "SELECT 1"}})
	if err := Save(gzPath, r.Workload()); err != nil {
		t.Fatal(err)
	}

	plain := filepath.Join(dir, "w.json")
	if err := os.WriteFile(plain, []byte(`{"version":1,"source":"sampled","queries":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(plain); err != nil {
		t.Errorf("plain JSON should load: %v", err)
	}
}

func TestLoadRejectsSomethingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-workload.json")
	if err := os.WriteFile(path, []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a file that is not a recording")
	}
	if !strings.Contains(err.Error(), "does not look like") {
		t.Errorf("error should say plainly what is wrong: %v", err)
	}

	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestTruncationDetection(t *testing.T) {
	// pg_stat_activity stores only the first track_activity_query_size bytes,
	// so a long statement arrives with its tail missing.
	limit := 64
	long := strings.Repeat("x", limit)
	if !isTruncated(long, limit) {
		t.Error("a statement at the limit should be treated as truncated")
	}
	if isTruncated("SELECT 1", limit) {
		t.Error("a short statement was wrongly treated as truncated")
	}
	if isTruncated("SELECT 1", 0) {
		t.Error("an unknown limit must not mark everything as truncated")
	}
}

func TestDescribeExplainsTheLimitation(t *testing.T) {
	w := &Workload{Source: SourceSampled, IntervalMS: 250}
	got := w.Describe()
	for _, want := range []string{"sampled", "estimates", "confidence"} {
		if !strings.Contains(got, want) {
			t.Errorf("description missing %q: %s", want, got)
		}
	}

	if got := (&Workload{Source: SourceStatements}).Describe(); got != "" {
		t.Errorf("only a sampled workload needs the caveat, got %q", got)
	}
}

func TestSummaryReadsAsASentence(t *testing.T) {
	r := NewRecorder(time.Second, false)
	r.Tick("primary", []Observation{{Node: "primary", Fingerprint: "f", Normalized: "SELECT 1"}})
	w := r.Workload()

	got := w.Summary()
	if !strings.Contains(got, "statement shapes") || !strings.Contains(got, "samples") {
		t.Errorf("summary = %q", got)
	}
}

func TestSampleOptionsClampInterval(t *testing.T) {
	// Below a certain frequency the sampler costs the server more than the
	// extra resolution is worth.
	got := SampleOptions{Interval: time.Millisecond}.withDefaults()
	if got.Interval < 100*time.Millisecond {
		t.Errorf("interval = %v, want a floor", got.Interval)
	}
	if got := (SampleOptions{}).withDefaults().Interval; got != time.Second {
		t.Errorf("default interval = %v, want 1s", got)
	}
}
