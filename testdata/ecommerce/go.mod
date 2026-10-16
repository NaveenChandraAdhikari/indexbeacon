// Fixture tooling lives in its own module because Go ignores directories
// named "testdata" for package patterns, so `go mod tidy` in the repo root
// would not track these dependencies.
//
//   cd testdata/ecommerce && go run ./gen -dsn "$DSN" -scale 1
//   cd testdata/ecommerce && go run ./workload -dsn "$DSN" -duration 30s
module github.com/indexbeacon/indexbeacon/testdata/ecommerce

go 1.26

require (
	github.com/jackc/pgx/v5 v5.10.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
