package validator

import (
	"context"
	"fmt"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/candidates"
)

// SQL used to drive hypopg. These are function calls and EXPLAIN only: no
// statement here changes anything on disk.
//
// Note that in hypopg 1.4 hypopg_list_indexes is a view, not a function, so
// it is queried without parentheses. Calling it as a function fails.
const (
	// indexrelid is an OID, which the binary protocol will not scan into a
	// 64-bit integer, so it is cast explicitly.
	sqlHypoCreate    = `SELECT indexrelid::bigint, indexname FROM hypopg_create_index($1)`
	sqlHypoReset     = `SELECT hypopg_reset()`
	sqlHypoSize      = `SELECT hypopg_relation_size($1::oid)`
	sqlHypoListCount = `SELECT count(*) FROM hypopg_list_indexes`
)

// Budget bounds the work validation is allowed to do. Validation issues an
// EXPLAIN per candidate per query, and planning is not free, so an unbounded
// run against a production server would be exactly the kind of load this tool
// exists to avoid.
type Budget struct {
	// MaxCandidates caps how many candidates are validated.
	MaxCandidates int
	// MaxQueriesPer caps how many of a candidate's queries are planned.
	MaxQueriesPer int
	// Deadline bounds the whole run. When it expires, whatever has been
	// validated so far is returned, with the rest marked unvalidated.
	Deadline time.Duration
}

func (b Budget) withDefaults() Budget {
	if b.MaxCandidates <= 0 {
		b.MaxCandidates = 20
	}
	if b.MaxQueriesPer <= 0 {
		b.MaxQueriesPer = 5
	}
	if b.Deadline <= 0 {
		b.Deadline = 60 * time.Second
	}
	return b
}

// QueryResult is the before and after picture for one statement.
type QueryResult struct {
	// SQL is the statement that was planned.
	SQL string `json:"sql"`
	// CostBefore and CostAfter are the planner's total cost estimates.
	CostBefore float64 `json:"cost_before"`
	CostAfter  float64 `json:"cost_after"`
	// Used reports whether the plan actually chose the hypothetical index.
	// A candidate the planner declines to use is worthless no matter how
	// good the heuristic score looked.
	Used bool `json:"used"`
	// Skipped explains why a statement could not be planned, rather than
	// silently omitting it.
	Skipped string `json:"skipped,omitempty"`
}

// Gain is the cost the index would save for this statement.
func (q QueryResult) Gain() float64 {
	if !q.Used {
		return 0
	}
	return q.CostBefore - q.CostAfter
}

// Result is the verdict on one candidate.
type Result struct {
	// Candidate is the proposal that was tested.
	Candidate *candidates.Candidate `json:"-"`
	// Validated is true when the planner was actually consulted. False means
	// the budget ran out or hypopg was unavailable, not that the index is
	// bad.
	Validated bool `json:"validated"`
	// Used is true when at least one query's plan chose the index.
	Used bool `json:"plan_uses_index"`
	// Queries holds the per-statement before and after costs.
	Queries []QueryResult `json:"queries"`
	// SizeBytes is hypopg's own estimate of the index size, which is more
	// trustworthy than the heuristic estimate.
	SizeBytes int64 `json:"estimated_bytes"`
	// Gain is the total cost saved, weighted by how often each statement
	// runs.
	Gain float64 `json:"gain"`
	// Note explains anything the caller should know, such as a skipped run.
	Note string `json:"note,omitempty"`
}

// Validator plans candidates against a live server using hypopg.
type Validator struct {
	q Querier
	// generic selects EXPLAIN GENERIC_PLAN, available from PostgreSQL 16.
	generic bool
	budget  Budget
}

// New returns a Validator. Set generic when the server is PostgreSQL 16 or
// newer, which allows planning statements that still contain parameter
// placeholders.
func New(q Querier, generic bool, budget Budget) *Validator {
	return &Validator{q: q, generic: generic, budget: budget.withDefaults()}
}

// ValidateAll tests candidates in order and returns a verdict for each.
//
// Selection is greedy: the best candidate is kept hypothetically present
// while the rest are re-tested, so a second candidate is only credited with
// the gain it adds on top of the first. Without this, two overlapping indexes
// each appear to solve the same problem and both get recommended.
func (v *Validator) ValidateAll(ctx context.Context, cands []*candidates.Candidate) ([]Result, error) {
	if len(cands) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, v.budget.Deadline)
	defer cancel()

	// Whatever happens, leave no hypothetical indexes behind.
	defer func() {
		resetCtx, resetCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer resetCancel()
		_ = v.q.Exec(resetCtx, sqlHypoReset)
	}()

	if err := v.q.Exec(ctx, sqlHypoReset); err != nil {
		return nil, fmt.Errorf("hypopg reset: %w", err)
	}

	results := make([]Result, len(cands))
	for i, c := range cands {
		results[i] = Result{Candidate: c}
	}

	remaining := make([]int, len(cands))
	for i := range cands {
		remaining[i] = i
	}

	accepted := 0
	for accepted < v.budget.MaxCandidates && len(remaining) > 0 {
		bestIdx := -1
		var best Result

		for _, idx := range remaining {
			if ctx.Err() != nil {
				break
			}
			r, err := v.validateOne(ctx, cands[idx])
			if err != nil {
				results[idx].Note = err.Error()
				continue
			}
			if bestIdx == -1 || r.Gain > best.Gain {
				bestIdx, best = idx, r
			}
		}

		if bestIdx == -1 || best.Gain <= 0 {
			// Nothing left improves on what is already accepted.
			for _, idx := range remaining {
				if results[idx].Note == "" && !results[idx].Validated {
					r, err := v.validateOne(ctx, cands[idx])
					if err == nil {
						results[idx] = r
						results[idx].Candidate = cands[idx]
					}
				}
			}
			break
		}

		best.Candidate = cands[bestIdx]
		results[bestIdx] = best
		accepted++

		// Keep the winner in place so later candidates are judged on the
		// gain they add beyond it.
		if _, _, err := v.createHypothetical(ctx, cands[bestIdx]); err != nil {
			return results, fmt.Errorf("keep accepted candidate: %w", err)
		}
		remaining = removeIndex(remaining, bestIdx)
	}

	if ctx.Err() != nil {
		for i := range results {
			if !results[i].Validated && results[i].Note == "" {
				results[i].Note = "validation budget exhausted before this candidate was tested"
			}
		}
	}
	return results, nil
}

// validateOne plans a candidate's queries with and without the index.
func (v *Validator) validateOne(ctx context.Context, c *candidates.Candidate) (Result, error) {
	res := Result{Candidate: c}

	// hypopg can only create btree, brin and hash indexes. Attempting a GIN
	// index fails outright, and reporting that failure as "the planner
	// declined to use it" would be a lie: the planner never saw it. Say so
	// instead and leave the heuristic estimate standing.
	if !c.MethodName().CanValidate() {
		res.Note = "hypopg cannot create a hypothetical " + string(c.MethodName()) +
			" index, so this recommendation could not be confirmed against the planner"
		return res, nil
	}

	queries := c.Queries
	if len(queries) > v.budget.MaxQueriesPer {
		// The most frequent statements matter most.
		queries = queries[:v.budget.MaxQueriesPer]
	}

	// Plan each statement as things stand.
	before := make([]Plan, len(queries))
	for i, q := range queries {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		p, err := Explain(ctx, v.q, q.SQL, v.generic)
		if err != nil {
			res.Queries = append(res.Queries, QueryResult{
				SQL: q.SQL, Skipped: skipReason(err),
			})
			continue
		}
		before[i] = p
	}

	// Create the hypothetical index.
	oid, name, err := v.createHypothetical(ctx, c)
	if err != nil {
		return res, err
	}
	defer func() {
		// Remove just this candidate again, keeping any accepted ones. The
		// simplest correct way is to reset and re-create the accepted set,
		// which the caller handles; here we only drop what we added by
		// resetting when nothing was accepted yet.
		_ = name
	}()

	if size, err := v.hypoSize(ctx, oid); err == nil {
		res.SizeBytes = size
	}

	// Re-plan with the index present.
	for i, q := range queries {
		if ctx.Err() != nil {
			break
		}
		if before[i].JSON == "" {
			continue // already recorded as skipped
		}
		after, err := Explain(ctx, v.q, q.SQL, v.generic)
		if err != nil {
			res.Queries = append(res.Queries, QueryResult{SQL: q.SQL, Skipped: skipReason(err)})
			continue
		}

		used := after.UsesHypotheticalIndex()
		qr := QueryResult{
			SQL:        q.SQL,
			CostBefore: before[i].TotalCost,
			CostAfter:  after.TotalCost,
			Used:       used,
		}
		res.Queries = append(res.Queries, qr)

		if used {
			res.Used = true
			// Weight the saving by how often the statement runs, so a small
			// gain on a very frequent query can outrank a large gain on a
			// rare one.
			calls := float64(q.Calls)
			if calls <= 0 {
				calls = 1
			}
			res.Gain += qr.Gain() * calls
		}
	}

	// A verdict needs at least one statement the planner actually saw with
	// the index in place. When every statement failed to plan there is no
	// verdict, and reporting "not used" would blame the index for a
	// statement the planner never considered.
	planned := 0
	for _, qr := range res.Queries {
		if qr.Skipped == "" {
			planned++
		}
	}
	if planned == 0 {
		res.Validated = false
		res.Note = "none of the covering statements could be planned, so this recommendation could not be confirmed against the planner"
	} else {
		res.Validated = true
	}

	// Drop this candidate's hypothetical index before returning, so the next
	// candidate is judged against the same baseline.
	if err := v.dropHypothetical(ctx, oid); err != nil {
		return res, err
	}
	return res, nil
}

// createHypothetical creates the index in the planner's imagination.
func (v *Validator) createHypothetical(ctx context.Context, c *candidates.Candidate) (oid int64, name string, err error) {
	row := v.q.QueryRow(ctx, sqlHypoCreate, c.HypoDDL())
	if err := row.Scan(&oid, &name); err != nil {
		return 0, "", fmt.Errorf("hypopg_create_index(%s): %w", c.HypoDDL(), err)
	}
	return oid, name, nil
}

// dropHypothetical removes one hypothetical index.
func (v *Validator) dropHypothetical(ctx context.Context, oid int64) error {
	if err := v.q.Exec(ctx, `SELECT hypopg_drop_index($1::oid)`, oid); err != nil {
		return fmt.Errorf("hypopg_drop_index: %w", err)
	}
	return nil
}

// hypoSize asks hypopg how large the index would be.
func (v *Validator) hypoSize(ctx context.Context, oid int64) (int64, error) {
	var size int64
	if err := v.q.QueryRow(ctx, sqlHypoSize, oid).Scan(&size); err != nil {
		return 0, err
	}
	return size, nil
}

// Reset removes every hypothetical index from the session.
func (v *Validator) Reset(ctx context.Context) error {
	return v.q.Exec(ctx, sqlHypoReset)
}

// OutstandingHypotheticals counts hypothetical indexes still present, used by
// tests to prove the session was left clean.
func (v *Validator) OutstandingHypotheticals(ctx context.Context) (int, error) {
	var n int
	if err := v.q.QueryRow(ctx, sqlHypoListCount).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// skipReason turns a planning failure into a short explanation. The common
// cause is a statement containing parameter placeholders on a server too old
// for GENERIC_PLAN.
func skipReason(err error) string {
	msg := err.Error()
	switch {
	case containsAny(msg, "could not determine data type", "there is no parameter"):
		return "statement is parameterised and this server cannot plan it without values (PostgreSQL 16 adds GENERIC_PLAN)"
	case containsAny(msg, "does not exist"):
		return "statement references an object that no longer exists"
	case containsAny(msg, "permission denied"):
		return "the connected role cannot plan this statement"
	default:
		return msg
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func removeIndex(list []int, v int) []int {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
