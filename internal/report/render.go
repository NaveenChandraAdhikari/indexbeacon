package report

import (
	"fmt"
	"io"
)

// Format names an output renderer.
type Format string

const (
	FormatTable    Format = "table"
	FormatJSON     Format = "json"
	FormatMarkdown Format = "markdown"
	FormatSQL      Format = "sql"
)

// Render writes r to w in the requested format.
func Render(w io.Writer, r *Report, f Format) error {
	switch f {
	case FormatTable, "":
		return RenderTable(w, r)
	case FormatJSON:
		return RenderJSON(w, r)
	case FormatMarkdown:
		return RenderMarkdown(w, r)
	case FormatSQL:
		return RenderSQL(w, r)
	default:
		return fmt.Errorf("unknown format %q", f)
	}
}

// HumanBytes formats a byte count.
func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// HumanCount formats a large integer with k/M suffixes.
func HumanCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fG", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
