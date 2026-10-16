# idxa test image: PostgreSQL 16 + hypopg + pg_stat_statements (preloaded).
#
# Build:  docker build -t idxa/postgres:16 docker/
# Run:    docker compose -f docker/compose.yml up -d
FROM postgres:16

# The official image already has the PGDG apt repository configured, so
# hypopg installs straight from apt.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends postgresql-16-hypopg; \
    rm -rf /var/lib/apt/lists/*

# Extra server settings (shared_preload_libraries etc.). The init script
# 00-include-conf.sh appends `include_dir = '/etc/postgresql/conf.d'` to
# postgresql.conf at initdb time; 01-extensions.sql then creates the
# extensions in the default database.
COPY postgresql.conf.d/ /etc/postgresql/conf.d/
COPY initdb/ /docker-entrypoint-initdb.d/

CMD ["postgres"]
