#!/bin/bash
# Make the server read /etc/postgresql/conf.d/*.conf (the bundled config sets
# shared_preload_libraries etc.). Runs once at initdb; the entrypoint restarts
# postgres afterwards so the preload takes effect before 01-extensions.sql.
set -e
echo "include_dir = '/etc/postgresql/conf.d'" >> "$PGDATA/postgresql.conf"
