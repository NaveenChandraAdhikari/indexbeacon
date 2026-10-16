package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/config"
)

// ClusterView is what the sidebar and the overview render.
//
// Each row carries a finding, not just a name. A list of database names is
// navigation; a list that says which database has the worst problem answers
// the question the user actually came with.
type ClusterView struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	NodeCount   int                 `json:"node_count"`
	Nodes       []NodeView          `json:"nodes"`
	Status      *ClusterStatus      `json:"status,omitempty"`
	Schedule    ScheduleView        `json:"schedule"`
	LastRun     *RunSummary         `json:"last_run,omitempty"`
	Headline    string              `json:"headline"`
	Mode        config.ScheduleMode `json:"-"`
}

// NodeView is one configured connection, always redacted.
type NodeView struct {
	Name  string `json:"name"`
	DSN   string `json:"dsn"`
	Label string `json:"label"`
	// InlinePassword warns that the password is stored in the file, so the
	// interface can suggest moving it to an environment variable.
	InlinePassword bool `json:"inline_password"`
}

// RunSummary is the headline of a finished analysis.
type RunSummary struct {
	ID              string    `json:"id"`
	Status          RunStatus `json:"status"`
	Trigger         Trigger   `json:"trigger"`
	EndedAt         time.Time `json:"ended_at"`
	Recommendations int       `json:"recommendations"`
	Unused          int       `json:"unused"`
	Duplicates      int       `json:"duplicates"`
	ReclaimBytes    int64     `json:"reclaimable_bytes"`
	NewSinceLast    int       `json:"new_since_last"`
	Mode            string    `json:"mode,omitempty"`
}

// clusterViews renders every configured database.
func (s *Server) clusterViews(ctx context.Context) []ClusterView {
	clusters := s.opts.Store.Clusters()
	out := make([]ClusterView, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, s.clusterView(ctx, c))
	}
	return out
}

// clusterView renders one database.
//
// It never blocks on a database connection. A cached probe is used when one
// is available and a refresh is started in the background otherwise, so the
// page paints immediately and fills in as answers arrive. Waiting on an
// unreachable server would leave the whole interface blank.
func (s *Server) clusterView(ctx context.Context, c *config.Cluster) ClusterView {
	v := ClusterView{
		ID:          c.ID,
		Name:        c.Name,
		Description: c.Description,
		NodeCount:   len(c.Nodes),
		Schedule:    s.sched.describe(c),
	}
	for _, n := range c.Nodes {
		name := n.Name
		if name == "" {
			name = config.Describe(n.DSN)
		}
		v.Nodes = append(v.Nodes, NodeView{
			Name:           name,
			DSN:            config.Redact(n.DSN),
			Label:          config.Describe(n.DSN),
			InlinePassword: config.HasInlinePassword(n.DSN),
		})
	}

	if status, fresh := s.probes.get(c.ID); status != nil {
		v.Status = status
		if !fresh {
			go s.probes.refresh(context.Background(), s, c)
		}
	} else {
		go s.probes.refresh(context.Background(), s, c)
	}

	if run, ok := s.runs.latest(c.ID); ok {
		v.LastRun = summarize(run)
	}
	v.Headline = headline(v)
	return v
}

// summarize reduces a run to its headline numbers.
func summarize(run *Run) *RunSummary {
	sum := &RunSummary{
		ID:      run.ID,
		Status:  run.Status,
		Trigger: run.Trigger,
		EndedAt: run.EndedAt,
	}
	if run.Report == nil {
		return sum
	}
	sum.Mode = run.Report.Mode
	sum.Recommendations = len(run.Report.Recommendations)
	sum.Unused = len(run.Report.Unused)
	sum.Duplicates = len(run.Report.Duplicates)
	for _, u := range run.Report.Unused {
		sum.ReclaimBytes += u.SizeBytes
	}
	for _, d := range run.Report.Duplicates {
		sum.ReclaimBytes += d.RedundantSize
	}
	if run.Changes != nil {
		sum.NewSinceLast = len(run.Changes.NewIndexes)
	}
	return sum
}

// headline is the single sentence shown on a database's row.
func headline(v ClusterView) string {
	if v.Status != nil && v.Status.State == "error" {
		return v.Status.Summary
	}
	if v.LastRun == nil {
		return "not analysed yet"
	}
	r := v.LastRun
	switch {
	case r.Status == RunFailed:
		return "the last analysis failed"
	case r.Recommendations == 0 && r.Unused == 0 && r.Duplicates == 0:
		return "nothing to change"
	}

	parts := make([]string, 0, 3)
	if r.Recommendations > 0 {
		parts = append(parts, fmt.Sprintf("%d missing index%s", r.Recommendations, es(r.Recommendations)))
	}
	if r.Unused > 0 {
		parts = append(parts, fmt.Sprintf("%d unused", r.Unused))
	}
	if r.Duplicates > 0 {
		parts = append(parts, fmt.Sprintf("%d redundant", r.Duplicates))
	}
	line := joinWords(parts)
	if r.ReclaimBytes > 0 {
		line += fmt.Sprintf(", %s reclaimable", humanBytes(r.ReclaimBytes))
	}
	return line
}

func es(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func joinWords(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		out := ""
		for i, p := range parts[:len(parts)-1] {
			if i > 0 {
				out += ", "
			}
			out += p
		}
		return out + " and " + parts[len(parts)-1]
	}
}

// humanBytes renders a size the way PostgreSQL does.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d bytes", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}

// CostView reports what idxa has spent on one database, so a database
// administrator can see the answer rather than take it on trust.
type CostView struct {
	Statements  int64   `json:"statements"`
	TotalMS     float64 `json:"total_ms"`
	LastMS      float64 `json:"last_ms"`
	SlowestMS   float64 `json:"slowest_ms"`
	DutyPercent float64 `json:"duty_percent"`
	CooldownSec float64 `json:"cooldown_seconds"`
	Skipped     int64   `json:"skipped_ticks"`
}

func (s *Server) costFor(clusterID string) CostView {
	st := s.governor.Stats(clusterID)
	return CostView{
		Statements:  st.Statements,
		TotalMS:     float64(st.TotalTime.Microseconds()) / 1000,
		LastMS:      float64(st.LastTime.Microseconds()) / 1000,
		SlowestMS:   float64(st.SlowestTime.Microseconds()) / 1000,
		DutyPercent: st.DutyCycle() * 100,
		CooldownSec: st.Cooldown.Seconds(),
		Skipped:     st.Skipped,
	}
}

func (s *Server) costReport() map[string]CostView {
	out := map[string]CostView{}
	for id, st := range s.governor.AllStats() {
		out[id] = CostView{
			Statements:  st.Statements,
			TotalMS:     float64(st.TotalTime.Microseconds()) / 1000,
			LastMS:      float64(st.LastTime.Microseconds()) / 1000,
			SlowestMS:   float64(st.SlowestTime.Microseconds()) / 1000,
			DutyPercent: st.DutyCycle() * 100,
			CooldownSec: st.Cooldown.Seconds(),
			Skipped:     st.Skipped,
		}
	}
	return out
}

// handleCheck probes a database on demand.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.probes.refresh(r.Context(), s, c))
}

// handleTestConnection tries a connection set without saving it, so the add
// form can give a verdict before anything is written to disk.
func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	var in config.Cluster
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if len(in.Nodes) == 0 {
		writeError(w, http.StatusBadRequest, "enter at least one connection string")
		return
	}
	if in.ID == "" {
		in.ID = "connection-test"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, probeCluster(ctx, s, &in))
}

// handleOverview returns the last analysis plus current status, which is what
// a database's landing page shows. Opening on data rather than a menu of
// buttons means the visit is useful before anything is clicked.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}

	resp := map[string]any{
		"cluster": s.clusterView(r.Context(), c),
		"cost":    s.costFor(c.ID),
	}
	if run, found := s.runs.latest(c.ID); found {
		resp["run"] = run
	} else {
		resp["run"] = nil
	}
	resp["runs"] = s.runs.list(c.ID)
	writeJSON(w, http.StatusOK, resp)
}
