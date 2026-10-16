package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// RenderTable writes a human-readable plain-text report.
func RenderTable(w io.Writer, r *Report) error {
	fmt.Fprintf(w, "idxa %s  (%s)\n", r.Kind, r.GeneratedAt.Format(time.RFC3339))
	for _, n := range r.Nodes {
		win := "unknown"
		if n.StatsKnown {
			win = n.StatsWindow.Round(time.Minute).String()
		}
		extras := []string{}
		if n.HasPSS {
			extras = append(extras, "pg_stat_statements")
		}
		if n.HasHypoPG {
			extras = append(extras, "hypopg")
		}
		fmt.Fprintf(w, "node %s  %s  %s  stats window %s", n.Name, n.Role, n.DSN, win)
		if len(extras) > 0 {
			fmt.Fprintf(w, "  [%s]", strings.Join(extras, ", "))
		}
		fmt.Fprintln(w)
	}
	for _, wmsg := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wmsg)
	}
	fmt.Fprintln(w)

	switch r.Kind {
	case KindUnused:
		renderUnusedTable(w, r)
	case KindDuplicates:
		renderDuplicatesTable(w, r)
	case KindAnalyze:
		renderAnalyzeTable(w, r)
	}
	return nil
}

func newTab(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
}

func renderUnusedTable(w io.Writer, r *Report) {
	if len(r.Unused) == 0 {
		fmt.Fprintln(w, "No unused indexes found.")
		return
	}
	fmt.Fprintf(w, "%d unused index(es):\n\n", len(r.Unused))
	tw := newTab(w)
	fmt.Fprintln(tw, "INDEX\tTABLE\tSIZE\tSCANS\tTABLE WRITES\tCONFIDENCE\tDEFINITION")
	for _, u := range r.Unused {
		fmt.Fprintf(tw, "%s\t%s.%s\t%s\t%s\t%s\t%s\t%s\n",
			u.Index, u.Schema, u.Table, HumanBytes(u.SizeBytes), scansString(u.Scans), HumanCount(u.TableWrite), u.Confidence, u.Definition)
	}
	tw.Flush()
}

func scansString(s []PerNodeCount) string {
	if len(s) <= 1 {
		if len(s) == 0 {
			return "0"
		}
		return fmt.Sprintf("%d", s[0].Count)
	}
	parts := make([]string, 0, len(s))
	for _, p := range s {
		parts = append(parts, fmt.Sprintf("%s=%d", p.Node, p.Count))
	}
	return strings.Join(parts, " ")
}

func renderDuplicatesTable(w io.Writer, r *Report) {
	if len(r.Duplicates) == 0 {
		fmt.Fprintln(w, "No duplicate or redundant indexes found.")
		return
	}
	fmt.Fprintf(w, "%d redundant index(es):\n\n", len(r.Duplicates))
	tw := newTab(w)
	fmt.Fprintln(tw, "REDUNDANT\tTABLE\tKIND\tSIZE\tSCANS\tCOVERED BY\tREASON")
	for _, d := range r.Duplicates {
		fmt.Fprintf(tw, "%s\t%s.%s\t%s\t%s\t%d\t%s\t%s\n",
			d.Redundant, d.Schema, d.Table, d.Kind, HumanBytes(d.RedundantSize), d.Scans, d.CoveredBy, d.Reason)
	}
	tw.Flush()
}

func renderAnalyzeTable(w io.Writer, r *Report) {
	if r.Mode != "" {
		fmt.Fprintf(w, "mode: %s\n\n", r.Mode)
	}
	if len(r.Recommendations) > 0 {
		fmt.Fprintf(w, "%d recommendation(s):\n\n", len(r.Recommendations))
		tw := newTab(w)
		fmt.Fprintln(tw, "#\tTABLE\tINDEX\tSCORE\tBENEFIT\tPENALTY\tEST SIZE\tCONF\tQUERIES\tVALIDATED")
		for _, rec := range r.Recommendations {
			validated := "estimate"
			if rec.Validated {
				validated = "yes"
				if rec.PlanUsesIndex != nil && !*rec.PlanUsesIndex {
					validated = "unused"
				}
			}
			fmt.Fprintf(tw, "%d\t%s.%s\t%s\t%.1f\t%.1f\t%.1f\t%s\t%s\t%d\t%s\n",
				rec.Rank, rec.Schema, rec.Table, indexSpec(rec), rec.Score, rec.Benefit, rec.WritePenalty,
				HumanBytes(rec.EstimatedBytes), rec.Confidence, len(rec.Queries), validated)
		}
		tw.Flush()
		fmt.Fprintln(w)
		for _, rec := range r.Recommendations {
			fmt.Fprintf(w, "[%d] %s\n", rec.Rank, rec.CreateSQL)
			for _, n := range rec.Notes {
				fmt.Fprintf(w, "    note: %s\n", n)
			}
			for i, q := range rec.Queries {
				if i >= 3 {
					fmt.Fprintf(w, "    ... and %d more\n", len(rec.Queries)-3)
					break
				}
				fmt.Fprintf(w, "    %s calls, %.0f ms: %s\n", HumanCount(q.Calls), q.TotalTimeMS, truncate(oneLine(q.Query), 110))
			}
		}
		fmt.Fprintln(w)
	} else if r.Mode != "table-only" {
		fmt.Fprintln(w, "No missing-index recommendations.")
	}
	if len(r.HotTables) > 0 {
		fmt.Fprintf(w, "Tables with the most sequential-scan pressure:\n\n")
		tw := newTab(w)
		fmt.Fprintln(tw, "TABLE\tROWS\tSEQ SCANS\tSEQ TUP READ\tIDX SCANS\tSEQ%\tSIZE\tINDEXES")
		for _, h := range r.HotTables {
			fmt.Fprintf(tw, "%s.%s\t%s\t%s\t%s\t%s\t%.0f%%\t%s\t%d\n",
				h.Schema, h.Table, HumanCount(h.Rows), HumanCount(h.SeqScans), HumanCount(h.SeqTupRead),
				HumanCount(h.IdxScans), h.SeqRatio*100, HumanBytes(h.TableBytes), h.IndexCount)
		}
		tw.Flush()
	}
}

func indexSpec(rec Recommendation) string {
	s := "(" + strings.Join(rec.Columns, ", ") + ")"
	if len(rec.Include) > 0 {
		s += " INCLUDE (" + strings.Join(rec.Include, ", ") + ")"
	}
	if rec.Predicate != "" {
		s += " WHERE " + rec.Predicate
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
