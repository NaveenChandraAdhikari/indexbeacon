package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Role of a node in a replication topology.
type Role string

const (
	RolePrimary Role = "primary"
	RoleReplica Role = "replica"
)

// Extension describes availability of an optional extension on a node.
type Extension struct {
	Name             string `json:"name"`
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version,omitempty"`
	DefaultVersion   string `json:"default_version,omitempty"`
}

// Info is everything idxa learns about a node at connect time.
type Info struct {
	Name                   string      `json:"name"`
	DSN                    string      `json:"dsn"` // redacted
	Role                   Role        `json:"role"`
	ServerVersionNum       int         `json:"server_version_num"`
	ServerVersion          string      `json:"server_version"`
	Database               string      `json:"database"`
	User                   string      `json:"user"`
	Superuser              bool        `json:"superuser"`
	ReadAllStats           bool        `json:"pg_read_all_stats"`
	Extensions             []Extension `json:"extensions"`
	SharedPreloadLibraries string      `json:"shared_preload_libraries"`
	// Warnings records what could not be read because the connected role
	// lacks the privilege, so the interface can explain a partial picture
	// rather than presenting it as a failure.
	Warnings               []string   `json:"warnings,omitempty"`
	TrackActivityQuerySize int        `json:"track_activity_query_size"`
	StatsReset             *time.Time `json:"stats_reset,omitempty"`
	PSSStatsReset          *time.Time `json:"pg_stat_statements_reset,omitempty"`
	ServerNow              time.Time  `json:"server_now"`
}

// HasExtension reports whether name is installed on the node.
func (i *Info) HasExtension(name string) bool {
	for _, e := range i.Extensions {
		if e.Name == name && e.Installed {
			return true
		}
	}
	return false
}

// PSSPreloaded reports whether pg_stat_statements is in shared_preload_libraries.
func (i *Info) PSSPreloaded() bool {
	for _, lib := range strings.Split(i.SharedPreloadLibraries, ",") {
		if strings.TrimSpace(lib) == "pg_stat_statements" {
			return true
		}
	}
	return false
}

// StatsAge returns the age of pg_stat_database counters (the conservative
// window for interpreting idx_scan/seq_scan) and whether it is known.
func (i *Info) StatsAge() (time.Duration, bool) {
	if i.StatsReset == nil {
		return 0, false
	}
	return i.ServerNow.Sub(*i.StatsReset), true
}

// Node is a connected Postgres server plus what idxa learned about it.
type Node struct {
	Info Info
	Pool *pgxpool.Pool
}

// Close releases the pool.
func (n *Node) Close() {
	if n != nil && n.Pool != nil {
		n.Pool.Close()
	}
}

// Open connects to dsn and inspects the node. name is a human label (may be
// empty; the redacted DSN is used instead).
func Open(ctx context.Context, name, dsn string, opts Options) (*Node, error) {
	pool, err := Connect(ctx, dsn, opts)
	if err != nil {
		return nil, err
	}
	info, err := Inspect(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	info.DSN = RedactDSN(dsn)
	if name == "" {
		name = info.DSN
	}
	info.Name = name
	return &Node{Info: info, Pool: pool}, nil
}

// Inspect fills an Info by querying the server.
func Inspect(ctx context.Context, pool *pgxpool.Pool) (Info, error) {
	var info Info

	if err := pool.QueryRow(ctx, SQLServerVersion).Scan(&info.ServerVersionNum, &info.ServerVersion); err != nil {
		return info, fmt.Errorf("server version: %w", err)
	}
	var inRecovery bool
	if err := pool.QueryRow(ctx, SQLIsInRecovery).Scan(&inRecovery); err != nil {
		return info, fmt.Errorf("pg_is_in_recovery: %w", err)
	}
	info.Role = RolePrimary
	if inRecovery {
		info.Role = RoleReplica
	}
	if err := pool.QueryRow(ctx, SQLCurrentRole).Scan(&info.User, &info.Superuser, &info.ReadAllStats); err != nil {
		return info, fmt.Errorf("current role: %w", err)
	}
	if err := pool.QueryRow(ctx, SQLCurrentDatabase).Scan(&info.Database); err != nil {
		return info, fmt.Errorf("current database: %w", err)
	}
	rows, err := pool.Query(ctx, SQLExtensions)
	if err != nil {
		return info, fmt.Errorf("extensions: %w", err)
	}
	for rows.Next() {
		var e Extension
		if err := rows.Scan(&e.Name, &e.Installed, &e.InstalledVersion, &e.DefaultVersion); err != nil {
			rows.Close()
			return info, fmt.Errorf("extensions: %w", err)
		}
		info.Extensions = append(info.Extensions, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return info, fmt.Errorf("extensions: %w", err)
	}
	// These two settings are useful but not essential, and reading them
	// requires privileges an ordinary application role often does not have.
	// A role that can read table statistics but not server settings is
	// completely normal in production, and refusing to connect at all would
	// turn a small gap into "cannot connect", which is both wrong and
	// unhelpful. Note what is missing and carry on.
	if err := pool.QueryRow(ctx, SQLSharedPreloadLibraries).Scan(&info.SharedPreloadLibraries); err != nil {
		info.Warnings = append(info.Warnings,
			"this role cannot read shared_preload_libraries, so idxa cannot tell whether "+
				"pg_stat_statements is loaded; grant pg_read_all_settings to check")
	}
	if err := pool.QueryRow(ctx, SQLTrackActivityQuerySize).Scan(&info.TrackActivityQuerySize); err != nil {
		info.Warnings = append(info.Warnings,
			"this role cannot read track_activity_query_size, so idxa cannot warn when "+
				"sampled statements are being truncated")
	}
	if err := pool.QueryRow(ctx, SQLStatsReset).Scan(&info.StatsReset); err != nil {
		info.Warnings = append(info.Warnings,
			"this role cannot read pg_stat_database, so the age of the statistics is unknown")
	}
	if err := pool.QueryRow(ctx, SQLNow).Scan(&info.ServerNow); err != nil {
		return info, fmt.Errorf("now(): %w", err)
	}
	if info.HasExtension("pg_stat_statements") && info.PSSPreloaded() {
		var reset *time.Time
		// pg_stat_statements_info exists from PG14; ignore failures.
		if err := pool.QueryRow(ctx, SQLPSSStatsReset).Scan(&reset); err == nil {
			info.PSSStatsReset = reset
		}
	}
	return info, nil
}
