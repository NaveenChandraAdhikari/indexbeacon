package db

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplicationName is reported in pg_stat_activity for every idxa session.
const ApplicationName = "idxa"

// Options control how idxa connects to a node.
type Options struct {
	// StatementTimeout bounds every statement idxa issues. Zero means the
	// server default.
	StatementTimeout time.Duration
	// MaxConns caps the pool size. Defaults to 4.
	MaxConns int32
}

// Connect opens a pool to dsn with idxa's read-only session settings:
//
//   - default_transaction_read_only = on
//   - statement_timeout = opts.StatementTimeout
//   - application_name = idxa
//
// The settings are sent as startup parameters and then re-asserted with an
// explicit SET on every new connection so that a DSN that already carries
// options cannot override them.
func Connect(ctx context.Context, dsn string, opts Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = 4
	}
	cfg.MaxConns = opts.MaxConns
	cfg.MinConns = 0

	rp := cfg.ConnConfig.RuntimeParams
	if rp == nil {
		rp = map[string]string{}
		cfg.ConnConfig.RuntimeParams = rp
	}
	rp["application_name"] = ApplicationName
	rp["default_transaction_read_only"] = "on"
	timeoutMS := int64(0)
	if opts.StatementTimeout > 0 {
		timeoutMS = opts.StatementTimeout.Milliseconds()
		rp["statement_timeout"] = strconv.FormatInt(timeoutMS, 10)
	}

	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, SQLSetReadOnly); err != nil {
			return fmt.Errorf("set read-only: %w", err)
		}
		if timeoutMS > 0 {
			if _, err := conn.Exec(ctx, SQLSetStatementTimeout+strconv.FormatInt(timeoutMS, 10)); err != nil {
				return fmt.Errorf("set statement_timeout: %w", err)
			}
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// RedactDSN returns dsn with any password removed, for display.
func RedactDSN(dsn string) string {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "<invalid dsn>"
	}
	host := cfg.Host
	if cfg.Port != 0 {
		host = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	}
	if cfg.User == "" {
		return fmt.Sprintf("%s/%s", host, cfg.Database)
	}
	return fmt.Sprintf("%s@%s/%s", cfg.User, host, cfg.Database)
}
