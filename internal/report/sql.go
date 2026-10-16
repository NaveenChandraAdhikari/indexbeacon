package report

import (
	"fmt"
	"io"
	"time"
)

// RenderSQL writes only the DDL a DBA would review and run. Recommendations
// are emitted as CREATE INDEX CONCURRENTLY; unused/duplicate drops are
// emitted commented-out so nothing destructive runs by accident.
func RenderSQL(w io.Writer, r *Report) error {
	fmt.Fprintf(w, "-- idxa %s  generated %s\n", r.Kind, r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintln(w, "-- idxa never executes DDL. Review before running.")
	for _, wm := range r.Warnings {
		fmt.Fprintf(w, "-- warning: %s\n", oneLine(wm))
	}
	fmt.Fprintln(w)
	for _, rec := range r.Recommendations {
		fmt.Fprintf(w, "-- [%d] score %.1f, benefit %.1f, write penalty %.1f, est. size %s, confidence %s\n",
			rec.Rank, rec.Score, rec.Benefit, rec.WritePenalty, HumanBytes(rec.EstimatedBytes), rec.Confidence)
		for _, q := range rec.Queries {
			if len(q.Query) > 0 {
				fmt.Fprintf(w, "--   %d calls: %s\n", q.Calls, truncate(oneLine(q.Query), 140))
			}
		}
		fmt.Fprintf(w, "%s\n\n", rec.CreateSQL)
	}
	for _, u := range r.Unused {
		fmt.Fprintf(w, "-- unused (%s, %s scans): %s\n", HumanBytes(u.SizeBytes), scansString(u.Scans), oneLine(u.Definition))
		fmt.Fprintf(w, "-- %s\n\n", u.DropSQL)
	}
	for _, d := range r.Duplicates {
		fmt.Fprintf(w, "-- %s duplicate of %s: %s\n", d.Kind, d.CoveredBy, oneLine(d.Reason))
		if d.DropSQL != "" {
			fmt.Fprintf(w, "-- %s\n\n", d.DropSQL)
		}
	}
	return nil
}
