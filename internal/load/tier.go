package load

import "time"

// Tier classifies how expensive a piece of collection is, which decides how
// often it may run. The costs below were measured against PostgreSQL 16 on a
// schema of 1,578 tables and 6,194 indexes, all pages cached:
//
//	pg_stat_activity                                    1.3 ms
//	pg_stat_user_tables counters only                   6.6 ms
//	pg_class + counters, relpages for size              5.2 ms
//	pg_index catalog only                               5.5 ms
//	pg_stat_statements, top 200 by total time           5.6 ms
//	pg_attribute joined to pg_stats, every column      15.8 ms
//	pg_class + pg_table_size() + pg_indexes_size()     79.0 ms
//	pg_index + pg_relation_size() + pg_get_indexdef()  74.6 ms
//
// The lesson is that the on-disk size functions dominate everything else, by
// roughly 15x. They are not planner estimates; each one stats every segment
// file of every fork, so their cost grows with the number of relations and
// with the size of each relation (a 500GB table is 500 one-gigabyte segments,
// and therefore 500 stat calls for a single pg_relation_size).
//
// The cheap substitute is exact: pg_class.relpages multiplied by the 8KB page
// size matched pg_relation_size to the byte on every table in the test schema,
// because relpages is what vacuum and analyze record and what the planner
// itself uses. It goes stale only between analyzes, and never by enough to
// change a ranking. So the live loop uses relpages, and true sizes are read on
// the slow tier or on demand.
type Tier int

const (
	// TierLive is safe to run every few seconds: in-memory statistics
	// counters and nothing else. No size functions, no pg_stats, no
	// pg_get_indexdef, no unbounded scans of pg_stat_statements.
	TierLive Tier = iota

	// TierWarm runs about once a minute: pg_stat_statements bounded by
	// LIMIT, and per-index usage counters.
	TierWarm

	// TierCold runs a few times an hour, or when a user asks for it: the
	// on-disk size functions, index definitions, and column statistics.
	// Everything here is expensive enough that it must never sit inside a
	// polling loop.
	TierCold
)

// String implements fmt.Stringer.
func (t Tier) String() string {
	switch t {
	case TierLive:
		return "live"
	case TierWarm:
		return "warm"
	case TierCold:
		return "cold"
	default:
		return "unknown"
	}
}

// DefaultInterval is how often a tier should refresh when nothing forces it
// to back off. The governor may stretch these further on a slow server.
func (t Tier) DefaultInterval() time.Duration {
	switch t {
	case TierLive:
		return 5 * time.Second
	case TierWarm:
		return 60 * time.Second
	case TierCold:
		return 15 * time.Minute
	default:
		return time.Minute
	}
}

// StatementTimeout is the per-statement ceiling for a tier. A live query that
// takes longer than this is not worth waiting for, because the next tick is
// already due; a cold query is allowed to be slow because it runs rarely.
func (t Tier) StatementTimeout() time.Duration {
	switch t {
	case TierLive:
		return 3 * time.Second
	case TierWarm:
		return 10 * time.Second
	case TierCold:
		return 60 * time.Second
	default:
		return 30 * time.Second
	}
}
