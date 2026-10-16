-- Runs once at cluster initialisation (primary only; replicas inherit via
-- pg_basebackup). Creates the extensions idxa can take advantage of.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE EXTENSION IF NOT EXISTS hypopg;
