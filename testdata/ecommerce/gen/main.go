// Command gen loads synthetic e-commerce data into a database that has been
// prepared with testdata/ecommerce/schema.sql.
//
//	cd testdata/ecommerce && go run ./gen -dsn postgres://idxa:idxa@localhost:5499/idxa -scale 1
//
// Scale 1 ≈ 20k customers, 5k products, 100k orders (~250k order items),
// 1M events. Data is written with COPY and ANALYZE runs at the end.
//
// Distributions are deliberately skewed so the advisor has something to find:
//   - orders.customer_id and events.customer_id are Zipf-distributed
//   - orders.status is ~90% delivered / 5% shipped / 2% pending / 2% cancelled / 1% paid
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	baseCustomers = 20_000
	baseProducts  = 5_000
	baseOrders    = 100_000
	baseEvents    = 1_000_000
	numCategories = 50
	numWarehouses = 3
)

var (
	countries  = []string{"US", "GB", "DE", "IN", "BR", "FR", "CA", "AU", "JP", "NL"}
	tiers      = []string{"standard", "standard", "standard", "standard", "gold", "platinum"}
	eventTypes = []string{"page_view", "page_view", "page_view", "page_view", "search", "search", "add_to_cart", "login", "checkout"}
	carriers   = []string{"UPS", "DHL", "FedEx", "USPS"}
	providers  = []string{"card", "wallet", "bank"}
	warehouses = []string{"east", "west", "central"}
)

// pickStatus returns an order status with the intended skew.
func pickStatus(r *rand.Rand) string {
	switch p := r.Float64(); {
	case p < 0.90:
		return "delivered"
	case p < 0.95:
		return "shipped"
	case p < 0.97:
		return "pending"
	case p < 0.99:
		return "cancelled"
	default:
		return "paid"
	}
}

func main() {
	dsn := flag.String("dsn", os.Getenv("IDXA_DSN"), "PostgreSQL DSN (or IDXA_DSN)")
	scale := flag.Float64("scale", 1, "scale factor (1 ≈ 100k orders, 1M events)")
	seed := flag.Uint64("seed", 42, "random seed")
	schema := flag.String("schema", "shop", "schema created by schema.sql")
	flag.Parse()
	if *dsn == "" {
		log.Fatal("-dsn is required")
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET search_path = %s, public", pgx.Identifier{*schema}.Sanitize())); err != nil {
		log.Fatalf("set search_path: %v", err)
	}

	r := rand.New(rand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15))
	nCustomers := int(float64(baseCustomers) * *scale)
	nProducts := int(float64(baseProducts) * *scale)
	nOrders := int(float64(baseOrders) * *scale)
	nEvents := int(float64(baseEvents) * *scale)
	now := time.Now().UTC()
	start := now.Add(-365 * 24 * time.Hour)

	// Zipf over customers: a small set of customers generate most orders/events.
	custZipf := rand.NewZipf(r, 1.1, 1, uint64(nCustomers-1))
	prodZipf := rand.NewZipf(r, 1.05, 1, uint64(nProducts-1))
	pickCustomer := func() int64 { return int64(custZipf.Uint64()) + 1 }
	pickProduct := func() int64 { return int64(prodZipf.Uint64()) + 1 }
	randTime := func() time.Time {
		return start.Add(time.Duration(r.Int64N(int64(now.Sub(start)))))
	}

	load := func(table string, cols []string, n int, row func(i int) []any) {
		t0 := time.Now()
		i := 0
		src := pgx.CopyFromFunc(func() ([]any, error) {
			if i >= n {
				return nil, nil
			}
			i++
			return row(i), nil
		})
		copied, err := conn.CopyFrom(ctx, pgx.Identifier{table}, cols, src)
		if err != nil {
			log.Fatalf("COPY %s: %v", table, err)
		}
		log.Printf("%-12s %9d rows  %6.1fs", table, copied, time.Since(t0).Seconds())
	}

	// categories: 10 roots + 40 children
	load("categories", []string{"parent_id", "name", "slug"}, numCategories, func(i int) []any {
		var parent any
		if i > 10 {
			parent = int32(r.IntN(10) + 1)
		}
		return []any{parent, fmt.Sprintf("Category %d", i), fmt.Sprintf("category-%d", i)}
	})

	load("customers", []string{"email", "full_name", "country_code", "tier", "created_at"}, nCustomers, func(i int) []any {
		return []any{
			fmt.Sprintf("user%d@example.com", i),
			fmt.Sprintf("Customer %d", i),
			countries[r.IntN(len(countries))],
			tiers[r.IntN(len(tiers))],
			randTime(),
		}
	})

	// ~1.5 addresses per customer
	nAddr := nCustomers * 3 / 2
	load("addresses", []string{"customer_id", "kind", "line1", "city", "postal_code", "country_code", "is_default"}, nAddr, func(i int) []any {
		cid := int64((i-1)%nCustomers) + 1
		kind := "shipping"
		if i > nCustomers {
			kind = "billing"
		}
		return []any{cid, kind, fmt.Sprintf("%d Main St", r.IntN(9999)+1), fmt.Sprintf("City %d", r.IntN(500)),
			fmt.Sprintf("%05d", r.IntN(99999)), countries[r.IntN(len(countries))], i <= nCustomers}
	})

	// SKUs are non-unique on purpose (variants share a base SKU ~10% of the time).
	load("products", []string{"category_id", "sku", "name", "description", "price_cents", "active", "created_at"}, nProducts, func(i int) []any {
		skuN := i
		if r.Float64() < 0.10 && i > 1 {
			skuN = i - 1
		}
		return []any{int32(r.IntN(numCategories) + 1), fmt.Sprintf("SKU-%06d", skuN), fmt.Sprintf("Product %d", i),
			fmt.Sprintf("Description of product %d", i), int32(r.IntN(50_000) + 100), r.Float64() < 0.95, randTime()}
	})

	load("inventory", []string{"product_id", "warehouse", "quantity", "updated_at"}, nProducts*numWarehouses, func(i int) []any {
		return []any{int64((i-1)/numWarehouses) + 1, warehouses[(i-1)%numWarehouses], int32(r.IntN(1000)), randTime()}
	})

	// Orders. Keep status/created_at so items/payments/shipments can be
	// generated consistently without re-reading the table.
	type ord struct {
		cust   int64
		status string
		at     time.Time
		total  int32
	}
	orders := make([]ord, nOrders)
	load("orders", []string{"customer_id", "address_id", "status", "total_cents", "currency", "created_at", "updated_at"}, nOrders, func(i int) []any {
		o := ord{cust: pickCustomer(), status: pickStatus(r), at: randTime(), total: int32(r.IntN(100_000) + 500)}
		orders[i-1] = o
		var addr any = o.cust // address ids 1..nCustomers map 1:1 to customers (shipping)
		if r.Float64() < 0.05 {
			addr = nil
		}
		return []any{o.cust, addr, o.status, o.total, "USD", o.at, o.at.Add(time.Duration(r.IntN(72)) * time.Hour)}
	})

	// ~2.5 items per order
	nItems := nOrders * 5 / 2
	load("order_items", []string{"order_id", "product_id", "quantity", "unit_cents"}, nItems, func(i int) []any {
		return []any{int64(r.IntN(nOrders)) + 1, pickProduct(), int32(r.IntN(4) + 1), int32(r.IntN(20_000) + 100)}
	})

	// One payment per non-pending order.
	payIdx := 0
	var payOrders []int64
	for i, o := range orders {
		if o.status != "pending" {
			payOrders = append(payOrders, int64(i+1))
		}
	}
	load("payments", []string{"order_id", "provider", "provider_ref", "amount_cents", "status", "created_at"}, len(payOrders), func(i int) []any {
		oid := payOrders[payIdx]
		payIdx++
		o := orders[oid-1]
		st := "captured"
		switch o.status {
		case "cancelled":
			st = "refunded"
		case "paid":
			st = "authorized"
		}
		return []any{oid, providers[r.IntN(len(providers))], fmt.Sprintf("ref-%d-%d", oid, r.IntN(1_000_000)), o.total, st, o.at.Add(time.Minute)}
	})

	// One shipment per shipped/delivered order.
	shipIdx := 0
	var shipOrders []int64
	for i, o := range orders {
		if o.status == "shipped" || o.status == "delivered" {
			shipOrders = append(shipOrders, int64(i+1))
		}
	}
	load("shipments", []string{"order_id", "carrier", "tracking_no", "shipped_at", "delivered_at"}, len(shipOrders), func(i int) []any {
		oid := shipOrders[shipIdx]
		shipIdx++
		o := orders[oid-1]
		shipped := o.at.Add(24 * time.Hour)
		var delivered any
		if o.status == "delivered" {
			delivered = shipped.Add(time.Duration(r.IntN(120)+24) * time.Hour)
		}
		return []any{oid, carriers[r.IntN(len(carriers))], fmt.Sprintf("TRK%010d", oid), shipped, delivered}
	})

	load("events", []string{"customer_id", "event_type", "occurred_at", "product_id", "payload"}, nEvents, func(i int) []any {
		et := eventTypes[r.IntN(len(eventTypes))]
		var pid any
		if et != "login" && et != "search" {
			pid = pickProduct()
		}
		payload, _ := json.Marshal(map[string]any{"session": r.IntN(1_000_000), "ua": "idxa-gen"})
		return []any{pickCustomer(), et, randTime(), pid, string(payload)}
	})

	t0 := time.Now()
	for _, t := range []string{"customers", "addresses", "categories", "products", "inventory", "orders", "order_items", "payments", "shipments", "events"} {
		if _, err := conn.Exec(ctx, "ANALYZE "+pgx.Identifier{*schema, t}.Sanitize()); err != nil {
			log.Fatalf("ANALYZE %s: %v", t, err)
		}
	}
	log.Printf("ANALYZE done in %.1fs", time.Since(t0).Seconds())
}
