package config

import (
	"net/url"
	"regexp"
	"strings"
)

// redactedMark is what replaces a secret.
const redactedMark = "****"

// keywordPassword matches password=... in a keyword-value connection string,
// which is the other form PostgreSQL accepts besides a URL.
var keywordPassword = regexp.MustCompile(`(?i)\bpassword\s*=\s*('(?:[^']|'')*'|"[^"]*"|\S+)`)

// Redact removes the password from a connection string so it can be shown,
// logged or put in an error message.
//
// Every path that renders a connection string goes through here. A tool that
// stores database credentials will eventually print one somewhere: into a log
// line, an error banner, a crash report. Redacting at the single point where
// connection strings become text is the only way to be sure that never
// happens, so callers should never format a raw DSN themselves.
func Redact(dsn string) string {
	if dsn == "" {
		return ""
	}

	// URL form: postgres://user:password@host/db
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.Host != "" {
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				u.User = url.UserPassword(u.User.Username(), redactedMark)
			}
		}
		// A password can also appear in the query string.
		if q := u.Query(); q.Get("password") != "" {
			q.Set("password", redactedMark)
			u.RawQuery = q.Encode()
		}
		out := u.String()
		// url.String percent-encodes the placeholder; put it back so the
		// result stays readable.
		return strings.ReplaceAll(out, url.QueryEscape(redactedMark), redactedMark)
	}

	// Keyword form: host=... password=... dbname=...
	return keywordPassword.ReplaceAllString(dsn, "password="+redactedMark)
}

// Describe renders a connection string as a short label for the interface,
// such as "idxa@localhost:5499/shop", never including the password.
func Describe(dsn string) string {
	if dsn == "" {
		return ""
	}
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.Host != "" {
		var b strings.Builder
		if u.User != nil && u.User.Username() != "" {
			b.WriteString(u.User.Username())
			b.WriteString("@")
		}
		b.WriteString(u.Host)
		if db := strings.TrimPrefix(u.Path, "/"); db != "" {
			b.WriteString("/")
			b.WriteString(db)
		}
		return b.String()
	}

	// Keyword form: pick out the interesting fields.
	fields := map[string]string{}
	for _, part := range strings.Fields(dsn) {
		if k, v, ok := strings.Cut(part, "="); ok {
			fields[strings.ToLower(k)] = strings.Trim(v, `'"`)
		}
	}
	var b strings.Builder
	if u := fields["user"]; u != "" {
		b.WriteString(u)
		b.WriteString("@")
	}
	host := fields["host"]
	if host == "" {
		host = "localhost"
	}
	b.WriteString(host)
	if port := fields["port"]; port != "" {
		b.WriteString(":")
		b.WriteString(port)
	}
	if db := fields["dbname"]; db != "" {
		b.WriteString("/")
		b.WriteString(db)
	}
	return b.String()
}

// HasInlinePassword reports whether a connection string carries a password
// directly, rather than through an environment reference. The interface warns
// about this so a user can choose to move the secret out of the file.
func HasInlinePassword(dsn string) bool {
	if envRef.MatchString(dsn) {
		// The password comes from the environment, which is the safe form.
		if u, err := url.Parse(dsn); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && envRef.MatchString(pw) {
				return false
			}
		}
		if keywordPassword.MatchString(dsn) {
			match := keywordPassword.FindString(dsn)
			if envRef.MatchString(match) {
				return false
			}
		}
	}
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.User != nil {
		if _, ok := u.User.Password(); ok {
			return true
		}
	}
	return keywordPassword.MatchString(dsn)
}
