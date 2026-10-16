package advisor

import (
	"testing"

	"github.com/indexbeacon/indexbeacon/internal/parser"
)

func TestSchemaResolverPrefersUniqueThenPublic(t *testing.T) {
	m := mergedStats{tables: map[string]tableAgg{
		"public.orders":  {},
		"shop.events":    {},
		"archive.events": {},
		"zeta.users":     {},
		"alpha.users":    {},
	}}
	resolve := schemaResolver(m)
	cases := map[string]string{
		"orders":  "public",
		"events":  "archive", // ambiguous, no public: lowest name, stable
		"users":   "alpha",
		"missing": "",
	}
	for name, want := range cases {
		if got := resolve(name); got != want {
			t.Errorf("resolve(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestQualifyRewritesEveryTableReference(t *testing.T) {
	shape, err := parser.Parse("SELECT id FROM posts p JOIN shop.orders o ON o.post_id = p.id WHERE p.author_id = $1 ORDER BY p.published_at DESC")
	if err != nil {
		t.Fatal(err)
	}
	shape.Qualify(func(name string) string {
		if name == "posts" {
			return "public"
		}
		return ""
	})
	for _, tb := range shape.Tables {
		if tb.Name == "posts" && tb.Schema != "public" {
			t.Errorf("posts not qualified: %+v", tb)
		}
		if tb.Name == "orders" && tb.Schema != "shop" {
			t.Errorf("explicit schema must be kept: %+v", tb)
		}
	}
	for _, p := range shape.Predicates {
		if p.Table == "posts" {
			t.Errorf("predicate still on bare table: %+v", p)
		}
	}
	for _, o := range shape.OrderBy {
		if o.Table != "public.posts" {
			t.Errorf("order by not qualified: %+v", o)
		}
	}
	if len(shape.PredicatesFor("public.posts")) == 0 {
		t.Error("no predicates found under the qualified name")
	}
}
