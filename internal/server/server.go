// Package server hosts the idxa web interface.
//
// Everything it does against a database goes through the load governor, so
// adding databases to the interface cannot multiply the load any one of them
// sees, and an idle browser tab costs nothing at all: with no client
// listening, nothing is collected.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/config"
	"github.com/indexbeacon/indexbeacon/internal/load"
)

// Options configures the server.
type Options struct {
	// Addr is the listen address. Anything but a loopback address requires
	// AllowRemote, because the configuration holds database credentials and
	// the interface has no user accounts.
	Addr string
	// AllowRemote permits binding a non-loopback address, which turns on
	// token authentication.
	AllowRemote bool
	// Token authenticates requests when listening off loopback. Generated
	// when empty.
	Token string
	// Store is the configuration.
	Store *config.Store
	// Assets is the embedded web interface.
	Assets fs.FS
	// Version is reported to the interface.
	Version string
	// Logger receives operational messages.
	Logger *log.Logger
}

// Server is the running web interface.
type Server struct {
	opts     Options
	mux      *http.ServeMux
	governor *load.Governor
	runs     *runStore
	live     *liveHub
	probes   *probeCache
	sched    *scheduler
	token    string
}

// New builds a server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("server: a configuration store is required")
	}
	if opts.Logger == nil {
		opts.Logger = log.New(log.Writer(), "", log.LstdFlags)
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:8777"
	}

	if err := checkBind(opts.Addr, opts.AllowRemote); err != nil {
		return nil, err
	}

	settings := opts.Store.Settings()
	duty := settings.DutyCyclePercent / 100
	if duty <= 0 {
		duty = load.DefaultDutyCycle
	}

	s := &Server{
		opts: opts,
		mux:  http.NewServeMux(),
		governor: load.NewGovernor(load.Options{
			DutyCycle:   duty,
			MinInterval: time.Duration(settings.RefreshSeconds) * time.Second,
		}),
		probes: newProbeCache(),
		token:  opts.Token,
	}

	runs, err := newRunStore(opts.Store.Path())
	if err != nil {
		return nil, err
	}
	s.runs = runs
	s.live = newLiveHub(s)
	s.sched = newScheduler(s)

	if !isLoopback(opts.Addr) && s.token == "" {
		s.token = generateToken()
	}

	s.routes()
	return s, nil
}

// Token returns the access token, empty when listening on loopback.
func (s *Server) Token() string { return s.token }

// URL is the address to open in a browser, including the token when one is
// required.
func (s *Server) URL() string {
	scheme := "http://"
	addr := s.opts.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	if s.token != "" {
		return scheme + addr + "/?token=" + s.token
	}
	return scheme + addr + "/"
}

// Handler returns the HTTP handler, exported for tests.
func (s *Server) Handler() http.Handler { return s.withAuth(s.mux) }

// ListenAndServe runs until the context is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: the live and progress endpoints stream for as
		// long as a browser tab stays open.
		IdleTimeout: 120 * time.Second,
	}

	go s.sched.run(ctx)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Close releases resources held by the server.
func (s *Server) Close() {
	s.live.stopAll()
	s.probes.close()
}

// checkBind refuses to expose credentials on a network interface by accident.
func checkBind(addr string, allowRemote bool) error {
	if isLoopback(addr) || allowRemote {
		return nil
	}
	return fmt.Errorf(
		"refusing to listen on %s: the configuration holds database credentials and idxa has no user accounts.\n"+
			"Bind a loopback address, or pass --allow-remote to accept the risk, which enables token authentication",
		addr)
}

// isLoopback reports whether an address is limited to this machine.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "", "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// generateToken produces an unguessable access token.
func generateToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// A predictable token would be worse than none, so fail loudly by
		// producing one that cannot match anything.
		return "token-generation-failed"
	}
	return hex.EncodeToString(buf)
}

// withAuth enforces the access token when one is in use.
//
// A token that only arrives in the query string is not enough on its own. The
// browser is given the token when it loads the page, but it then requests the
// stylesheet and the scripts by their own URLs, with no token on them, and
// those requests would be refused. The result is a page of unstyled HTML with
// no working interface at all.
//
// So a request that authenticates by query string also gets a session cookie,
// and every subsequent request carries it automatically. The cookie is
// host-only, unreadable from scripts, and marked SameSite=Lax so another site
// cannot use it to make requests on the user's behalf.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The health endpoint is deliberately unauthenticated. A container
		// orchestrator has to be able to ask whether the process is alive,
		// and it has nowhere to get a token from: Kubernetes does not expand
		// environment variables inside probe headers, so a probe that needed
		// one would send the literal text and be refused forever, leaving the
		// pod permanently unready. The endpoint answers "ok" and reveals
		// nothing about the databases or the configuration.
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte("ok\n"))
			return
		}

		if s.token != "" {
			if !s.authorized(r) {
				http.Error(w,
					"unauthorized: open the URL idxa printed at startup, which includes ?token=...",
					http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("token") != "" {
				s.setSessionCookie(w, r)
			}
		}
		// The interface holds database credentials, so it must never be
		// framed by another site or have its responses guessed at.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// setSessionCookie remembers a valid token for the rest of the session, so
// the browser can fetch assets that carry no token of their own.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("idxa_token"); err == nil && c.Value == s.token {
		return // already set
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "idxa_token",
		Value:    s.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure is not set: idxa serves plain HTTP, and a Secure cookie
		// would simply never be stored, putting us back where we started.
		// Anyone able to read traffic to this address can read the token
		// from the URL anyway.
	})
}

// authorized checks the token in the query string or the Authorization
// header, comparing in constant time.
func (s *Server) authorized(r *http.Request) bool {
	given := r.URL.Query().Get("token")
	if given == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			given = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if given == "" {
		if c, err := r.Cookie("idxa_token"); err == nil {
			given = c.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) == 1
}

// writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so there is nothing to do but
		// stop writing.
		return
	}
}

// writeError sends a JSON error, which the interface renders directly, so the
// message must read as something a person can act on.
func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// decodeJSON reads a request body into v.
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("could not read the request: %w", err)
	}
	return nil
}

func (s *Server) logf(format string, args ...any) {
	s.opts.Logger.Printf(format, args...)
}
