package candidates

import "strings"

// ExistingIndex describes an index already present on the server, in the form
// the merge step needs to decide whether a candidate is redundant.
type ExistingIndex struct {
	// Schema and Table identify the relation.
	Schema string
	Table  string
	// Name is the index name, used when explaining why a candidate was
	// dropped.
	Name string
	// Columns are the key columns in order.
	Columns []string
	// Descending parallels Columns, recording each column's sort direction.
	Descending []bool
	// Predicate is the partial index predicate, empty for a full index.
	Predicate string
	// Unique marks a unique constraint or unique index. A unique index makes
	// any longer candidate sharing its leading columns pointless: at most one
	// row can match, so the extra columns cannot narrow anything further.
	Unique bool
	// Valid is false for an index left behind by a failed concurrent build,
	// which the planner will not use.
	Valid bool
	// Scans is how many times the index has been used, summed across nodes.
	Scans int64
}

func (e ExistingIndex) qualified() string {
	if e.Schema == "" {
		return e.Table
	}
	return e.Schema + "." + e.Table
}

// Merge combines candidates generated from many statements into one
// deduplicated, non-redundant set.
//
// Three things happen here, and each removes a class of bad recommendation:
//
//   - Identical candidates collapse, accumulating the statements they serve,
//     so a column filtered by twenty queries is recommended once with the
//     combined frequency rather than twenty times.
//   - A candidate whose columns are a leading prefix of another is absorbed
//     into the longer one. Recommending both (customer_id) and
//     (customer_id, created_at) is wasteful: the second serves every query
//     the first would.
//   - A candidate already covered by an existing index is dropped. If the
//     server is still scanning that table sequentially, that is reported as
//     a note, because the useful finding is that an index exists and is not
//     being used, not that one should be created.
func Merge(cands []*Candidate, existing []ExistingIndex) []*Candidate {
	// Collapse exact duplicates.
	byKey := map[string]*Candidate{}
	var order []string
	for _, c := range cands {
		key := c.Key()
		prev, ok := byKey[key]
		if !ok {
			clone := *c
			clone.Queries = append([]QueryRef{}, c.Queries...)
			byKey[key] = &clone
			order = append(order, key)
			continue
		}
		for _, q := range c.Queries {
			prev.addQuery(q)
		}
	}

	merged := make([]*Candidate, 0, len(order))
	for _, key := range order {
		merged = append(merged, byKey[key])
	}

	// Collapse candidates that differ only in the sort direction of trailing
	// columns. A btree can be scanned in either direction, so
	// (a, b) and (a, b DESC) are the same index for every query that does not
	// mix directions across columns.
	merged = collapseDirectionVariants(merged)

	// Absorb prefixes into longer candidates.
	absorbed := make([]bool, len(merged))
	for i, short := range merged {
		if absorbed[i] {
			continue
		}
		for j, long := range merged {
			if i == j || absorbed[j] {
				continue
			}
			if len(short.Columns) >= len(long.Columns) {
				continue
			}
			if !short.IsPrefixOf(long) {
				continue
			}
			for _, q := range short.Queries {
				long.addQuery(q)
			}
			long.Alternatives = appendUnique(long.Alternatives, short.shortDescription())
			absorbed[i] = true
			break
		}
	}

	var kept []*Candidate
	for i, c := range merged {
		if !absorbed[i] {
			kept = append(kept, c)
		}
	}

	// Drop candidates an existing index already covers.
	var out []*Candidate
	for _, c := range kept {
		if idx, covered := coveredBy(c, existing); covered {
			// The candidate is redundant, but the fact that the server still
			// wanted it is worth reporting when the index is going unused.
			if idx.Scans == 0 {
				c.Note = "an existing index " + idx.Name +
					" already covers these columns but has never been used;" +
					" check for a type mismatch, stale statistics, or a collation difference"
				out = append(out, c)
			}
			continue
		}
		out = append(out, c)
	}

	sortCandidates(out)
	return out
}

// collapseDirectionVariants merges candidates whose key columns match except
// for sort direction, keeping the one motivated by the most calls so the
// direction that queries actually ask for is the one recommended.
func collapseDirectionVariants(cands []*Candidate) []*Candidate {
	byShape := map[string]*Candidate{}
	var order []string

	for _, c := range cands {
		key := c.directionlessKey()
		prev, ok := byShape[key]
		if !ok {
			byShape[key] = c
			order = append(order, key)
			continue
		}
		// Keep whichever variant more calls asked for; merge the rest in.
		winner, loser := prev, c
		if c.Calls() > prev.Calls() {
			winner, loser = c, prev
			byShape[key] = c
		}
		for _, q := range loser.Queries {
			winner.addQuery(q)
		}
		if loser.descriptionDiffers(winner) {
			winner.Alternatives = appendUnique(winner.Alternatives, loser.shortDescription())
		}
	}

	out := make([]*Candidate, 0, len(order))
	for _, key := range order {
		out = append(out, byShape[key])
	}
	return out
}

// directionlessKey identifies a candidate ignoring sort direction.
func (c *Candidate) directionlessKey() string {
	var b strings.Builder
	b.WriteString(c.QualifiedTable())
	b.WriteString("(")
	for i, col := range c.Columns {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strings.ToLower(col.Name))
	}
	b.WriteString(")")
	if c.Predicate != "" {
		b.WriteString(" where ")
		b.WriteString(normalizePredicate(c.Predicate))
	}
	return b.String()
}

// descriptionDiffers reports whether two candidates render differently, so an
// identical variant is not listed as its own alternative.
func (c *Candidate) descriptionDiffers(other *Candidate) bool {
	return c.shortDescription() != other.shortDescription()
}

// coveredBy reports whether an existing valid index already serves a
// candidate, which is true when the candidate's columns are a leading prefix
// of the index's columns and the partial predicates agree.
func coveredBy(c *Candidate, existing []ExistingIndex) (ExistingIndex, bool) {
	for _, e := range existing {
		if !e.Valid || e.qualified() != c.QualifiedTable() {
			continue
		}
		if !predicatesAgree(c.Predicate, e.Predicate) {
			continue
		}
		if len(c.Columns) > len(e.Columns) {
			// A longer candidate is normally not covered, with one
			// exception: if the existing index is unique and its columns are
			// a leading prefix of the candidate, at most one row can match
			// already, so the extra columns buy nothing.
			if e.Unique && prefixMatches(e.Columns, c) {
				return e, true
			}
			continue
		}
		match := true
		for i, col := range c.Columns {
			if !strings.EqualFold(col.Name, e.Columns[i]) {
				match = false
				break
			}
			// A descending candidate column is still served by an ascending
			// index, because a btree can be scanned backwards; only a mixed
			// multi-column ordering genuinely needs a matching direction.
			if i < len(e.Descending) && len(c.Columns) > 1 && col.Desc != e.Descending[i] {
				match = false
				break
			}
		}
		if match {
			return e, true
		}
	}
	return ExistingIndex{}, false
}

// prefixMatches reports whether the existing index's columns are a leading
// prefix of the candidate's columns.
func prefixMatches(existing []string, c *Candidate) bool {
	if len(existing) == 0 || len(existing) > len(c.Columns) {
		return false
	}
	for i, name := range existing {
		if !strings.EqualFold(name, c.Columns[i].Name) {
			return false
		}
	}
	return true
}

// predicatesAgree compares partial index predicates. A full index covers a
// partial candidate, but a partial index does not cover a full candidate.
func predicatesAgree(candidate, existing string) bool {
	if existing == "" {
		return true
	}
	return normalizePredicate(candidate) == normalizePredicate(existing)
}

func normalizePredicate(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(s, `"`, ""))), " ")
}

// shortDescription renders a candidate compactly for the alternatives list.
func (c *Candidate) shortDescription() string {
	var b strings.Builder
	b.WriteString(c.QualifiedTable())
	b.WriteString("(")
	for i, col := range c.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(col.String())
	}
	b.WriteString(")")
	return b.String()
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}
