package scorer

import (
	"fmt"
	"math"
)

// formatf is a thin wrapper so explanation strings read consistently.
func formatf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// humanCount renders a number compactly: 1.2M rather than 1200000, since
// exact counts of estimates imply a precision that is not there.
func humanCount(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "unknown"
	}
	abs := math.Abs(v)
	switch {
	case abs >= 1e12:
		return trimZero(v/1e12) + "T"
	case abs >= 1e9:
		return trimZero(v/1e9) + "G"
	case abs >= 1e6:
		return trimZero(v/1e6) + "M"
	case abs >= 1e3:
		return trimZero(v/1e3) + "k"
	case abs >= 10:
		return fmt.Sprintf("%.0f", v)
	default:
		return trimZero(v)
	}
}

// trimZero renders one decimal place, dropping a trailing ".0".
func trimZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		return s[:len(s)-2]
	}
	return s
}

// HumanBytes renders a byte count using binary units, matching the way
// PostgreSQL's own pg_size_pretty reports sizes.
func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d bytes", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}
