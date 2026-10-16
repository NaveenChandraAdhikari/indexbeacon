package report

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// RenderMarkdown writes the report as a Markdown document.
func RenderMarkdown(w io.Writer, r *Report) error {
	fmt.Fprintf(w, "# idxa %s report\n\n", r.Kind)
	fmt.Fprintf(w, "Generated %s", r.GeneratedAt.Format(time.RFC3339))
	if r.Mode != "" {
		fmt.Fprintf(w, " · mode: %s", r.Mode)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)
	if len(r.Nodes) > 0 {
		fmt.Fprintln(w, "| Node | Role | Server | Stats window | Extensions |")
		fmt.Fprintln(w, "|---|---|---|---|---|")
		for _, n := range r.Nodes {
			win := "unknown"
			if n.StatsKnown {
				win = n.StatsWindow.Round(time.Minute).String()
			}
			var ext []string
			if n.HasPSS {
				ext = append(ext, "pg_stat_statements")
			}
			if n.HasHypoPG {
				ext = append(ext, "hypopg")
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n", n.Name, n.Role, mdEscape(shortVersion(n.Version)), win, strings.Join(ext, ", "))
		}
		fmt.Fprintln(w)
	}
	if len(r.Warnings) > 0 {
		for _, wm := range r.Warnings {
			fmt.Fprintf(w, "> ⚠️ %s\n", mdEscape(wm))
		}
		fmt.Fprintln(w)
	}

	if len(r.Recommendations) > 0 {
		fmt.Fprintf(w, "## Recommended indexes (%d)\n\n", len(r.Recommendations))
		fmt.Fprintln(w, "| # | Table | Index | Score | Benefit | Write penalty | Est. size | Confidence | Queries | Validated |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|")
		for _, rec := range r.Recommendations {
			validated := "estimate"
			if rec.Validated {
				validated = "yes"
				if rec.PlanUsesIndex != nil && !*rec.PlanUsesIndex {
					validated = "plan did not use it"
				}
			}
			fmt.Fprintf(w, "| %d | `%s.%s` | `%s` | %.1f | %.1f | %.1f | %s | %s | %d | %s |\n",
				rec.Rank, rec.Schema, rec.Table, indexSpec(rec), rec.Score, rec.Benefit, rec.WritePenalty,
				HumanBytes(rec.EstimatedBytes), rec.Confidence, len(rec.Queries), validated)
		}
		fmt.Fprintln(w)
		for _, rec := range r.Recommendations {
			fmt.Fprintf(w, "### %d. `%s.%s` %s\n\n", rec.Rank, rec.Schema, rec.Table, indexSpec(rec))
			fmt.Fprintf(w, "```sql\n%s\n```\n\n", rec.CreateSQL)
			if len(rec.Rules) > 0 {
				fmt.Fprintf(w, "Rules: %s\n\n", strings.Join(rec.Rules, ", "))
			}
			for _, n := range rec.Notes {
				fmt.Fprintf(w, "- %s\n", mdEscape(n))
			}
			if len(rec.Alternatives) > 0 {
				fmt.Fprintf(w, "- Alternatives considered: `%s`\n", strings.Join(rec.Alternatives, "`, `"))
			}
			if len(rec.Notes) > 0 || len(rec.Alternatives) > 0 {
				fmt.Fprintln(w)
			}
			if len(rec.Queries) > 0 {
				fmt.Fprintln(w, "| Calls | Total ms | Query |")
				fmt.Fprintln(w, "|---|---|---|")
				for i, q := range rec.Queries {
					if i >= 5 {
						fmt.Fprintf(w, "| … | … | %d more |\n", len(rec.Queries)-5)
						break
					}
					fmt.Fprintf(w, "| %d | %.0f | `%s` |\n", q.Calls, q.TotalTimeMS, mdCode(truncate(oneLine(q.Query), 160)))
				}
				fmt.Fprintln(w)
			}
		}
	}
	if len(r.HotTables) > 0 {
		fmt.Fprintln(w, "## Tables with sequential-scan pressure")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| Table | Rows | Seq scans | Seq tuples read | Idx scans | Seq % | Size | Indexes |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|")
		for _, h := range r.HotTables {
			fmt.Fprintf(w, "| `%s.%s` | %s | %s | %s | %s | %.0f%% | %s | %d |\n",
				h.Schema, h.Table, HumanCount(h.Rows), HumanCount(h.SeqScans), HumanCount(h.SeqTupRead), HumanCount(h.IdxScans), h.SeqRatio*100, HumanBytes(h.TableBytes), h.IndexCount)
		}
		fmt.Fprintln(w)
	}
	if r.Kind == KindUnused {
		fmt.Fprintf(w, "## Unused indexes (%d)\n\n", len(r.Unused))
		if len(r.Unused) > 0 {
			fmt.Fprintln(w, "| Index | Table | Size | Scans | Table writes | Confidence |")
			fmt.Fprintln(w, "|---|---|---|---|---|---|")
			for _, u := range r.Unused {
				fmt.Fprintf(w, "| `%s` | `%s.%s` | %s | %s | %s | %s |\n", u.Index, u.Schema, u.Table, HumanBytes(u.SizeBytes), scansString(u.Scans), HumanCount(u.TableWrite), u.Confidence)
			}
			fmt.Fprintln(w)
		}
	}
	if r.Kind == KindDuplicates {
		fmt.Fprintf(w, "## Redundant indexes (%d)\n\n", len(r.Duplicates))
		if len(r.Duplicates) > 0 {
			fmt.Fprintln(w, "| Redundant | Table | Kind | Size | Covered by | Reason |")
			fmt.Fprintln(w, "|---|---|---|---|---|---|")
			for _, d := range r.Duplicates {
				fmt.Fprintf(w, "| `%s` | `%s.%s` | %s | %s | `%s` | %s |\n", d.Redundant, d.Schema, d.Table, d.Kind, HumanBytes(d.RedundantSize), d.CoveredBy, mdEscape(d.Reason))
			}
			fmt.Fprintln(w)
		}
	}
	return nil
}

func shortVersion(v string) string {
	parts := strings.SplitN(v, " (", 2)
	return parts[0]
}

func mdEscape(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}

func mdCode(s string) string {
	return strings.NewReplacer("|", "\\|", "`", "'").Replace(s)
}
