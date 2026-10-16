package advisor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/indexbeacon/indexbeacon/internal/candidates"
	"github.com/indexbeacon/indexbeacon/internal/report"
	"github.com/indexbeacon/indexbeacon/internal/validator"
)

// Validate replaces heuristic estimates with the planner's own verdict.
//
// The heuristic scorer predicts what the planner will do. This asks it. A
// candidate the planner refuses to use is demoted regardless of how good its
// estimate looked, which is the single most useful correction available: a
// confident wrong recommendation costs disk space, write throughput, and the
// user's trust.
func Validate(ctx context.Context, q validator.Querier, generic bool,
	recs []report.Recommendation, budget validator.Budget) ([]report.Recommendation, []string, error) {

	if len(recs) == 0 {
		return recs, nil, nil
	}

	// Rebuild candidates from the recommendations so the validator can
	// construct the index statements it needs.
	cands := make([]*candidates.Candidate, 0, len(recs))
	for _, r := range recs {
		cands = append(cands, candidateFromRecommendation(r))
	}

	v := validator.New(q, generic, budget)
	results, err := v.ValidateAll(ctx, cands)
	if err != nil {
		// A validation failure must not lose the heuristic recommendations;
		// they are still useful, just unproven.
		return recs, []string{fmt.Sprintf(
			"validation could not complete (%v); showing heuristic estimates only", err)}, nil
	}

	var warnings []string
	out := make([]report.Recommendation, 0, len(recs))
	unused := 0

	for i, r := range recs {
		if i >= len(results) {
			out = append(out, r)
			continue
		}
		res := results[i]

		r.Validated = res.Validated
		if !res.Validated && res.Note != "" {
			// Not tested is not the same as tested and rejected; leave the
			// heuristic score and ranking alone.
			r.Notes = append(r.Notes, res.Note)
			out = append(out, r)
			continue
		}
		if res.Validated {
			used := res.Used
			r.PlanUsesIndex = &used
			r.ValidatedGain = res.Gain
			if res.SizeBytes > 0 {
				r.EstimatedBytes = res.SizeBytes
			}
			if used {
				// Rank by what the planner says the index saves.
				r.Score = res.Gain
				for _, qr := range res.Queries {
					if qr.Used {
						r.Notes = append(r.Notes, fmt.Sprintf(
							"planner confirms cost falls from %.0f to %.0f for a covered query",
							qr.CostBefore, qr.CostAfter))
						break
					}
				}
			} else {
				unused++
				r.Notes = append(r.Notes,
					"the planner did not choose this index when it was offered, so it would not be used as written")
			}
		}
		if res.Note != "" {
			r.Notes = append(r.Notes, res.Note)
		}
		for _, qr := range res.Queries {
			if qr.Skipped != "" {
				r.Notes = append(r.Notes, "one query could not be planned: "+qr.Skipped)
				break
			}
		}
		out = append(out, r)
	}

	// Recommendations the planner would actually use come first; the rest
	// are kept, clearly marked, because knowing an idea was rejected is
	// itself useful.
	sort.SliceStable(out, func(i, j int) bool {
		iUsed := out[i].PlanUsesIndex != nil && *out[i].PlanUsesIndex
		jUsed := out[j].PlanUsesIndex != nil && *out[j].PlanUsesIndex
		if iUsed != jUsed {
			return iUsed
		}
		return out[i].Score > out[j].Score
	})
	for i := range out {
		out[i].Rank = i + 1
	}

	if unused > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d candidate(s) were rejected by the planner and ranked last; they are shown so you can see what was considered",
			unused))
	}
	return out, warnings, nil
}

// candidateFromRecommendation reconstructs the candidate a recommendation was
// built from, which is what the validator needs to write index statements.
func candidateFromRecommendation(r report.Recommendation) *candidates.Candidate {
	cols := make([]candidates.Column, 0, len(r.Columns))
	for _, name := range r.Columns {
		col := candidates.Column{Name: name}
		if strings.HasSuffix(name, " DESC") {
			col.Name = strings.TrimSuffix(name, " DESC")
			col.Desc = true
		}
		cols = append(cols, col)
	}

	queries := make([]candidates.QueryRef, 0, len(r.Queries))
	for _, q := range r.Queries {
		queries = append(queries, candidates.QueryRef{
			Fingerprint: q.Fingerprint,
			SQL:         q.Query,
			Calls:       q.Calls,
			TotalTimeMS: q.TotalTimeMS,
		})
	}

	return &candidates.Candidate{
		Schema:  r.Schema,
		Table:   r.Table,
		Columns: cols,
		// The index type has to survive the round trip through the report,
		// or a GIN candidate would be validated as though it were a btree.
		Method:    candidates.AccessMethod(r.Method),
		Predicate: r.Predicate,
		Include:   r.Include,
		Queries:   queries,
	}
}
