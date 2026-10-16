// Package db owns every SQL statement idxa sends to a server.
//
// All statements live in this file so they can be audited in one place. idxa
// is strictly read-only: every constant below must begin with SELECT, WITH,
// EXPLAIN, SET or SHOW, or be one of the whitelisted hypopg calls (which only
// create session-local hypothetical indexes). A static test enforces this.
package db

// Session setup. These run once per pooled connection.
const (
	SQLSetReadOnly = `SET default_transaction_read_only = on`
	// SQLSetStatementTimeout is completed with an integer millisecond value.
	SQLSetStatementTimeout = `SET statement_timeout = `
)

// Node identity and health.
const (
	SQLServerVersion = `SELECT current_setting('server_version_num')::int, version()`

	SQLIsInRecovery = `SELECT pg_is_in_recovery()`

	SQLCurrentRole = `
SELECT
  current_user,
  rolsuper,
  pg_has_role(current_user, 'pg_read_all_stats', 'MEMBER')
FROM pg_roles
WHERE rolname = current_user`

	SQLCurrentDatabase = `SELECT current_database()`

	SQLExtensions = `
SELECT name, installed_version IS NOT NULL, coalesce(installed_version, ''), coalesce(default_version, '')
FROM pg_available_extensions
WHERE name IN ('pg_stat_statements', 'hypopg', 'pg_qualstats')
ORDER BY name`

	SQLSharedPreloadLibraries = `SHOW shared_preload_libraries`

	// track_activity_query_size is a memory-unit GUC: current_setting() renders it
	// as e.g. '4kB', so read the raw integer (in the GUC's own unit) from
	// pg_settings and normalise it to bytes via the unit column.
	SQLTrackActivityQuerySize = `SELECT (setting::bigint * CASE unit
                WHEN 'B' THEN 1 WHEN 'kB' THEN 1024
                WHEN 'MB' THEN 1024*1024 WHEN 'GB' THEN 1024*1024*1024
                ELSE 1 END)::int
        FROM pg_settings WHERE name = 'track_activity_query_size'`

	SQLStatsReset = `SELECT stats_reset FROM pg_stat_database WHERE datname = current_database()`

	SQLPSSStatsReset = `SELECT stats_reset FROM pg_stat_statements_info`

	SQLNow = `SELECT now()`
)

// Catalog and statistics collectors.
const (
	// SQLTables returns one row per ordinary/partitioned table with usage
	// counters and size. relpages/reltuples come from the planner estimates.
	SQLTables = `
SELECT
  n.nspname,
  c.relname,
  c.oid,
  c.relkind,
  c.relpages,
  c.reltuples,
  coalesce(s.n_live_tup, 0),
  coalesce(s.n_dead_tup, 0),
  coalesce(s.seq_scan, 0),
  coalesce(s.seq_tup_read, 0),
  coalesce(s.idx_scan, 0),
  coalesce(s.idx_tup_fetch, 0),
  coalesce(s.n_tup_ins, 0),
  coalesce(s.n_tup_upd, 0),
  coalesce(s.n_tup_del, 0),
  coalesce(s.n_tup_hot_upd, 0),
  pg_table_size(c.oid),
  pg_indexes_size(c.oid)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r', 'p', 'm')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  AND n.nspname NOT LIKE 'pg_temp_%'
  AND n.nspname NOT LIKE 'pg_toast_temp_%'
ORDER BY n.nspname, c.relname`

	// SQLIndexes returns one row per index with its definition, key columns
	// (expressions rendered by pg_get_indexdef), usage counters and size.
	SQLIndexes = `
SELECT
  n.nspname,
  t.relname,
  t.oid,
  i.relname,
  i.oid,
  am.amname,
  x.indisunique,
  x.indisprimary,
  x.indisvalid,
  x.indnkeyatts,
  x.indnatts,
  pg_get_indexdef(i.oid),
  coalesce(pg_get_expr(x.indpred, x.indrelid), ''),
  ARRAY(
    SELECT pg_get_indexdef(i.oid, k, true)
    FROM generate_series(1, x.indnatts) k
  ),
  x.indoption::int2[],
  x.indclass::oid[],
  x.indcollation::oid[],
  EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.oid),
  coalesce(s.idx_scan, 0),
  coalesce(s.idx_tup_read, 0),
  coalesce(s.idx_tup_fetch, 0),
  pg_relation_size(i.oid)
FROM pg_index x
JOIN pg_class i ON i.oid = x.indexrelid
JOIN pg_class t ON t.oid = x.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_am am ON am.oid = i.relam
LEFT JOIN pg_stat_user_indexes s ON s.indexrelid = i.oid
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  AND n.nspname NOT LIKE 'pg_temp_%'
  AND n.nspname NOT LIKE 'pg_toast_temp_%'
ORDER BY n.nspname, t.relname, i.relname`

	// SQLColumns returns column metadata joined with planner statistics from
	// pg_stats. Tables without ANALYZE data still appear with NULL stats.
	SQLColumns = `
SELECT
  n.nspname,
  c.relname,
  a.attnum,
  a.attname,
  format_type(a.atttypid, a.atttypmod),
  a.atttypid,
  a.attnotnull,
  s.null_frac,
  s.n_distinct,
  s.correlation,
  s.avg_width,
  coalesce(s.most_common_freqs[1], 0)
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stats s ON s.schemaname = n.nspname AND s.tablename = c.relname AND s.attname = a.attname
WHERE c.relkind IN ('r', 'p', 'm')
  AND a.attnum > 0
  AND NOT a.attisdropped
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  AND n.nspname NOT LIKE 'pg_temp_%'
  AND n.nspname NOT LIKE 'pg_toast_temp_%'
ORDER BY n.nspname, c.relname, a.attnum`

	// SQLStatements reads pg_stat_statements for the current database only.
	// Column names are stable across PG13+ (total_exec_time / mean_exec_time).
	SQLStatements = `
SELECT
  s.queryid,
  s.query,
  s.calls,
  s.total_exec_time,
  s.mean_exec_time,
  s.rows,
  s.shared_blks_hit,
  s.shared_blks_read
FROM pg_stat_statements s
JOIN pg_database d ON d.oid = s.dbid
WHERE d.datname = current_database()
  AND s.calls > 0
ORDER BY s.total_exec_time DESC
LIMIT $1`

	// SQLActivity samples currently executing statements.
	//
	// idxa excludes its own backend and, by application_name, any other idxa
	// session, so a sampler never records itself and mistake its own
	// collection for the workload. How long each statement has been running
	// is returned too, which is what lets sample counts be turned into
	// rough call counts.
	SQLActivity = `
SELECT
  coalesce(query_id, 0),
  query,
  coalesce(extract(epoch FROM (clock_timestamp() - query_start)) * 1000, 0)::bigint
FROM pg_stat_activity
WHERE datname = current_database()
  AND pid <> pg_backend_pid()
  AND state = 'active'
  AND backend_type = 'client backend'
  AND coalesce(application_name, '') <> 'idxa'
  AND query <> ''`
)

// Hypothetical-index validation (hypopg). These are session-local and never
// touch on-disk state. They are the only non SELECT/EXPLAIN calls allowed.
const (
	SQLHypoCreate         = `SELECT indexrelid, indexname FROM hypopg_create_index($1)`
	SQLHypoReset          = `SELECT hypopg_reset()`
	SQLHypoList           = `SELECT indexrelid, indexname FROM hypopg_list_indexes()`
	SQLHypoRelationSize   = `SELECT hypopg_relation_size($1)`
	SQLExplainJSON        = `EXPLAIN (FORMAT JSON) `
	SQLExplainGenericJSON = `EXPLAIN (FORMAT JSON, GENERIC_PLAN) `
)
