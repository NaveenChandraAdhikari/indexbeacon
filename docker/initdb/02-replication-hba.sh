#!/bin/bash
# Allow streaming-replication connections from other containers on the
# compose network (used by compose.replica.yml). Harmless for single-node.
set -e
echo "host replication all 0.0.0.0/0 scram-sha-256" >> "$PGDATA/pg_hba.conf"
echo "host replication all ::/0 scram-sha-256" >> "$PGDATA/pg_hba.conf"
