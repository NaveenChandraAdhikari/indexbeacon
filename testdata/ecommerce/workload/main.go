// Command workload replays the weighted query mix in queries.yaml against a
// database loaded by testdata/ecommerce/gen, so that pg_stat_statements and
// pg_stat_user_tables accumulate realistic counters for the idxa e2e tests.
//
//	cd testdata/ecommerce && go run ./workload -dsn postgres://idxa:idxa@localhost:5499/idxa -duration 30s -concurrency 8
//
// Parameter values are sampled from the database at start-up (Zipf-ish
// customers, existing SKUs/order ids/tracking numbers) so the plans and
// selectivities resemble a real application.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

type query struct {
	Name   string   `yaml:"name"`
	Weight int      `yaml:"weight"`
	Params []string `yaml:"params"`
	SQL    string   `yaml:"sql"`
}

type mix struct {
	Queries []query `yaml:"queries"`
}

// sample holds pools of realistic parameter values pulled from the database.
type sample struct {
	customerIDs []int64 // weighted towards heavy customers (taken from orders)
	productIDs  []int64
	orderIDs    []int64
	skus        []string
	eventTypes  []string
	trackingNos []string
	categoryIDs []int32
	minTime     time.Time
	maxTime     time.Time
}

func main() {
	dsn := flag.String("dsn", os.Getenv("IDXA_DSN"), "PostgreSQL DSN (or IDXA_DSN)")
	duration := flag.Duration("duration", 30*time.Second, "how long to run")
	concurrency := flag.Int("concurrency", runtime.NumCPU(), "number of concurrent workers")
	queriesPath := flag.String("queries", defaultQueriesPath(), "path to queries.yaml")
	seed := flag.Uint64("seed", 7, "random seed")
	flag.Parse()
	if *dsn == "" {
		log.Fatal("-dsn is required")
	}

	raw, err := os.ReadFile(*queriesPath)
	if err != nil {
		log.Fatalf("read %s: %v", *queriesPath, err)
	}
	var m mix
	if err := yaml.Unmarshal(raw, &m); err != nil {
		log.Fatalf("parse %s: %v", *queriesPath, err)
	}
	if len(m.Queries) == 0 {
		log.Fatal("no queries in mix")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(*dsn)
	if err != nil {
		log.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = int32(*concurrency) + 1
	cfg.ConnConfig.RuntimeParams["application_name"] = "idxa-workload"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	s, err := loadSample(context.Background(), pool)
	if err != nil {
		log.Fatalf("sample parameters: %v", err)
	}
	log.Printf("sampled %d customers, %d products, %d orders, %d skus, %d tracking numbers",
		len(s.customerIDs), len(s.productIDs), len(s.orderIDs), len(s.skus), len(s.trackingNos))

	// cumulative weights for weighted pick
	cum := make([]int, len(m.Queries))
	total := 0
	for i, q := range m.Queries {
		total += q.Weight
		cum[i] = total
	}
	pick := func(r *rand.Rand) *query {
		n := r.IntN(total)
		i := sort.SearchInts(cum, n+1)
		return &m.Queries[i]
	}

	counts := make([]atomic.Int64, len(m.Queries))
	var errs atomic.Int64
	var wg sync.WaitGroup
	log.Printf("running %d workers for %s", *concurrency, *duration)
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(*seed, uint64(w)+1))
			for ctx.Err() == nil {
				q := pick(r)
				args := s.args(r, q.Params)
				rows, err := pool.Query(ctx, q.SQL, args...)
				if err != nil {
					if ctx.Err() == nil {
						errs.Add(1)
						log.Printf("%s: %v", q.Name, err)
					}
					continue
				}
				for rows.Next() {
				}
				rows.Close()
				if rows.Err() == nil {
					counts[idx(&m, q)].Add(1)
				} else if ctx.Err() == nil {
					errs.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()

	var sum int64
	for i, q := range m.Queries {
		c := counts[i].Load()
		sum += c
		fmt.Printf("%-28s %8d\n", q.Name, c)
	}
	fmt.Printf("%-28s %8d  (%.0f qps, %d errors)\n", "total", sum, float64(sum)/duration.Seconds(), errs.Load())
}

func idx(m *mix, q *query) int {
	for i := range m.Queries {
		if &m.Queries[i] == q {
			return i
		}
	}
	return 0
}

func defaultQueriesPath() string {
	if _, err := os.Stat("queries.yaml"); err == nil {
		return "queries.yaml"
	}
	return filepath.Join("testdata", "ecommerce", "queries.yaml")
}

func loadSample(ctx context.Context, pool *pgxpool.Pool) (*sample, error) {
	s := &sample{}
	// customers weighted by activity: take customer_id from a random slice of orders
	if err := collect(ctx, pool, "SELECT customer_id FROM shop.orders TABLESAMPLE SYSTEM (5) LIMIT 5000", &s.customerIDs); err != nil {
		return nil, err
	}
	if err := collect(ctx, pool, "SELECT id FROM shop.products TABLESAMPLE SYSTEM (20) LIMIT 2000", &s.productIDs); err != nil {
		return nil, err
	}
	if err := collect(ctx, pool, "SELECT id FROM shop.orders TABLESAMPLE SYSTEM (5) LIMIT 5000", &s.orderIDs); err != nil {
		return nil, err
	}
	if err := collect(ctx, pool, "SELECT sku FROM shop.products TABLESAMPLE SYSTEM (20) LIMIT 2000", &s.skus); err != nil {
		return nil, err
	}
	if err := collect(ctx, pool, "SELECT DISTINCT event_type FROM shop.events TABLESAMPLE SYSTEM (1)", &s.eventTypes); err != nil {
		return nil, err
	}
	if err := collect(ctx, pool, "SELECT tracking_no FROM shop.shipments TABLESAMPLE SYSTEM (5) LIMIT 2000", &s.trackingNos); err != nil {
		return nil, err
	}
	if err := collect(ctx, pool, "SELECT id FROM shop.categories", &s.categoryIDs); err != nil {
		return nil, err
	}
	if err := pool.QueryRow(ctx, "SELECT min(occurred_at), max(occurred_at) FROM shop.events").Scan(&s.minTime, &s.maxTime); err != nil {
		return nil, err
	}
	for name, n := range map[string]int{"customers": len(s.customerIDs), "products": len(s.productIDs), "orders": len(s.orderIDs), "skus": len(s.skus), "event types": len(s.eventTypes), "tracking numbers": len(s.trackingNos), "categories": len(s.categoryIDs)} {
		if n == 0 {
			return nil, fmt.Errorf("no %s found — run the generator first", name)
		}
	}
	return s, nil
}

func collect[T any](ctx context.Context, pool *pgxpool.Pool, sql string, dst *[]T) error {
	rows, err := pool.Query(ctx, sql)
	if err != nil {
		return fmt.Errorf("%s: %w", sql, err)
	}
	defer rows.Close()
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			return err
		}
		*dst = append(*dst, v)
	}
	return rows.Err()
}

func (s *sample) args(r *rand.Rand, params []string) []any {
	out := make([]any, 0, len(params))
	for _, p := range params {
		switch p {
		case "customer_id":
			out = append(out, s.customerIDs[r.IntN(len(s.customerIDs))])
		case "product_id":
			out = append(out, s.productIDs[r.IntN(len(s.productIDs))])
		case "order_id":
			out = append(out, s.orderIDs[r.IntN(len(s.orderIDs))])
		case "sku":
			out = append(out, s.skus[r.IntN(len(s.skus))])
		case "event_type":
			out = append(out, s.eventTypes[r.IntN(len(s.eventTypes))])
		case "since":
			// somewhere in the last 30% of the time range
			span := s.maxTime.Sub(s.minTime)
			out = append(out, s.maxTime.Add(-time.Duration(r.Int64N(int64(span)*3/10))))
		case "tracking_no":
			out = append(out, s.trackingNos[r.IntN(len(s.trackingNos))])
		case "category_id":
			out = append(out, s.categoryIDs[r.IntN(len(s.categoryIDs))])
		default:
			log.Fatalf("unknown param sampler %q", p)
		}
	}
	return out
}
