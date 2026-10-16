package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/config"
	"github.com/indexbeacon/indexbeacon/internal/report"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(Options{
		Addr:    "127.0.0.1:0",
		Store:   store,
		Assets:  fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>idxa</html>")}},
		Version: "test",
		Logger:  log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, srv.Handler()
}

func do(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(data))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

// --------------------------------------------------------------- binding

// TestRefusesNonLoopbackWithoutPermission is a security test. The
// configuration holds database credentials and the interface has no user
// accounts, so binding a public address must be a deliberate act.
func TestRefusesNonLoopbackWithoutPermission(t *testing.T) {
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))

	_, err := New(Options{Addr: "0.0.0.0:8777", Store: store, Logger: log.New(io.Discard, "", 0)})
	if err == nil {
		t.Fatal("binding 0.0.0.0 was allowed without --allow-remote")
	}
	if !strings.Contains(err.Error(), "allow-remote") {
		t.Errorf("the error should say how to proceed: %v", err)
	}

	srv, err := New(Options{
		Addr: "0.0.0.0:8777", AllowRemote: true, Store: store,
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("with --allow-remote: %v", err)
	}
	defer srv.Close()

	// Exposing it must switch authentication on, not merely permit the bind.
	if srv.Token() == "" {
		t.Error("a network-reachable server was started without an access token")
	}
	if !strings.Contains(srv.URL(), "token=") {
		t.Errorf("the printed URL should carry the token: %s", srv.URL())
	}
}

func TestLoopbackAddressesNeedNoToken(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8777", "localhost:8777", "[::1]:8777"} {
		store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
		srv, err := New(Options{Addr: addr, Store: store, Logger: log.New(io.Discard, "", 0)})
		if err != nil {
			t.Errorf("%s: %v", addr, err)
			continue
		}
		if srv.Token() != "" {
			t.Errorf("%s: a loopback server should not demand a token", addr)
		}
		srv.Close()
	}
}

func TestTokenIsEnforced(t *testing.T) {
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	srv, err := New(Options{
		Addr: "0.0.0.0:8777", AllowRemote: true, Token: "secret-token",
		Store: store, Assets: fstest.MapFS{}, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	handler := srv.Handler()

	if rec := do(t, handler, "GET", "/api/clusters", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("without a token = %d, want 401", rec.Code)
	}
	if rec := do(t, handler, "GET", "/api/clusters?token=wrong", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("with the wrong token = %d, want 401", rec.Code)
	}
	if rec := do(t, handler, "GET", "/api/clusters?token=secret-token", nil); rec.Code != http.StatusOK {
		t.Errorf("with the right token = %d, want 200", rec.Code)
	}

	req := httptest.NewRequest("GET", "/api/clusters", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("bearer header = %d, want 200", rec.Code)
	}
}

// --------------------------------------------------------------- clusters

func TestClusterLifecycle(t *testing.T) {
	_, handler := newTestServer(t)

	rec := do(t, handler, "POST", "/api/clusters", map[string]any{
		"name":     "Production",
		"nodes":    []map[string]string{{"dsn": "postgres://u:p@localhost:5432/app"}},
		"schedule": map[string]string{"mode": "manual"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}

	var created ClusterView
	decode(t, rec, &created)
	if created.ID != "production" {
		t.Errorf("id = %q", created.ID)
	}

	// The password must never come back over the wire.
	if strings.Contains(rec.Body.String(), "u:p@") {
		t.Errorf("the response leaked the password: %s", rec.Body)
	}
	if !strings.Contains(created.Nodes[0].DSN, "****") {
		t.Errorf("connection string was not redacted: %s", created.Nodes[0].DSN)
	}

	if rec := do(t, handler, "GET", "/api/clusters/production", nil); rec.Code != http.StatusOK {
		t.Errorf("get = %d", rec.Code)
	}
	if rec := do(t, handler, "GET", "/api/clusters/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown database = %d, want 404", rec.Code)
	}
	if rec := do(t, handler, "DELETE", "/api/clusters/production", nil); rec.Code != http.StatusOK {
		t.Errorf("delete = %d", rec.Code)
	}
	if rec := do(t, handler, "GET", "/api/clusters/production", nil); rec.Code != http.StatusNotFound {
		t.Errorf("after delete = %d, want 404", rec.Code)
	}
}

// TestEditingKeepsCredentials covers a bug that would lock a user out of
// their own database: the interface shows connection strings redacted, so a
// form that submits what it was shown would save "****" as the password.
func TestEditingKeepsCredentials(t *testing.T) {
	srv, handler := newTestServer(t)

	do(t, handler, "POST", "/api/clusters", map[string]any{
		"name":     "Prod",
		"nodes":    []map[string]string{{"dsn": "postgres://user:realpassword@host:5432/app"}},
		"schedule": map[string]string{"mode": "manual"},
	})

	var view ClusterView
	decode(t, do(t, handler, "GET", "/api/clusters/prod", nil), &view)

	// Submit exactly what the interface displays, changing only the name.
	rec := do(t, handler, "PUT", "/api/clusters/prod", map[string]any{
		"name":     "Renamed",
		"nodes":    []map[string]string{{"dsn": view.Nodes[0].DSN}},
		"schedule": map[string]string{"mode": "manual"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body)
	}

	stored, ok := srv.opts.Store.Cluster("prod")
	if !ok {
		t.Fatal("cluster disappeared")
	}
	if stored.Nodes[0].DSN != "postgres://user:realpassword@host:5432/app" {
		t.Errorf("the password was destroyed by an edit: %q", stored.Nodes[0].DSN)
	}
	if stored.Name != "Renamed" {
		t.Errorf("the rename did not take: %q", stored.Name)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	_, handler := newTestServer(t)

	cases := []map[string]any{
		{"nodes": []map[string]string{{"dsn": "postgres://h/d"}}}, // no name
		{"name": "x"}, // no nodes
		{"name": "x", "nodes": []map[string]string{{"dsn": "  "}}}, // empty dsn
	}
	for _, body := range cases {
		rec := do(t, handler, "POST", "/api/clusters", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid input accepted (%v): %d", body, rec.Code)
		}
		var out map[string]string
		decode(t, rec, &out)
		if out["error"] == "" {
			t.Error("the error response carried no message for the interface to show")
		}
	}
}

// --------------------------------------------------------------- schedules

func TestSchedulePreviewComputesNextRuns(t *testing.T) {
	// The preview is what tells a person what their cron expression means
	// before they commit to it.
	_, handler := newTestServer(t)
	do(t, handler, "POST", "/api/clusters", map[string]any{
		"name":     "Prod",
		"nodes":    []map[string]string{{"dsn": "postgres://h/d"}},
		"schedule": map[string]string{"mode": "manual"},
	})

	rec := do(t, handler, "PUT", "/api/clusters/prod/schedule/preview", map[string]any{
		"mode": "cron", "cron": "0 3 * * *", "timezone": "Asia/Kolkata",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d: %s", rec.Code, rec.Body)
	}

	var view ScheduleView
	decode(t, rec, &view)
	if view.Error != "" {
		t.Fatalf("valid expression reported an error: %s", view.Error)
	}
	if len(view.NextRuns) < 3 {
		t.Errorf("preview returned %d runs, want three so the user can see the pattern", len(view.NextRuns))
	}
	for i := 1; i < len(view.NextRuns); i++ {
		if !view.NextRuns[i].After(view.NextRuns[i-1]) {
			t.Error("next runs are not in ascending order")
		}
	}
	if view.Describe == "" {
		t.Error("the schedule was not described in words")
	}
}

func TestScheduleRejectsBadExpressions(t *testing.T) {
	_, handler := newTestServer(t)
	do(t, handler, "POST", "/api/clusters", map[string]any{
		"name":     "Prod",
		"nodes":    []map[string]string{{"dsn": "postgres://h/d"}},
		"schedule": map[string]string{"mode": "manual"},
	})

	// A bad expression must be refused when saved, not at the next firing
	// where nobody would see it.
	rec := do(t, handler, "PUT", "/api/clusters/prod/schedule", map[string]any{
		"mode": "cron", "cron": "not a cron expression",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid cron accepted: %d %s", rec.Code, rec.Body)
	}

	rec = do(t, handler, "PUT", "/api/clusters/prod/schedule", map[string]any{
		"mode": "cron", "cron": "0 3 * * *", "timezone": "Mars/Olympus",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown timezone accepted: %d", rec.Code)
	}
}

func TestScheduleSurvivesAndDefaultsToManual(t *testing.T) {
	_, handler := newTestServer(t)
	do(t, handler, "POST", "/api/clusters", map[string]any{
		"name":     "Prod",
		"nodes":    []map[string]string{{"dsn": "postgres://h/d"}},
		"schedule": map[string]string{"mode": "manual"},
	})

	var view ScheduleView
	decode(t, do(t, handler, "GET", "/api/clusters/prod/schedule", nil), &view)
	if view.Mode != config.ScheduleManual {
		t.Errorf("default mode = %q, want manual: automatic queries must be opt-in", view.Mode)
	}

	if rec := do(t, handler, "PUT", "/api/clusters/prod/schedule", map[string]any{
		"mode": "cron", "cron": "0 3 * * *", "timezone": "UTC",
	}); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body)
	}

	decode(t, do(t, handler, "GET", "/api/clusters/prod/schedule", nil), &view)
	if view.Cron != "0 3 * * *" {
		t.Errorf("schedule did not persist: %+v", view)
	}
}

// TestJitterIsStableAndBounded covers the property that keeps ten databases
// on a daily schedule from all firing at once and undoing the load budget.
func TestJitterIsStableAndBounded(t *testing.T) {
	first := jitterFor("production")
	if first != jitterFor("production") {
		t.Error("jitter is not stable for the same database, so runs would drift on every restart")
	}
	if first < 0 || first >= maxJitter {
		t.Errorf("jitter %v is outside the permitted spread", first)
	}
	if jitterFor("production") == jitterFor("staging") {
		t.Error("two databases got the same offset, which defeats the point of spreading them")
	}
}

// --------------------------------------------------------------- runs

func TestRunHistoryRoundTripAndPruning(t *testing.T) {
	srv, _ := newTestServer(t)

	for i := 0; i < maxRunsPerCluster+5; i++ {
		run := &Run{
			ID:        time.Now().Add(time.Duration(i)*time.Second).UTC().Format("20060102-150405.000") + "-x",
			ClusterID: "prod",
			Status:    RunComplete,
			StartedAt: time.Now().Add(time.Duration(i) * time.Second),
			EndedAt:   time.Now().Add(time.Duration(i) * time.Second),
		}
		if err := srv.runs.save(run); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	entries, err := os.ReadDir(srv.runs.clusterDir("prod"))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) > maxRunsPerCluster {
		t.Errorf("%d runs kept on disk, the cap is %d", len(entries), maxRunsPerCluster)
	}

	list := srv.runs.list("prod")
	for i := 1; i < len(list); i++ {
		if list[i].StartedAt.After(list[i-1].StartedAt) {
			t.Error("runs are not listed newest first")
			break
		}
	}
}

// TestRunIdentifiersCannotEscapeTheDirectory guards against a crafted
// identifier reading or writing outside the run directory.
func TestRunIdentifiersCannotEscapeTheDirectory(t *testing.T) {
	for _, input := range []string{"../../etc/passwd", "..", "a/b/c", "with spaces"} {
		got := sanitizeID(input)
		if strings.Contains(got, "/") || strings.Contains(got, "..") {
			t.Errorf("sanitizeID(%q) = %q, which is not a single safe segment", input, got)
		}
	}
	if sanitizeID("") == "" {
		t.Error("an empty identifier must still produce a usable name")
	}
}

func TestUnknownRunReturnsNotFound(t *testing.T) {
	_, handler := newTestServer(t)
	if rec := do(t, handler, "GET", "/api/runs/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", rec.Code)
	}
	if rec := do(t, handler, "DELETE", "/api/runs/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cancelling an unknown run = %d, want 404", rec.Code)
	}
}

func TestDiffRunsMarksWhatChanged(t *testing.T) {
	previous := &Run{ID: "old", Report: reportWith("orders", "customer_id")}
	current := &Run{ID: "new", Report: reportWith("events", "occurred_at")}

	changes := diffRuns(previous, current)
	if changes == nil {
		t.Fatal("no changes computed")
	}
	if len(changes.NewIndexes) != 1 {
		t.Errorf("new = %v, want the events recommendation", changes.NewIndexes)
	}
	if len(changes.GoneIndexes) != 1 {
		t.Errorf("resolved = %v, want the orders recommendation", changes.GoneIndexes)
	}

	same := diffRuns(previous, &Run{ID: "same", Report: reportWith("orders", "customer_id")})
	if len(same.NewIndexes) != 0 || len(same.GoneIndexes) != 0 {
		t.Errorf("an unchanged run reported changes: %+v", same)
	}
	if diffRuns(nil, current) != nil {
		t.Error("diffing against no previous run should produce nothing")
	}
}

// --------------------------------------------------------------- static

func TestUnknownPathsServeTheInterface(t *testing.T) {
	// Client-side routes must survive a reload rather than 404.
	_, handler := newTestServer(t)
	rec := do(t, handler, "GET", "/db/production/recommendations", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("deep link = %d, want the interface", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "idxa") {
		t.Errorf("deep link did not return the document: %s", rec.Body)
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	_, handler := newTestServer(t)
	rec := do(t, handler, "GET", "/api/clusters", nil)

	// The interface holds database credentials, so it must not be framable.
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

func TestCostEndpointReports(t *testing.T) {
	_, handler := newTestServer(t)
	rec := do(t, handler, "GET", "/api/cost", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cost = %d", rec.Code)
	}
	var costs map[string]CostView
	decode(t, rec, &costs)
}

// --------------------------------------------------------------- helpers

func TestFriendlyConnectErrors(t *testing.T) {
	// A person reading an error should learn what to do, not just what the
	// driver called it.
	cases := map[string]string{
		"dial tcp 127.0.0.1:5432: connect: connection refused": "nothing is listening",
		"lookup nosuchhost: no such host":                      "check the hostname",
		"password authentication failed for user \"app\"":      "check the user and password",
		"context deadline exceeded":                            "firewall",
	}
	for input, want := range cases {
		got := friendlyConnectError(errString(input))
		if !strings.Contains(got, want) {
			t.Errorf("friendlyConnectError(%q) = %q, expected it to mention %q", input, got, want)
		}
	}
}

func TestHeadlineDescribesFindings(t *testing.T) {
	view := ClusterView{LastRun: &RunSummary{
		Status: RunComplete, Recommendations: 3, Unused: 2, Duplicates: 1,
		ReclaimBytes: 5 * 1024 * 1024,
	}}
	got := headline(view)
	for _, want := range []string{"3 missing", "2 unused", "1 redundant", "reclaimable"} {
		if !strings.Contains(got, want) {
			t.Errorf("headline = %q, missing %q", got, want)
		}
	}

	if got := headline(ClusterView{}); got != "not analysed yet" {
		t.Errorf("headline with no run = %q", got)
	}
	clean := headline(ClusterView{LastRun: &RunSummary{Status: RunComplete}})
	if clean != "nothing to change" {
		t.Errorf("headline with a clean result = %q", clean)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// reportWith builds a minimal report carrying one recommendation.
func reportWith(table, column string) *report.Report {
	return &report.Report{
		Recommendations: []report.Recommendation{{
			Schema: "shop", Table: table, Columns: []string{column},
		}},
	}
}

// TestAssetsLoadInTokenMode covers a bug that made the whole interface
// unusable whenever a token was required, which is always the case in a
// container.
//
// The browser is handed the token when it loads the page, but it then fetches
// the stylesheet and the scripts by their own URLs, which carry no token.
// Those requests were refused, so the user got a page of unstyled HTML and no
// working interface. A curl of the API kept passing, because curl was given
// the token every time.
func TestAssetsLoadInTokenMode(t *testing.T) {
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	srv, err := New(Options{
		Addr: "0.0.0.0:8777", AllowRemote: true, Token: "secret",
		Store: store, Logger: log.New(io.Discard, "", 0),
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<link rel="stylesheet" href="/app.css">`)},
			"app.css":    &fstest.MapFile{Data: []byte("body{}")},
			"app.js":     &fstest.MapFile{Data: []byte("export {}")},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	handler := srv.Handler()

	// Loading the page with the token must hand back a session cookie.
	page := do(t, handler, "GET", "/?token=secret", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("page = %d", page.Code)
	}

	var session *http.Cookie
	for _, c := range page.Result().Cookies() {
		if c.Name == "idxa_token" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie was set, so assets will be refused")
	}
	if !session.HttpOnly {
		t.Error("the session cookie should not be readable from scripts")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Error("the session cookie should be SameSite=Lax")
	}

	// Now the browser fetches assets the way it actually does: by plain URL,
	// carrying only the cookie.
	for _, path := range []string{"/app.css", "/app.js"} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(session)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with the session cookie = %d, want 200: the interface would render unstyled",
				path, rec.Code)
		}
	}

	// Without the cookie or a token, everything is still refused.
	if rec := do(t, handler, "GET", "/app.css", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("/app.css with no credentials = %d, want 401", rec.Code)
	}

	// A wrong cookie must not be accepted.
	req := httptest.NewRequest("GET", "/app.css", nil)
	req.AddCookie(&http.Cookie{Name: "idxa_token", Value: "wrong"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/app.css with a wrong cookie = %d, want 401", rec.Code)
	}
}

func TestNoCookieIssuedOnLoopback(t *testing.T) {
	// With no token in use there is nothing to remember.
	_, handler := newTestServer(t)
	rec := do(t, handler, "GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "idxa_token" {
			t.Error("a cookie was set even though no token is required")
		}
	}
}

// TestHealthEndpointNeedsNoToken covers why the endpoint exists at all.
//
// Kubernetes does not expand environment variables inside probe headers, so a
// readiness probe cannot carry the access token. Pointing one at an
// authenticated path makes the kubelet send a literal "$(IDXA_TOKEN)", get a
// 401, and mark the pod unready forever, which is exactly what happened the
// first time this was deployed.
func TestHealthEndpointNeedsNoToken(t *testing.T) {
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	srv, err := New(Options{
		Addr: "0.0.0.0:8777", AllowRemote: true, Token: "secret",
		Store: store, Assets: fstest.MapFS{}, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	rec := do(t, srv.Handler(), "GET", "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz without a token = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "ok") {
		t.Errorf("/healthz body = %q", body)
	}

	// It must stay boring: no configuration, no database names, no token.
	if strings.Contains(rec.Body.String(), "secret") {
		t.Error("/healthz leaked the token")
	}

	// Everything else still needs credentials.
	if rec := do(t, srv.Handler(), "GET", "/api/clusters", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the health endpoint must not open anything else: /api/clusters = %d", rec.Code)
	}
}
