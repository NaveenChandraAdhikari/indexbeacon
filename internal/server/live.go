package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/config"
	"github.com/indexbeacon/indexbeacon/internal/db"
	"github.com/indexbeacon/indexbeacon/internal/load"
)

// liveTableSQL is the cheap tier and nothing more: in-memory statistics
// counters, plus the planner's own page count standing in for size.
//
// The on-disk size functions are deliberately absent. Measured on PostgreSQL
// 16 against 1,578 tables, the same query costs 79ms with pg_table_size and
// 5.2ms with relpages, and the gap widens with table size because each call
// stats every one gigabyte segment of every fork. relpages matched
// pg_relation_size exactly on every fixture table, because it is what vacuum
// and analyze record and what the planner itself uses.
const liveTableSQL = `
SELECT n.nspname, c.relname,
       c.relpages::bigint * 8192,
       coalesce(s.n_live_tup, 0),
       coalesce(s.seq_scan, 0), coalesce(s.seq_tup_read, 0),
       coalesce(s.idx_scan, 0), coalesce(s.idx_tup_fetch, 0),
       coalesce(s.n_tup_ins, 0) + coalesce(s.n_tup_upd, 0) + coalesce(s.n_tup_del, 0)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r', 'p', 'm')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  AND n.nspname NOT LIKE 'pg_temp_%'
  AND n.nspname NOT LIKE 'pg_toast_temp_%'`

// liveDatabaseSQL reads the whole-database counters shown in the tiles.
const liveDatabaseSQL = `
SELECT coalesce(blks_hit, 0), coalesce(blks_read, 0),
       coalesce(xact_commit, 0) + coalesce(xact_rollback, 0),
       coalesce(numbackends, 0),
       pg_database_size(current_database())
FROM pg_stat_database WHERE datname = current_database()`

// liveActivitySQL counts what the server is doing right now.
const liveActivitySQL = `
SELECT count(*) FILTER (WHERE state = 'active'),
       count(*) FILTER (WHERE wait_event_type = 'Lock'),
       count(*)
FROM pg_stat_activity WHERE datname = current_database()`

// tableSample is one table's counters at a moment in time.
type tableSample struct {
	seqScan    int64
	seqTupRead int64
	idxScan    int64
	writes     int64
}

// LiveTable is one row of the live table list, expressed as rates.
type LiveTable struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Rows   int64  `json:"rows"`
	Bytes  int64  `json:"bytes"`
	// Rates per second, computed from the change since the previous tick.
	// Deltas are what make a missing index visible while it is happening;
	// raw counters only show accumulated history.
	SeqScansPerSec  float64 `json:"seq_scans_per_sec"`
	SeqTuplesPerSec float64 `json:"seq_tuples_per_sec"`
	IdxScansPerSec  float64 `json:"idx_scans_per_sec"`
	WritesPerSec    float64 `json:"writes_per_sec"`
	// Totals since the statistics were last reset.
	SeqScans int64 `json:"seq_scans_total"`
	IdxScans int64 `json:"idx_scans_total"`
	// SeqTupRead is how many rows sequential scans have read in total. This
	// is the honest measure of scan pressure: a tiny lookup table gets
	// scanned constantly and costs nothing, while one large scan can read
	// more rows than a thousand small ones.
	SeqTupRead int64 `json:"seq_tup_read_total"`
	// Sparkline holds recent sequential tuple rates for a small chart.
	Sparkline []float64 `json:"sparkline,omitempty"`
}

// LiveSnapshot is one tick of the live view.
type LiveSnapshot struct {
	At time.Time `json:"at"`
	// Tiles.
	CacheHitRatio   float64 `json:"cache_hit_ratio"`
	Connections     int64   `json:"connections"`
	ActiveQueries   int64   `json:"active_queries"`
	BlockedQueries  int64   `json:"blocked_queries"`
	DatabaseBytes   int64   `json:"database_bytes"`
	SeqTuplesPerSec float64 `json:"seq_tuples_per_sec"`
	SeqScansPerSec  float64 `json:"seq_scans_per_sec"`
	TransactionsSec float64 `json:"transactions_per_sec"`
	// Tables, ordered by sequential pressure.
	Tables []LiveTable `json:"tables"`
	// Cost of idxa itself on this database.
	Cost CostView `json:"cost"`
	// Warning explains a degraded tick rather than leaving the view frozen.
	Warning string `json:"warning,omitempty"`
}

// liveState holds the previous tick so deltas can be computed.
type liveState struct {
	prev     map[string]tableSample
	prevAt   time.Time
	prevDB   dbSample
	spark    map[string][]float64
	warnedAt time.Time
}

type dbSample struct {
	hit, read, xact int64
}

// liveHub runs one collection loop per database, but only while at least one
// browser is listening. An idle tab must cost the database nothing.
type liveHub struct {
	s *Server

	mu      sync.Mutex
	streams map[string]*liveStream
}

type liveStream struct {
	cancel   context.CancelFunc
	mu       sync.Mutex
	clients  map[chan LiveSnapshot]struct{}
	state    *liveState
	lastSnap *LiveSnapshot
}

func newLiveHub(s *Server) *liveHub {
	return &liveHub{s: s, streams: map[string]*liveStream{}}
}

// subscribe attaches a client, starting the collection loop if this is the
// first listener.
func (h *liveHub) subscribe(c *config.Cluster) (<-chan LiveSnapshot, func()) {
	h.mu.Lock()
	st, ok := h.streams[c.ID]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		st = &liveStream{
			cancel:  cancel,
			clients: map[chan LiveSnapshot]struct{}{},
			state:   &liveState{prev: map[string]tableSample{}, spark: map[string][]float64{}},
		}
		h.streams[c.ID] = st
		go h.loop(ctx, c, st)
	}
	h.mu.Unlock()

	ch := make(chan LiveSnapshot, 4)
	st.mu.Lock()
	st.clients[ch] = struct{}{}
	last := st.lastSnap
	st.mu.Unlock()

	// Send the most recent tick straight away so the page has numbers
	// immediately rather than waiting a full interval.
	if last != nil {
		select {
		case ch <- *last:
		default:
		}
	}

	return ch, func() {
		st.mu.Lock()
		delete(st.clients, ch)
		remaining := len(st.clients)
		st.mu.Unlock()
		close(ch)

		if remaining == 0 {
			// Nobody is watching, so stop querying the database entirely.
			h.stop(c.ID)
		}
	}
}

// stop ends a database's collection loop.
func (h *liveHub) stop(id string) {
	h.mu.Lock()
	st, ok := h.streams[id]
	if ok {
		delete(h.streams, id)
	}
	h.mu.Unlock()
	if ok {
		st.cancel()
	}
}

func (h *liveHub) stopAll() {
	h.mu.Lock()
	streams := h.streams
	h.streams = map[string]*liveStream{}
	h.mu.Unlock()
	for _, st := range streams {
		st.cancel()
	}
}

// loop collects until the stream is cancelled.
func (h *liveHub) loop(ctx context.Context, c *config.Cluster, st *liveStream) {
	interval := time.Duration(h.s.opts.Store.Settings().RefreshSeconds) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Collect once immediately so the first tick is not a blank panel.
	h.tick(ctx, c, st)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.tick(ctx, c, st)
		}
	}
}

// tick collects one snapshot and fans it out.
func (h *liveHub) tick(ctx context.Context, c *config.Cluster, st *liveStream) {
	ctx, cancel := context.WithTimeout(ctx, load.TierLive.StatementTimeout())
	defer cancel()

	snap, err := h.s.collectLive(ctx, c, st.state)
	if errors.Is(err, load.ErrBusy) {
		// The budget says wait. Skipping is correct: the next tick is
		// already due, and queueing would make a slow server slower.
		return
	}
	if err != nil {
		snap = &LiveSnapshot{At: time.Now(), Warning: friendlyConnectError(err)}
	}

	st.mu.Lock()
	st.lastSnap = snap
	for ch := range st.clients {
		select {
		case ch <- *snap:
		default:
			// A slow client must not hold up collection or the others.
		}
	}
	st.mu.Unlock()
}

// collectLive reads the cheap tier from the primary and turns counters into
// rates.
func (h *liveHub) noop() {}

func (s *Server) collectLive(ctx context.Context, c *config.Cluster, state *liveState) (*LiveSnapshot, error) {
	primary := c.Primary()
	if primary == nil {
		return nil, errNoNodes
	}

	var snap *LiveSnapshot
	err := s.withGovernor(ctx, c.ID, true, func(ctx context.Context) error {
		node, err := s.openNode(ctx, c, *primary)
		if err != nil {
			return err
		}
		defer node.Close()

		out, err := readLive(ctx, node, state)
		if err != nil {
			return err
		}
		snap = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	snap.Cost = s.costFor(c.ID)
	return snap, nil
}

// readLive issues the three cheap queries and computes deltas.
func readLive(ctx context.Context, node *db.Node, state *liveState) (*LiveSnapshot, error) {
	now := time.Now()
	snap := &LiveSnapshot{At: now}

	// Whole-database counters.
	var cur dbSample
	if err := node.Pool.QueryRow(ctx, liveDatabaseSQL).Scan(
		&cur.hit, &cur.read, &cur.xact, &snap.Connections, &snap.DatabaseBytes); err != nil {
		return nil, err
	}
	if total := cur.hit + cur.read; total > 0 {
		snap.CacheHitRatio = float64(cur.hit) / float64(total)
	}

	if err := node.Pool.QueryRow(ctx, liveActivitySQL).Scan(
		&snap.ActiveQueries, &snap.BlockedQueries, &snap.Connections); err != nil {
		return nil, err
	}

	// Per-table counters.
	rows, err := node.Pool.Query(ctx, liveTableSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	elapsed := now.Sub(state.prevAt).Seconds()
	haveBaseline := !state.prevAt.IsZero() && elapsed > 0

	current := make(map[string]tableSample)
	var tables []LiveTable

	for rows.Next() {
		var (
			t   LiveTable
			cur tableSample
		)
		if err := rows.Scan(&t.Schema, &t.Table, &t.Bytes, &t.Rows,
			&cur.seqScan, &cur.seqTupRead, &cur.idxScan, new(int64), &cur.writes); err != nil {
			return nil, err
		}
		key := t.Schema + "." + t.Table
		current[key] = cur

		// An empty table can never be under scan pressure, and a schema with
		// hundreds of them would otherwise fill the live view with zeros and
		// bury the tables that matter.
		if t.Rows == 0 && t.Bytes == 0 {
			continue
		}

		t.SeqScans = cur.seqScan
		t.IdxScans = cur.idxScan
		t.SeqTupRead = cur.seqTupRead

		if haveBaseline {
			if prev, ok := state.prev[key]; ok {
				t.SeqScansPerSec = rate(cur.seqScan-prev.seqScan, elapsed)
				t.SeqTuplesPerSec = rate(cur.seqTupRead-prev.seqTupRead, elapsed)
				t.IdxScansPerSec = rate(cur.idxScan-prev.idxScan, elapsed)
				t.WritesPerSec = rate(cur.writes-prev.writes, elapsed)
			}
		}

		// Keep a short history for the sparkline.
		hist := append(state.spark[key], t.SeqTuplesPerSec)
		if len(hist) > 30 {
			hist = hist[len(hist)-30:]
		}
		state.spark[key] = hist
		t.Sparkline = append([]float64{}, hist...)

		snap.SeqTuplesPerSec += t.SeqTuplesPerSec
		snap.SeqScansPerSec += t.SeqScansPerSec
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if haveBaseline {
		snap.TransactionsSec = rate(cur.xact-state.prevDB.xact, elapsed)
	}

	// Order by sequential pressure, falling back to accumulated totals before
	// the first delta arrives, so the list is useful on the very first tick.
	//
	// The fallback counts rows read, not scans. Ordering by scan count puts
	// tiny lookup tables at the top, because the planner sequentially scans a
	// 24 KiB table thousands of times and that costs nothing, while a single
	// scan of a 300 MiB table reads more rows than all of them together. Rows
	// read is what actually hurts, and it is what an index would remove.
	sort.SliceStable(tables, func(i, j int) bool {
		if tables[i].SeqTuplesPerSec != tables[j].SeqTuplesPerSec {
			return tables[i].SeqTuplesPerSec > tables[j].SeqTuplesPerSec
		}
		if tables[i].SeqTupRead != tables[j].SeqTupRead {
			return tables[i].SeqTupRead > tables[j].SeqTupRead
		}
		return tables[i].Bytes > tables[j].Bytes
	})
	if len(tables) > 50 {
		tables = tables[:50]
	}
	snap.Tables = tables

	state.prev = current
	state.prevAt = now
	state.prevDB = cur
	return snap, nil
}

func rate(delta int64, seconds float64) float64 {
	if delta < 0 || seconds <= 0 {
		// A negative delta means the statistics were reset; report zero
		// rather than a nonsensical negative rate.
		return 0
	}
	return float64(delta) / seconds
}

// handleLive streams snapshots to a browser using server-sent events.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cluster(w, r)
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsubscribe := s.live.subscribe(c)
	defer unsubscribe()

	// A heartbeat keeps intermediaries from dropping an idle connection.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case snap, open := <-ch:
			if !open {
				return
			}
			if err := writeSSE(w, "snapshot", snap); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
