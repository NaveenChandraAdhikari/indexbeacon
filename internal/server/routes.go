package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/indexbeacon/indexbeacon/internal/config"
)

// routes registers every endpoint.
func (s *Server) routes() {
	m := s.mux

	// Configuration.
	m.HandleFunc("GET /api/settings", s.handleGetSettings)
	m.HandleFunc("PUT /api/settings", s.handlePutSettings)
	m.HandleFunc("GET /api/clusters", s.handleListClusters)
	m.HandleFunc("POST /api/clusters", s.handleCreateCluster)
	m.HandleFunc("POST /api/clusters/test", s.handleTestConnection)
	m.HandleFunc("GET /api/clusters/{id}", s.handleGetCluster)
	m.HandleFunc("PUT /api/clusters/{id}", s.handleUpdateCluster)
	m.HandleFunc("DELETE /api/clusters/{id}", s.handleDeleteCluster)

	// Inspection.
	m.HandleFunc("GET /api/clusters/{id}/check", s.handleCheck)
	m.HandleFunc("GET /api/clusters/{id}/overview", s.handleOverview)
	m.HandleFunc("GET /api/clusters/{id}/live", s.handleLive)

	// Analysis.
	m.HandleFunc("POST /api/clusters/{id}/analyze", s.handleStartAnalysis)
	m.HandleFunc("GET /api/clusters/{id}/runs", s.handleListRuns)
	m.HandleFunc("GET /api/clusters/{id}/latest", s.handleLatestRun)
	m.HandleFunc("GET /api/runs/{runID}", s.handleGetRun)
	m.HandleFunc("GET /api/runs/{runID}/events", s.handleRunEvents)
	m.HandleFunc("DELETE /api/runs/{runID}", s.handleCancelRun)
	m.HandleFunc("GET /api/runs/{runID}/export", s.handleExportRun)

	// Scheduling.
	m.HandleFunc("GET /api/schedules", s.handleListSchedules)
	m.HandleFunc("GET /api/clusters/{id}/schedule", s.handleGetSchedule)
	m.HandleFunc("PUT /api/clusters/{id}/schedule", s.handlePutSchedule)
	m.HandleFunc("PUT /api/clusters/{id}/schedule/preview", s.handlePreviewSchedule)

	// Cost of the tool itself, so a database administrator can see what
	// idxa is spending on their server.
	m.HandleFunc("GET /api/cost", s.handleCost)

	m.Handle("/", s.staticHandler())
}

// staticHandler serves the embedded interface, falling back to the single
// page document so client-side routes reload correctly.
func (s *Server) staticHandler() http.Handler {
	if s.opts.Assets == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "the web interface was not built into this binary", http.StatusNotFound)
		})
	}
	files := http.FileServer(http.FS(s.opts.Assets))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" {
			serveIndex(w, r, s.opts.Assets)
			return
		}
		if _, err := fs.Stat(s.opts.Assets, clean); err != nil {
			// An unknown path is a client-side route, so hand back the
			// document and let the interface render it.
			serveIndex(w, r, s.opts.Assets)
			return
		}
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "the web interface was not built into this binary", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// cluster resolves the {id} path value, writing a 404 when it is unknown.
func (s *Server) cluster(w http.ResponseWriter, r *http.Request) (*config.Cluster, bool) {
	id := r.PathValue("id")
	c, ok := s.opts.Store.Cluster(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no database named %q is configured", id)
		return nil, false
	}
	return c, true
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.opts.Store.Settings())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in config.Settings
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := s.opts.Store.SetSettings(in); err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Store.Settings())
}

func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"clusters": s.clusterViews(r.Context()),
		"version":  s.opts.Version,
	})
}

func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.clusterView(r.Context(), c))
}

func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	var in config.Cluster
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	added, err := s.opts.Store.Add(&in)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	// Probe immediately so the interface can report a missing extension
	// straight away, but never start an analysis: that would spend somebody's
	// production capacity without being asked.
	go s.probes.refresh(context.Background(), s, added)

	writeJSON(w, http.StatusCreated, s.clusterView(r.Context(), added))
}

func (s *Server) handleUpdateCluster(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in config.Cluster
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	// Connection strings are shown redacted, so a form that submits what it
	// was given would save "****" as the password and lock the user out of
	// their own database. Any submitted string that matches the redacted form
	// of a stored one is treated as unchanged.
	if existing, ok := s.opts.Store.Cluster(id); ok {
		restoreRedactedDSNs(&in, existing)
	}

	updated, err := s.opts.Store.Update(id, &in)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.probes.invalidate(id)
	writeJSON(w, http.StatusOK, s.clusterView(r.Context(), updated))
}

func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.opts.Store.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, "%v", err)
		return
	}
	s.probes.invalidate(id)
	s.live.stop(id)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"schedules": s.sched.summaries()})
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.sched.describe(c))
}

func (s *Server) handlePutSchedule(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}
	var in config.Schedule
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	c.Schedule = in
	if err := c.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	// Reject an unparseable cron expression here rather than at the next
	// firing, where nobody would see the error.
	if _, err := s.sched.nextRuns(c, 1); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if _, err := s.opts.Store.Update(c.ID, c); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.sched.reload()
	writeJSON(w, http.StatusOK, s.sched.describe(c))
}

func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.costReport())
}

// restoreRedactedDSNs replaces submitted connection strings that are merely
// the redacted rendering of a stored one with the original, so renaming a
// database or editing its schedule does not destroy its credentials.
func restoreRedactedDSNs(incoming, existing *config.Cluster) {
	for i, node := range incoming.Nodes {
		if !strings.Contains(node.DSN, "****") {
			continue
		}
		for _, old := range existing.Nodes {
			if config.Redact(old.DSN) == node.DSN {
				incoming.Nodes[i].DSN = old.DSN
				break
			}
		}
	}
}

var errNoNodes = errors.New("this database has no connection strings")

// handlePreviewSchedule computes firing times for a schedule without saving
// it, so the settings form can show what a cron expression actually means
// before the user commits to it.
func (s *Server) handlePreviewSchedule(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}
	var in config.Schedule
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	probe := *c
	probe.Schedule = in
	if err := probe.Validate(); err != nil {
		writeJSON(w, http.StatusOK, ScheduleView{Mode: in.Mode, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.sched.describe(&probe))
}
