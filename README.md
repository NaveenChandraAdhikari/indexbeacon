<p align="center">
  <img src="docs/hero.svg" alt="idxa: find the indexes your queries are missing" width="880">
</p>

<p align="center">
  <a href="LICENSE"><img alt="MIT licence" src="https://img.shields.io/badge/licence-MIT-7ec79c?style=flat-square"></a>
  <a href="https://hub.docker.com/r/indexbeacon/indexbeacon"><img alt="Docker image" src="https://img.shields.io/badge/docker-indexbeacon%2Findexbeacon-5aa8c8?style=flat-square"></a>
  <img alt="PostgreSQL 12 to 17" src="https://img.shields.io/badge/postgresql-12%20to%2017-a2a69c?style=flat-square">
  <img alt="Go" src="https://img.shields.io/badge/go-1.26-a2a69c?style=flat-square">
</p>

**idxa** reads your PostgreSQL query statistics, works out which indexes would
help, and then proves each suggestion by asking the planner what it would do
if the index existed. It also finds indexes nobody uses and indexes that
duplicate each other.

It only ever reads. idxa never creates, drops or alters anything, and a test
in this repository fails the build if that ever stops being true.

<p align="center">
  <img src="docs/demo.svg" alt="idxa analyze recommending three indexes, each confirmed by the planner" width="880">
</p>

## Why another index advisor

**It is safe to point at production.** Every statement idxa issues goes
through a duty-cycle limiter that holds it to about one percent of any one
server's time. An expensive collection buys a proportionally longer pause, so
a slow server is polled less often without anyone tuning anything. The live
view never calls the on-disk size functions, which cost fifteen times more
than the same information taken from the planner's own page counts.

**It proves its recommendations.** A cost model can be confident and wrong.
Where `hypopg` is installed, every candidate is created hypothetically and the
affected queries are re-planned. Anything the planner declines to use is
demoted and labelled, however good its estimate looked.

**It understands replicas.** Statistics counters are per instance. Give idxa
your primary and its replicas and it sums them, so an index used only on one
replica is not reported as unused, and a table scanned on three replicas is
correctly seen as under three times the pressure.

**It works without extensions.** `pg_stat_statements` is used when present.
Where it is not, `idxa sample` watches `pg_stat_activity` instead. Without
`hypopg` you get the same recommendations as cost-model estimates, clearly
marked as unconfirmed.

**It explains itself.** Every recommendation lists the queries it would help,
how selective it expects to be, what the planner said, and what the index
would cost to maintain on every write.

## Install

The quickest way is the container, which needs nothing else installed:

```
docker run -d --name idxa -p 127.0.0.1:8777:8777 \
  -v idxa-data:/var/lib/idxa -e IDXA_TOKEN=pick-something \
  indexbeacon/indexbeacon:latest
```

Then open `http://127.0.0.1:8777/?token=pick-something`.

This repository has a script that does the same thing and remembers the
token for you:

```
./scripts/run.sh          # pull, start, and print the URL
./scripts/run.sh --logs   # follow the output
./scripts/run.sh --stop   # stop it, keeping your configured databases
```

The volume matters. idxa keeps the databases you configure and the analyses it
has run on disk, so without it a restart loses everything you set up.

To build from source instead:

```
go install .
```

It is a single static binary with no runtime to install. The SQL parser is
the real PostgreSQL grammar via `pg_query`, so the build needs a C compiler.

## Use

```
idxa check      --dsn postgres://user@host/db   # can idxa work here?
idxa analyze    --dsn postgres://user@host/db   # what indexes are missing?
idxa unused     --dsn postgres://user@host/db   # what can I drop?
idxa duplicates --dsn postgres://user@host/db   # what is redundant?
idxa sample     --dsn postgres://user@host/db   # record the workload without pg_stat_statements
idxa serve                                      # the web interface
```

Pass `--dsn` once per node to analyse a primary and its replicas together:

```
idxa analyze \
  --dsn postgres://user@primary/app \
  --dsn postgres://user@replica-1/app \
  --dsn postgres://user@replica-2/app
```

Output is a table by default. `--format json`, `--format markdown` and
`--format sql` are also available; the SQL form emits the `CREATE INDEX`
statements with any drops commented out, so running the file by accident
cannot remove anything.

### Without pg_stat_statements

That extension needs a line in `postgresql.conf` and a restart, which on a
managed service can mean a maintenance window. Where it is missing, record the
workload by watching what is running:

```
idxa sample  --dsn ... --duration 10m --out workload.json.gz
idxa analyze --dsn ... --workload workload.json.gz
```

A query that runs constantly appears in most samples; one that runs rarely
appears in few. The counts are estimates rather than exact call counts, and
every recommendation built from them is marked with lower confidence.

### Checking a recommendation yourself

```
idxa explain-candidate --dsn ... \
  --index "shop.orders(customer_id, created_at DESC)" \
  --query "SELECT id FROM shop.orders WHERE customer_id = 42 ORDER BY created_at DESC LIMIT 20"
```

It creates the index hypothetically and prints the plan with and without it,
which answers the only question that matters: would the planner use this?

### The web interface

```
idxa serve --open
```

Add as many databases as you like, watch sequential-scan pressure as it
happens, and run an analysis when something looks wrong. Analyses can also run
on a schedule, and every run is kept so you can see what changed since the
last one.

It listens on loopback only, because the configuration holds database
credentials and there are no user accounts. Exposing it on a network address
requires `--allow-remote`, which turns on token authentication.

## How it decides

<p align="center">
  <img src="docs/pipeline.svg" alt="Read statistics, parse queries, propose indexes, score them, confirm each with the planner" width="880">
</p>

1. Read table, index and column statistics, plus the workload.
2. Parse each statement with the real PostgreSQL grammar, extracting the
   columns used in `WHERE`, joins and `ORDER BY`.
3. Propose indexes: equality columns first, ordered most selective first, then
   at most one range column, then the sort columns. Containment on `jsonb`
   and arrays, full-text search and unanchored `LIKE` get GIN; very large,
   well-correlated columns get a BRIN alternative.
4. Merge the proposals, discarding any that an existing index already covers
   or that another proposal subsumes.
5. Score each against the planner's own cost constants, weighing the saving
   against what the index costs to maintain on every write. The saving is
   capped by the sequential scans the table has actually seen.
6. Where `hypopg` is available, create each one hypothetically and re-plan the
   affected queries. Anything the planner declines to use is demoted.

## What the database needs

| | Needed for |
|---|---|
| A role that can read statistics | Everything. `pg_read_all_stats` is enough; superuser is not required |
| `pg_stat_statements` | Seeing which queries run, and therefore recommending indexes |
| `hypopg` | Confirming recommendations against the planner |

Without `pg_stat_statements`, unused and duplicate index detection still work,
and `idxa sample` can build a workload picture by watching active queries.

To install them:

```sql
-- in postgresql.conf, then restart
shared_preload_libraries = 'pg_stat_statements'

CREATE EXTENSION pg_stat_statements;
CREATE EXTENSION hypopg;
```

## Keeping credentials out of the configuration

The web interface stores connection strings in
`~/.config/idxa/config.json`, written owner-only. To keep the password out of
that file, reference an environment variable:

```
postgres://app:${PGPASSWORD}@db.internal:5432/orders
```

Where the file must carry its own passwords, such as on a volume that gets
snapshotted, set `IDXA_CONFIG_KEY` and idxa will encrypt it at rest with
AES-256-GCM. That is the second line of defence; the first is not writing the
secret down at all.

## Running it on Kubernetes

```
./scripts/deploy.sh --image indexbeacon/indexbeacon:latest
```

It prints the cluster and namespace it is about to change and waits for you to
confirm, then applies a single replica with a persistent volume and waits for
the rollout. Add `--dry-run` to see the manifests without applying anything.

There is deliberately no Service. idxa listens on the pod's loopback address,
so the only way in is a port-forward, and that already requires permission on
the namespace:

```
kubectl -n idxa port-forward deploy/idxa 8777:8777
```

Put database passwords in the secret rather than in connection strings, so the
volume that gets snapshotted holds nothing sensitive:

```
kubectl -n idxa patch secret idxa --type merge \
  -p '{"stringData":{"PGPASSWORD_PROD":"..."}}'
```

then use `postgres://user:${PGPASSWORD_PROD}@host:5432/dbname`.

## Trying it out

Against the [pagila](https://github.com/devrimgunduz/pagila) sample database:

```
createdb pagila && psql pagila -f pagila-schema.sql -f pagila-data.sql
psql pagila -c 'DROP INDEX idx_fk_customer_id'
psql pagila -c 'SELECT count(*) FROM rental WHERE customer_id = 42' # a few times
idxa analyze --dsn postgres://localhost/pagila --min-rows 1000
```

The repository also ships a fixture: a ten table shop schema, a data
generator and a workload runner.

```
docker compose -f docker/compose.yml up -d --wait
export DSN=postgres://idxa:idxa@localhost:5499/idxa
psql "$DSN" -f testdata/ecommerce/schema.sql
cd testdata/ecommerce
go run ./gen -dsn "$DSN" -scale 1
go run ./workload -dsn "$DSN" -duration 30s
idxa analyze --dsn "$DSN"
```

The schema deliberately omits three indexes. idxa should recommend exactly
those three and nothing else.

## Development

```
make build   # build the binary
make test    # unit tests
make docker  # build the container image
```

The web interface has its own notes and a browser-driven test in
[web/README.md](web/README.md).

## Licence

[MIT](LICENSE).

## Project origin

The PostgreSQL index-advisor implementation, architecture, tests, and
e-commerce fixture are based on the upstream project. This repository includes
branding, user-facing, configuration, and deployment-name changes while
retaining the upstream package and import structure.

The upstream copyright and MIT license notice are retained in `LICENSE`. See
`NOTICE.md` for additional attribution details.
