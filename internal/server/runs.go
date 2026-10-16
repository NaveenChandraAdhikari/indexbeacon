package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/advisor"
	"github.com/indexbeacon/indexbeacon/internal/collector"
	"github.com/indexbeacon/indexbeacon/internal/config"
	"github.com/indexbeacon/indexbeacon/internal/db"
	"github.com/indexbeacon/indexbeacon/internal/report"
	"github.com/indexbeacon/indexbeacon/internal/validator"
	"github.com/indexbeacon/indexbeacon/internal/workload"
)

// Trigger records who asked for a run.
type Trigger string

const (
	TriggerManual    Trigger = "manual"
	TriggerScheduled Trigger = "scheduled"
)

// Phase names appear in the interface as the run progresses, so they are
// written for a reader rather than as internal identifiers.
var phases = []string{
	"connecting",
	"reading statistics",
	"reading the workload",
	"parsing queries",
	"proposing indexes",
	"scoring",
	"confirming with the planner",
}

// phaseSampling is used instead of "reading the workload" when the run is
// watching live queries rather than reading pg_stat_statements.
const phaseSampling = "watching queries"

// maxSampleDuration bounds a recording started from the interface. Sampling
// is cheap per tick but runs continuously, and an unbounded recording started
// by a stray click would keep querying somebody's server for hours.
const maxSampleDuration = 15 * time.Minute

// RunStatus is the state of an analysis.
type RunStatus string

const (
	RunRunning  RunStatus = "running"
	RunComplete RunStatus = "complete"
	RunFailed   RunStatus = "failed"
	RunCanceled RunStatus = "canceled"
)

// Run is one analysis, live or saved.
type Run struct {
	ID        string    `json:"id"`
	ClusterID string    `json:"cluster_id"`
	Trigger   Trigger   `json:"trigger"`
	Status    RunStatus `json:"status"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	Error     string    `json:"error,omitempty"`
	// Report is the finished analysis. Missing, unused and duplicate index
	// findings all come from it, because they all derive from the same
	// statistics snapshot: running them separately would make the server pay
	// for the same collection three times.
	Report *report.Report `json:"report,omitempty"`
	// Changes marks what is new since the previous run, which is far more
	// actionable than a list that looks the same every day.
	Changes *RunChanges `json:"changes,omitempty"`
	// SampleFor is how long this run watches live queries before analysing,
	// for databases without pg_stat_statements. Zero reads the workload from
	// the server's own statistics instead.
	SampleFor time.Duration `json:"sample_for_ns,omitempty"`
	// SampleProgress is filled in while a recording is running so the
	// interface can show something during a wait measured in minutes.
	SampleProgress string `json:"sample_progress,omitempty"`
}

// RunChanges summarises the difference from the previous completed run.
type RunChanges struct {
	PreviousID  string   `json:"previous_id,omitempty"`
	NewIndexes  []string `json:"new_recommendations,omitempty"`
	GoneIndexes []string `json:"resolved_recommendations,omitempty"`
}

// Duration is how long the run took.
func (r *Run) Duration() time.Duration {
	if r.EndedAt.IsZero() {
		return time.Since(r.StartedAt)
	}
	return r.EndedAt.Sub(r.StartedAt)
}

// runStore keeps runs on disk so history survives a restart.
type runStore struct {
	dir string

	mu     sync.RWMutex
	live   map[string]*liveRun
	byID   map[string]*Run
	loaded bool
}

// liveRun is an in-progress analysis with its subscribers.
type liveRun struct {
	run    *Run
	cancel context.CancelFunc

	mu      sync.Mutex
	clients map[chan Run]struct{}
	done    bool
}

// maxRunsPerCluster bounds history so the directory cannot grow without end.
const maxRunsPerCluster = 20

func newRunStore(configPath string) (*runStore, error) {
	dir := filepath.Join(filepath.Dir(configPath), "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	return &runStore{
		dir:  dir,
		live: map[string]*liveRun{},
		byID: map[string]*Run{},
	}, nil
}

// clusterDir is where one database's runs live.
func (rs *runStore) clusterDir(clusterID string) string {
	return filepath.Join(rs.dir, sanitizeID(clusterID))
}

// save writes a finished run and prunes old ones.
func (rs *runStore) save(run *Run) error {
	dir := rs.clusterDir(run.ClusterID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, run.ID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	rs.prune(run.ClusterID)
	return nil
}

// prune keeps only the most recent runs.
func (rs *runStore) prune(clusterID string) {
	entries, err := os.ReadDir(rs.clusterDir(clusterID))
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= maxRunsPerCluster {
		return
	}
	sort.Strings(names) // identifiers begin with a timestamp
	for _, name := range names[:len(names)-maxRunsPerCluster] {
		_ = os.Remove(filepath.Join(rs.clusterDir(clusterID), name))
	}
}

// list returns saved runs for a database, newest first, without their
// reports so the listing stays small.
func (rs *runStore) list(clusterID string) []*Run {
	entries, err := os.ReadDir(rs.clusterDir(clusterID))
	if err != nil {
		return nil
	}
	var out []*Run
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		run, err := rs.read(clusterID, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		summary := *run
		summary.Report = nil
		out = append(out, &summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })

	// Include anything still running.
	rs.mu.RLock()
	for _, lr := range rs.live {
		if lr.run.ClusterID == clusterID {
			summary := *lr.run
			summary.Report = nil
			out = append([]*Run{&summary}, out...)
		}
	}
	rs.mu.RUnlock()
	return out
}

// read loads one saved run.
func (rs *runStore) read(clusterID, runID string) (*Run, error) {
	path := filepath.Join(rs.clusterDir(clusterID), sanitizeID(runID)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// find locates a run by identifier, live or saved.
func (rs *runStore) find(runID string) (*Run, bool) {
	rs.mu.RLock()
	if lr, ok := rs.live[runID]; ok {
		run := *lr.run
		rs.mu.RUnlock()
		return &run, true
	}
	rs.mu.RUnlock()

	// The identifier carries the cluster, so search every directory.
	entries, err := os.ReadDir(rs.dir)
	if err != nil {
		return nil, false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if run, err := rs.read(e.Name(), runID); err == nil {
			return run, true
		}
	}
	return nil, false
}

// latest returns the most recent completed run for a database.
func (rs *runStore) latest(clusterID string) (*Run, bool) {
	for _, summary := range rs.list(clusterID) {
		if summary.Status != RunComplete {
			continue
		}
		if run, err := rs.read(clusterID, summary.ID); err == nil {
			return run, true
		}
	}
	return nil, false
}

// sanitizeID keeps identifiers to a single safe path segment, so a crafted
// name cannot escape the run directory.
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "unnamed"
	}
	return out
}

// startAnalysis begins a run, returning immediately.
//
// Only one analysis per database runs at a time. A second request returns the
// one already in flight rather than doubling the load, which is the same
// reasoning that keeps the live loop from queueing ticks.
func (s *Server) startAnalysis(c *config.Cluster, trigger Trigger) (*Run, bool) {
	return s.startAnalysisFor(c, trigger, 0)
}

// startAnalysisFor begins a run, optionally recording live queries first.
func (s *Server) startAnalysisFor(c *config.Cluster, trigger Trigger, sampleFor time.Duration) (*Run, bool) {
	s.runs.mu.Lock()
	for _, lr := range s.runs.live {
		if lr.run.ClusterID == c.ID && !lr.done {
			run := *lr.run
			s.runs.mu.Unlock()
			return &run, false
		}
	}

	if sampleFor > maxSampleDuration {
		sampleFor = maxSampleDuration
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{
		ID:        fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405"), sanitizeID(c.ID)),
		ClusterID: c.ID,
		Trigger:   trigger,
		Status:    RunRunning,
		StartedAt: time.Now().UTC(),
		Phase:     phases[0],
		SampleFor: sampleFor,
	}
	lr := &liveRun{run: run, cancel: cancel, clients: map[chan Run]struct{}{}}
	s.runs.live[run.ID] = lr
	s.runs.mu.Unlock()

	go s.executeRun(ctx, c, lr)

	copied := *run
	return &copied, true
}

// executeRun performs the analysis, publishing progress as it goes.
func (s *Server) executeRun(ctx context.Context, c *config.Cluster, lr *liveRun) {
	defer func() {
		lr.mu.Lock()
		lr.done = true
		for ch := range lr.clients {
			close(ch)
		}
		lr.clients = map[chan Run]struct{}{}
		lr.mu.Unlock()

		// Keep the finished run addressable briefly, then rely on disk.
		time.AfterFunc(30*time.Second, func() {
			s.runs.mu.Lock()
			delete(s.runs.live, lr.run.ID)
			s.runs.mu.Unlock()
		})
	}()

	setPhase := func(phase string) {
		lr.mu.Lock()
		lr.run.Phase = phase
		snapshot := *lr.run
		for ch := range lr.clients {
			select {
			case ch <- snapshot:
			default:
			}
		}
		lr.mu.Unlock()
	}

	fail := func(err error) {
		lr.mu.Lock()
		lr.run.Status = RunFailed
		lr.run.Error = friendlyConnectError(err)
		lr.run.EndedAt = time.Now().UTC()
		snapshot := *lr.run
		for ch := range lr.clients {
			select {
			case ch <- snapshot:
			default:
			}
		}
		lr.mu.Unlock()
		_ = s.runs.save(lr.run)
	}

	setProgress := func(msg string) {
		lr.mu.Lock()
		lr.run.SampleProgress = msg
		snapshot := *lr.run
		for ch := range lr.clients {
			select {
			case ch <- snapshot:
			default:
			}
		}
		lr.mu.Unlock()
	}

	rep, err := s.runAnalysis(ctx, c, lr.run.SampleFor, setPhase, setProgress)
	if err != nil {
		if ctx.Err() != nil {
			lr.mu.Lock()
			lr.run.Status = RunCanceled
			lr.run.EndedAt = time.Now().UTC()
			lr.mu.Unlock()
			_ = s.runs.save(lr.run)
			return
		}
		fail(err)
		return
	}

	lr.mu.Lock()
	lr.run.Report = rep
	lr.run.Status = RunComplete
	lr.run.Phase = "done"
	lr.run.EndedAt = time.Now().UTC()
	lr.mu.Unlock()

	// Compare against the previous run before saving this one.
	if previous, ok := s.runs.latest(c.ID); ok {
		lr.run.Changes = diffRuns(previous, lr.run)
	}

	if err := s.runs.save(lr.run); err != nil {
		s.logf("could not save run %s: %v", lr.run.ID, err)
	}

	lr.mu.Lock()
	snapshot := *lr.run
	for ch := range lr.clients {
		select {
		case ch <- snapshot:
		default:
		}
	}
	lr.mu.Unlock()
}

// runAnalysis performs one collection and derives all three findings from it.
func (s *Server) runAnalysis(ctx context.Context, c *config.Cluster, sampleFor time.Duration,
	setPhase func(string), setProgress func(string)) (*report.Report, error) {
	setPhase(phases[0])

	var nodes []*db.Node
	defer func() {
		for _, n := range nodes {
			n.Close()
		}
	}()

	err := s.withGovernor(ctx, c.ID, false, func(ctx context.Context) error {
		for _, n := range c.Nodes {
			node, err := s.openNode(ctx, c, n)
			if err != nil {
				return err
			}
			nodes = append(nodes, node)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errNoNodes
	}

	setPhase(phases[1])
	var snaps []*collector.Snapshot
	err = s.withGovernor(ctx, c.ID, false, func(ctx context.Context) error {
		for _, node := range nodes {
			snap, err := collector.Collect(ctx, node, collector.Options{})
			if err != nil {
				return err
			}
			snaps = append(snaps, snap)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	setPhase(phases[2])
	rep := &report.Report{
		Kind:        report.KindAnalyze,
		Version:     s.opts.Version,
		GeneratedAt: time.Now().UTC(),
	}
	statements := 0
	for _, snap := range snaps {
		rep.Nodes = append(rep.Nodes, report.NodeSummaryFrom(snap.Node, len(snap.Statements)))
		rep.Warnings = append(rep.Warnings, snap.Warnings...)
		statements += len(snap.Statements)
	}

	// Unused and duplicate findings come from the same snapshot, at no extra
	// cost to the server.
	unused, unusedWarnings := advisor.Unused(snaps, advisor.UnusedOptions{})
	rep.Unused = unused
	rep.Warnings = append(rep.Warnings, unusedWarnings...)
	rep.Duplicates = advisor.Duplicates(snaps)
	rep.HotTables = advisor.HotTables(snaps, 1000, 20)

	// Recording live queries, for a database whose statistics cannot tell us
	// what runs on it.
	var recording *workload.Workload
	if sampleFor > 0 {
		setPhase(phaseSampling)
		started := time.Now()
		rec, err := workload.Sample(ctx, nodes, workload.SampleOptions{
			Interval: time.Second,
			Duration: sampleFor,
			Governor: s.governor,
			Progress: func(elapsed time.Duration, samples int64, shapes int) {
				remaining := sampleFor - elapsed
				if remaining < 0 {
					remaining = 0
				}
				setProgress(fmt.Sprintf(
					"watched for %s, %s to go; %d distinct quer%s seen so far",
					elapsed.Round(time.Second), remaining.Round(time.Second),
					shapes, plural2(shapes)))
			},
		})
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if rec != nil && len(rec.Queries) > 0 {
			recording = rec
			rep.Warnings = append(rep.Warnings, rec.Describe())
			statements = len(rec.Queries)
			_ = started
		} else {
			rep.Warnings = append(rep.Warnings,
				"nothing was running during the recording, so no queries could be observed. "+
					"Record again while the application is under its normal load.")
		}
	}

	if statements == 0 {
		rep.Mode = "table-only"
		rep.Warnings = append(rep.Warnings,
			"no query workload is visible, so idxa can report unused and duplicate indexes but cannot yet recommend new ones")
		return rep, nil
	}

	setPhase(phases[3])
	res := advisor.Missing(snaps, advisor.MissingOptions{
		MinRows:            1000,
		MaxRecommendations: 10,
		Workload:           recording,
	})
	setPhase(phases[5])
	rep.Warnings = append(rep.Warnings, res.Warnings...)
	rep.Recommendations = res.Recommendations
	rep.Mode = "heuristic"

	if len(rep.Recommendations) == 0 {
		return rep, nil
	}

	// Confirm with the planner where hypopg is available.
	for _, node := range nodes {
		if !node.Info.HasExtension("hypopg") {
			continue
		}
		setPhase(phases[6])
		q, err := validator.Acquire(ctx, node.Pool)
		if err != nil {
			break
		}
		validated, warns, err := advisor.Validate(ctx, q, node.Info.ServerVersionNum >= 160000,
			rep.Recommendations, validator.Budget{MaxCandidates: 10})
		q.Release()
		if err == nil && validated != nil {
			rep.Recommendations = validated
			rep.Warnings = append(rep.Warnings, warns...)
			rep.Mode = "validated"
		}
		break
	}
	return rep, nil
}

// diffRuns reports what changed between two runs.
func diffRuns(previous, current *Run) *RunChanges {
	if previous == nil || previous.Report == nil || current.Report == nil {
		return nil
	}
	was := map[string]bool{}
	for _, r := range previous.Report.Recommendations {
		was[recKey(r)] = true
	}
	now := map[string]bool{}
	for _, r := range current.Report.Recommendations {
		now[recKey(r)] = true
	}

	changes := &RunChanges{PreviousID: previous.ID}
	for _, r := range current.Report.Recommendations {
		if !was[recKey(r)] {
			changes.NewIndexes = append(changes.NewIndexes, recKey(r))
		}
	}
	for _, r := range previous.Report.Recommendations {
		if !now[recKey(r)] {
			changes.GoneIndexes = append(changes.GoneIndexes, recKey(r))
		}
	}
	if len(changes.NewIndexes) == 0 && len(changes.GoneIndexes) == 0 {
		return &RunChanges{PreviousID: previous.ID}
	}
	return changes
}

func recKey(r report.Recommendation) string {
	return fmt.Sprintf("%s.%s(%s)", r.Schema, r.Table, strings.Join(r.Columns, ", "))
}

// handleStartAnalysis begins a run.
func (s *Server) handleStartAnalysis(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}

	// An optional recording, for databases where pg_stat_statements cannot
	// tell us what runs. Absent or zero keeps the original behaviour exactly.
	var body struct {
		SampleSeconds int `json:"sample_seconds"`
	}
	if r.ContentLength > 0 {
		_ = decodeJSON(r, &body)
	}
	sampleFor := time.Duration(body.SampleSeconds) * time.Second
	if sampleFor < 0 {
		sampleFor = 0
	}

	run, started := s.startAnalysisFor(c, TriggerManual, sampleFor)
	status := http.StatusAccepted
	if !started {
		// An analysis is already running; hand back the same one rather than
		// making the database do the work twice.
		status = http.StatusOK
	}
	writeJSON(w, status, run)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.runs.list(c.ID)})
}

func (s *Server) handleLatestRun(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}
	run, ok := s.runs.latest(c.ID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"run": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runs.find(r.PathValue("runID"))
	if !ok {
		writeError(w, http.StatusNotFound, "no analysis with that identifier")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runID")
	s.runs.mu.RLock()
	lr, ok := s.runs.live[id]
	s.runs.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no analysis with that identifier is running")
		return
	}
	lr.cancel()
	writeJSON(w, http.StatusOK, map[string]string{"canceled": id})
}

// handleRunEvents streams progress for one run.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runID")

	s.runs.mu.RLock()
	lr, live := s.runs.live[id]
	s.runs.mu.RUnlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if !live {
		// Already finished: send the saved result and close, so a reconnect
		// after completion still shows the outcome.
		if run, found := s.runs.find(id); found {
			_ = writeSSE(w, "run", run)
			flusher.Flush()
		}
		return
	}

	ch := make(chan Run, 8)
	lr.mu.Lock()
	alreadyDone := lr.done
	if !alreadyDone {
		lr.clients[ch] = struct{}{}
	}
	current := *lr.run
	lr.mu.Unlock()

	_ = writeSSE(w, "run", current)
	flusher.Flush()
	if alreadyDone {
		return
	}

	defer func() {
		lr.mu.Lock()
		delete(lr.clients, ch)
		lr.mu.Unlock()
	}()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case run, open := <-ch:
			if !open {
				// Send the final saved state so the interface does not have
				// to poll for it.
				if final, found := s.runs.find(id); found {
					_ = writeSSE(w, "run", final)
					flusher.Flush()
				}
				return
			}
			if err := writeSSE(w, "run", run); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleExportRun renders a saved run in another format.
func (s *Server) handleExportRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runs.find(r.PathValue("runID"))
	if !ok || run.Report == nil {
		writeError(w, http.StatusNotFound, "no finished analysis with that identifier")
		return
	}

	format := report.Format(r.URL.Query().Get("format"))
	switch format {
	case report.FormatJSON, report.FormatMarkdown, report.FormatSQL, report.FormatTable:
	default:
		format = report.FormatJSON
	}

	contentType := map[report.Format]string{
		report.FormatJSON:     "application/json; charset=utf-8",
		report.FormatMarkdown: "text/markdown; charset=utf-8",
		report.FormatSQL:      "application/sql; charset=utf-8",
		report.FormatTable:    "text/plain; charset=utf-8",
	}[format]
	ext := map[report.Format]string{
		report.FormatJSON:     "json",
		report.FormatMarkdown: "md",
		report.FormatSQL:      "sql",
		report.FormatTable:    "txt",
	}[format]

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "idxa-"+run.ID+"."+ext))
	if err := report.Render(w, run.Report, format); err != nil {
		s.logf("export failed: %v", err)
	}
}

// writeSSE writes one server-sent event.
func writeSSE(w http.ResponseWriter, event string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

// plural2 is a small helper for the sampling progress line.
func plural2(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
