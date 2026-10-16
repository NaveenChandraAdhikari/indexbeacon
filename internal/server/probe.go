package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/config"
	"github.com/indexbeacon/indexbeacon/internal/db"
	"github.com/indexbeacon/indexbeacon/internal/load"
)

// NodeStatus is what the interface shows about one instance.
type NodeStatus struct {
	Name        string `json:"name"`
	DSN         string `json:"dsn"` // redacted, always
	Role        string `json:"role"`
	Version     string `json:"version,omitempty"`
	VersionNum  int    `json:"version_num,omitempty"`
	Reachable   bool   `json:"reachable"`
	Error       string `json:"error,omitempty"`
	HasPSS      bool   `json:"pg_stat_statements"`
	PSSLoaded   bool   `json:"pg_stat_statements_preloaded"`
	HasHypoPG   bool   `json:"hypopg"`
	Superuser   bool   `json:"superuser"`
	CanReadAll  bool   `json:"can_read_all_stats"`
	StatsAgeSec int64  `json:"stats_age_seconds"`
	StatsKnown  bool   `json:"stats_age_known"`
}

// ClusterStatus summarises a whole database for the interface.
type ClusterStatus struct {
	// State is ok, warning or error, which drives the status dot.
	State string `json:"state"`
	// Summary is a single sentence a person can act on.
	Summary string `json:"summary"`
	// Advice lists concrete next steps, such as the command that installs a
	// missing extension. An empty state that only says "no data" is useless.
	Advice []string     `json:"advice,omitempty"`
	Nodes  []NodeStatus `json:"nodes"`
	// CheckedAt is when this was last probed.
	CheckedAt time.Time `json:"checked_at"`
}

// probeCache remembers what each database looked like, so the sidebar can
// render immediately without waiting on a connection, and so a page reload
// does not re-probe every server.
type probeCache struct {
	mu      sync.RWMutex
	entries map[string]*ClusterStatus
	// inflight prevents two probes of the same database at once.
	inflight map[string]bool
	ttl      time.Duration
}

func newProbeCache() *probeCache {
	return &probeCache{
		entries:  map[string]*ClusterStatus{},
		inflight: map[string]bool{},
		ttl:      30 * time.Second,
	}
}

func (p *probeCache) get(id string) (*ClusterStatus, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st, ok := p.entries[id]
	if !ok {
		return nil, false
	}
	return st, time.Since(st.CheckedAt) < p.ttl
}

func (p *probeCache) put(id string, st *ClusterStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[id] = st
}

func (p *probeCache) invalidate(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, id)
}

func (p *probeCache) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = map[string]*ClusterStatus{}
}

// refresh probes a database unless one is already in flight.
func (p *probeCache) refresh(ctx context.Context, s *Server, c *config.Cluster) *ClusterStatus {
	p.mu.Lock()
	if p.inflight[c.ID] {
		st, ok := p.entries[c.ID]
		p.mu.Unlock()
		if ok {
			return st
		}
		return &ClusterStatus{State: "warning", Summary: "checking", CheckedAt: time.Now()}
	}
	p.inflight[c.ID] = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.inflight, c.ID)
		p.mu.Unlock()
	}()

	st := probeCluster(ctx, s, c)
	p.put(c.ID, st)
	return st
}

// probeCluster opens each node, records what it found, and turns that into
// advice a person can act on.
func probeCluster(ctx context.Context, s *Server, c *config.Cluster) *ClusterStatus {
	st := &ClusterStatus{CheckedAt: time.Now(), State: "ok"}

	if len(c.Nodes) == 0 {
		st.State = "error"
		st.Summary = "no connection strings configured"
		return st
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var reachable, withPSS, withHypo int
	for _, n := range c.Nodes {
		ns := NodeStatus{Name: n.Name, DSN: config.Describe(n.DSN)}

		node, err := s.openNode(ctx, c, n)
		if err != nil {
			ns.Error = friendlyConnectError(err)
			st.Nodes = append(st.Nodes, ns)
			continue
		}

		ns.Reachable = true
		st.Advice = append(st.Advice, node.Info.Warnings...)
		ns.Role = string(node.Info.Role)
		ns.Version = node.Info.ServerVersion
		ns.VersionNum = node.Info.ServerVersionNum
		ns.HasPSS = node.Info.HasExtension("pg_stat_statements")
		ns.PSSLoaded = node.Info.PSSPreloaded()
		ns.HasHypoPG = node.Info.HasExtension("hypopg")
		if age, known := node.Info.StatsAge(); known {
			ns.StatsAgeSec = int64(age.Seconds())
			ns.StatsKnown = true
		}
		node.Close()

		reachable++
		if ns.HasPSS && ns.PSSLoaded {
			withPSS++
		}
		if ns.HasHypoPG {
			withHypo++
		}
		st.Nodes = append(st.Nodes, ns)
	}

	switch {
	case reachable == 0:
		st.State = "error"
		st.Summary = "cannot connect"
	case reachable < len(c.Nodes):
		st.State = "warning"
		st.Summary = fmt.Sprintf("%d of %d nodes reachable", reachable, len(c.Nodes))
	default:
		st.Summary = fmt.Sprintf("%d node%s connected", reachable, plural(reachable))
	}

	// Advice is the difference between an interface that reports a problem
	// and one that helps. The common case by far is a server without
	// pg_stat_statements, where the fix is two commands.
	if reachable > 0 && withPSS == 0 {
		if st.State == "ok" {
			st.State = "warning"
		}
		st.Advice = append(st.Advice,
			"pg_stat_statements is not active, so idxa cannot see which queries run. "+
				"Add it with: ALTER SYSTEM SET shared_preload_libraries = 'pg_stat_statements'; "+
				"restart the server, then: CREATE EXTENSION pg_stat_statements;")
		st.Advice = append(st.Advice,
			"Without it, idxa can still sample running queries, and still reports unused and duplicate indexes.")
	}
	if reachable > 0 && withHypo == 0 {
		st.Advice = append(st.Advice,
			"hypopg is not installed, so recommendations are estimates rather than confirmed by the planner. "+
				"Add it with: CREATE EXTENSION hypopg;")
	}
	for _, n := range st.Nodes {
		if n.Reachable && n.StatsKnown && n.StatsAgeSec < 3600 {
			st.Advice = append(st.Advice, fmt.Sprintf(
				"statistics on %s are only %s old; recommendations become more reliable after about an hour of normal traffic",
				n.Name, humanDuration(time.Duration(n.StatsAgeSec)*time.Second)))
			break
		}
	}
	return st
}

// openNode connects to one node, resolving environment references first.
func (s *Server) openNode(ctx context.Context, c *config.Cluster, n config.Node) (*db.Node, error) {
	dsn, err := config.Resolve(n.DSN)
	if err != nil {
		return nil, err
	}
	name := n.Name
	if name == "" {
		name = config.Describe(n.DSN)
	}
	return db.Open(ctx, name, dsn, db.Options{
		StatementTimeout: 20 * time.Second,
		MaxConns:         2, // the interface never needs more than a couple
	})
}

// withGovernor runs fn against a node while holding its load budget slot.
//
// Background work uses TryAcquire and skips when the budget says wait, so a
// slow server is polled less often rather than queueing work behind itself.
// Work a person asked for waits instead, because dropping it silently would
// look like a broken button.
func (s *Server) withGovernor(ctx context.Context, nodeKey string, background bool, fn func(context.Context) error) error {
	var (
		release func()
		err     error
	)
	if background {
		release, err = s.governor.TryAcquire(ctx, nodeKey)
		if errors.Is(err, load.ErrBusy) {
			return load.ErrBusy
		}
	} else {
		release, err = s.governor.Acquire(ctx, nodeKey)
	}
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// friendlyConnectError turns driver errors into something a person can act
// on, since the raw text is often just an address and a code.
func friendlyConnectError(err error) string {
	msg := err.Error()
	switch {
	case contains(msg, "connection refused"):
		return "connection refused: nothing is listening on that host and port"
	case contains(msg, "no such host"), contains(msg, "server misbehaving"):
		return "host not found: check the hostname"
	case contains(msg, "password authentication failed"):
		return "password authentication failed: check the user and password"
	case contains(msg, "database") && contains(msg, "does not exist"):
		return "that database does not exist on the server"
	case contains(msg, "timeout"), contains(msg, "deadline exceeded"):
		return "timed out: the server did not answer, which usually means a firewall is dropping the connection"
	case contains(msg, "SSL"), contains(msg, "TLS"):
		return "TLS negotiation failed: try adding sslmode=require or sslmode=disable to the connection string"
	case contains(msg, "unset environment variable"):
		return msg
	default:
		return msg
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// humanDuration renders a duration the way a person would say it.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
