// Package ident quotes SQL identifiers the way pg_get_indexdef does.
package ident

import (
	"strings"
	"unicode"
)

// Quote returns name quoted only when needed.
func Quote(name string) string {
	if isSafe(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Qualified returns schema.name with each part quoted as needed.
func Qualified(schema, name string) string {
	return Quote(schema) + "." + Quote(name)
}

func isSafe(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case unicode.IsUpper(r):
			return false
		default:
			return false
		}
	}
	return !reserved[s]
}

// A short list of words that would need quoting as identifiers.
var reserved = map[string]bool{
	"all": true, "and": true, "any": true, "as": true, "asc": true, "between": true, "case": true,
	"check": true, "column": true, "constraint": true, "create": true, "default": true, "desc": true,
	"distinct": true, "do": true, "else": true, "end": true, "except": true, "false": true, "for": true,
	"from": true, "grant": true, "group": true, "having": true, "in": true, "index": true, "into": true,
	"is": true, "join": true, "like": true, "limit": true, "not": true, "null": true, "offset": true,
	"on": true, "or": true, "order": true, "primary": true, "references": true, "select": true,
	"table": true, "then": true, "to": true, "true": true, "union": true, "unique": true, "user": true,
	"using": true, "when": true, "where": true, "with": true,
}
